package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YUNGC0DE/Cairn/internal/llm"
	"github.com/YUNGC0DE/Cairn/internal/record"
	"github.com/YUNGC0DE/Cairn/internal/testutil"
	"github.com/YUNGC0DE/Cairn/internal/transcript/claudecode"
)

// scripted is a stand-in for a headless agent.
type scripted struct {
	replies []string
	errs    []error
	calls   int
}

func (s *scripted) Name() string    { return "scripted" }
func (s *scripted) Available() bool { return true }
func (s *scripted) Path() string    { return "/scripted" }

func (s *scripted) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	if i >= len(s.replies) {
		return nil, errors.New("scripted: out of replies")
	}
	return &llm.Response{Text: s.replies[i], Engine: s.Name(), Model: "test-model"}, nil
}

const extractReply = `{
  "subject": "",
  "intent": "Rate limits on /login stop the credential stuffing seen in production logs.",
  "decision": "An in-memory token bucket is enough at current QPS.",
  "rejected": [{"option": "Redis-backed sliding window", "reason": "adds an external datastore ruled out in #412"}],
  "invariants": [{"text": "No new external datastores without an ADR", "scope": ["internal/**"]}],
  "claims": ["The limiter keeps state in memory"]
}`

const verifyReply = `{"claims":[{"index":0,"status":"supported","note":"limit.go"}]}`

// harness wires a repository, a fake transcript and a scripted engine together.
type harness struct {
	repo   *testutil.Repo
	env    *Env
	engine *scripted
	out    *bytes.Buffer
	errOut *bytes.Buffer
	envs   map[string]string
}

func newHarness(t *testing.T, replies ...string) *harness {
	t.Helper()
	repo := testutil.NewRepo(t)
	// Point the Claude Code parser at a transcript root we control.
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	// Keep the Cursor parser from finding the developer's real sessions.
	t.Setenv("HOME", t.TempDir())

	h := &harness{
		repo:   repo,
		engine: &scripted{replies: replies},
		out:    &bytes.Buffer{},
		errOut: &bytes.Buffer{},
		envs:   map[string]string{},
	}
	h.env = &Env{
		Out: h.out, Err: h.errOut, Dir: repo.Root, Engine: h.engine,
		Getenv: func(k string) string { return h.envs[k] },
	}
	return h
}

// writeTranscript lays down a Claude Code JSONL transcript for this repository.
func (h *harness) writeTranscript(t *testing.T, prompt string, tools ...string) string {
	t.Helper()
	root := claudecode.DefaultRoot()
	dir := filepath.Join(root, claudecode.Slug(h.repo.Root))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var content []any
	content = append(content,
		map[string]any{"type": "thinking", "thinking": "redis would add a datastore we do not want"},
		map[string]any{"type": "text", "text": "Using an in-memory token bucket instead."})
	for _, f := range tools {
		content = append(content, map[string]any{
			"type": "tool_use", "name": "Edit", "input": map[string]any{"file_path": f},
		})
	}
	lines := []map[string]any{
		{"type": "user", "cwd": h.repo.Root, "sessionId": "sess-abcd1234", "promptId": "p1",
			"gitBranch": "main", "timestamp": now.Format(time.RFC3339),
			"message": map[string]any{"role": "user", "content": prompt}},
		{"type": "assistant", "cwd": h.repo.Root, "sessionId": "sess-abcd1234",
			"timestamp": now.Add(time.Second).Format(time.RFC3339),
			"message":   map[string]any{"role": "assistant", "model": "claude-opus-5", "content": content}},
	}
	var body strings.Builder
	for _, l := range lines {
		b, _ := json.Marshal(l)
		body.Write(b)
		body.WriteByte('\n')
	}
	path := filepath.Join(dir, "sess-abcd1234.jsonl")
	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// neutralizeHooks replaces the installed hook scripts with no-ops that still
// carry cairn's marker.
//
// The installed script invokes os.Executable(), which under `go test` is the
// test binary — git running post-commit would then re-enter the whole suite.
// Keeping the marker preserves what the tests care about: that cairn sees its
// own post-commit hook as installed and therefore defers offset bookkeeping.
func (h *harness) neutralizeHooks(t *testing.T) {
	t.Helper()
	dir := filepath.Join(h.repo.GitDir, "hooks")
	for _, name := range hookNames {
		path := filepath.Join(dir, name)
		if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), marker) {
			continue
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+marker+"\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// commitViaHook writes a message file, runs the two hooks the way git would, and
// returns the resulting commit message.
func (h *harness) commitViaHook(t *testing.T, subject string) string {
	t.Helper()
	h.neutralizeHooks(t)
	msgFile := filepath.Join(h.repo.GitDir, "COMMIT_EDITMSG")
	if err := os.WriteFile(msgFile, []byte(subject+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdHook(h.env, []string{"prepare-commit-msg", msgFile, "message"}); err != nil {
		t.Fatalf("prepare-commit-msg: %v", err)
	}
	final, err := os.ReadFile(msgFile)
	if err != nil {
		t.Fatal(err)
	}
	h.repo.Git("commit", "--no-verify", "-F", msgFile)
	if err := cmdHook(h.env, []string{"post-commit"}); err != nil {
		t.Fatalf("post-commit: %v", err)
	}
	return string(final)
}

func TestHookWritesRecordIntoCommitMessage(t *testing.T) {
	h := newHarness(t, extractReply, verifyReply)
	h.repo.Write("README.md", "start\n")
	h.repo.Add(".")
	h.repo.Commit("Initial commit")

	h.writeTranscript(t, "add rate limiting to /login", "internal/auth/limit.go")
	h.repo.Write("internal/auth/limit.go", "package auth\n\n// token bucket\n")
	h.repo.Add(".")
	if err := cmdInit(h.env, nil); err != nil {
		t.Fatal(err)
	}

	msg := h.commitViaHook(t, "Add rate limiting to auth endpoints")

	if !strings.HasPrefix(msg, "Add rate limiting to auth endpoints") {
		t.Errorf("the author's subject must come first:\n%s", msg)
	}
	for _, want := range []string{
		"credential stuffing",
		"Rejected: Redis-backed sliding window",
		"Invariant: No new external datastores",
		record.TrailerAgent + ": claude-code/claude-opus-5",
		record.TrailerConfidence + ": verified",
		record.TrailerFiles + ": internal/auth/limit.go",
		record.TrailerTranscript + ": sha256:",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}

	// Offsets must advance only after the commit exists.
	off := filepath.Join(h.repo.CairnDir(), "offsets.json")
	b, err := os.ReadFile(off)
	if err != nil {
		t.Fatalf("offsets not written: %v", err)
	}
	if !strings.Contains(string(b), "claude-code:") {
		t.Errorf("offsets = %s", b)
	}
	if _, err := os.Stat(pendingPath(h.repo.Repo)); !os.IsNotExist(err) {
		t.Error("pending state must be cleared by post-commit")
	}
}

func TestHookIsIdempotentAcrossTwoCommits(t *testing.T) {
	h := newHarness(t, extractReply, verifyReply, extractReply, verifyReply)
	h.repo.Write("README.md", "start\n")
	h.repo.Add(".")
	h.repo.Commit("Initial commit")
	if err := cmdInit(h.env, nil); err != nil {
		t.Fatal(err)
	}

	h.writeTranscript(t, "add rate limiting", "internal/auth/limit.go")
	h.repo.Write("internal/auth/limit.go", "package auth\n")
	h.repo.Add(".")
	h.commitViaHook(t, "First agent commit")

	// A second commit with no new transcript content must not re-record: the
	// cursor already consumed the session.
	h.repo.Write("other.go", "package other\n")
	h.repo.Add(".")
	msg := h.commitViaHook(t, "Second commit, no new session")
	if strings.Contains(msg, record.TrailerAgent) {
		t.Errorf("a commit with no new transcript must be left alone:\n%s", msg)
	}
	if h.engine.calls != 2 {
		t.Errorf("engine called %d times, want 2 (one distillation)", h.engine.calls)
	}
}

func TestHookLeavesHumanCommitsAlone(t *testing.T) {
	h := newHarness(t)
	h.repo.Write("README.md", "start\n")
	h.repo.Add(".")
	h.repo.Commit("Initial commit")
	if err := cmdInit(h.env, nil); err != nil {
		t.Fatal(err)
	}

	// No transcript at all: a human typing at a keyboard.
	h.repo.Write("hand-written.go", "package main\n")
	h.repo.Add(".")
	msg := h.commitViaHook(t, "Fix typo")
	if strings.TrimSpace(msg) != "Fix typo" {
		t.Errorf("human commit was modified:\n%q", msg)
	}
	if h.engine.calls != 0 {
		t.Error("no engine call should happen without a transcript")
	}
}

func TestHookDegradesToTrailersWhenExtractionFails(t *testing.T) {
	h := newHarness(t, "the model refused to produce JSON")
	h.repo.Write("README.md", "start\n")
	h.repo.Add(".")
	h.repo.Commit("Initial commit")
	if err := cmdInit(h.env, nil); err != nil {
		t.Fatal(err)
	}

	h.writeTranscript(t, "add rate limiting", "internal/auth/limit.go")
	h.repo.Write("internal/auth/limit.go", "package auth\n")
	h.repo.Add(".")
	msg := h.commitViaHook(t, "Add rate limiting")

	// The prose is gone, but the pointer to the transcript is not: that is the
	// whole point of the degradation ladder (spec §3.2).
	if !strings.Contains(msg, record.TrailerConfidence+": metadata-only") {
		t.Errorf("want a metadata-only record:\n%s", msg)
	}
	if !strings.Contains(msg, record.TrailerTranscript+": sha256:") {
		t.Errorf("transcript pointer lost:\n%s", msg)
	}
	if strings.Contains(msg, "Rejected:") {
		t.Errorf("no prose should be invented:\n%s", msg)
	}
	// Nothing was distilled, so nothing may take credit for distilling it.
	if strings.Contains(msg, "distilled by") {
		t.Errorf("a metadata-only record must not name a distiller:\n%s", msg)
	}
}

// TestRunLogExplainsADegradedRecord covers the case the log exists for: the
// commit was made from an editor's UI, nobody saw the hook's stderr, and the
// record came out thin. The reason has to survive somewhere.
func TestRunLogExplainsADegradedRecord(t *testing.T) {
	h := newHarness(t, "the model refused to produce JSON")
	h.repo.Write("README.md", "start\n")
	h.repo.Add(".")
	h.repo.Commit("Initial commit")
	if err := cmdInit(h.env, nil); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(h.repo.GitDir, "cairn", runLogName)
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("nothing has run yet; there should be no log")
	}

	h.writeTranscript(t, "add rate limiting", "internal/auth/limit.go")
	h.repo.Write("internal/auth/limit.go", "package auth\n")
	h.repo.Add(".")
	h.commitViaHook(t, "Add rate limiting")

	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("no run log: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "metadata-only") {
		t.Errorf("the log must carry the outcome:\n%s", got)
	}
	if !strings.Contains(got, "unusable JSON") {
		t.Errorf("the log must carry the reason, not just the outcome:\n%s", got)
	}
	// The transcript itself must never be copied out of its own store — not into
	// git, and not into a log file either.
	if strings.Contains(got, "add rate limiting") {
		t.Errorf("the log must not quote the session:\n%s", got)
	}
}

func TestRunLogStaysBounded(t *testing.T) {
	repo := testutil.NewRepo(t)
	path := filepath.Join(repo.GitDir, "cairn", runLogName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("2026-08-01T00:00:00Z  recorded from claude-code [verified]\n", 8000)
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() < runLogMaxSize {
		t.Fatalf("fixture is too small to trigger trimming: %d", before.Size())
	}

	logRun(repo.Repo, "recorded from claude-code [verified]", nil)

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() >= before.Size() {
		t.Errorf("log grew from %d to %d instead of being trimmed", before.Size(), after.Size())
	}
	b, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(b), "[verified]\n") || !strings.HasPrefix(string(b), "2026-") {
		t.Error("trimming must cut whole lines and keep the newest ones")
	}
}

func TestHookSkipsMergeAndSquash(t *testing.T) {
	for _, source := range []string{"merge", "squash"} {
		h := newHarness(t, extractReply, verifyReply)
		h.repo.Write("README.md", "x\n")
		h.repo.Add(".")
		h.repo.Commit("Initial commit")
		h.writeTranscript(t, "irrelevant", "a.go")
		h.repo.Write("a.go", "package a\n")
		h.repo.Add(".")

		msgFile := filepath.Join(h.repo.GitDir, "COMMIT_EDITMSG")
		os.WriteFile(msgFile, []byte("Merge branch 'x'\n"), 0o644)
		if err := cmdHook(h.env, []string{"prepare-commit-msg", msgFile, source}); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(msgFile)
		if strings.Contains(string(b), "Cairn-") {
			t.Errorf("source=%s must be skipped: %s", source, b)
		}
	}
}

func TestHookSkipsWhenAlreadyRecorded(t *testing.T) {
	h := newHarness(t, extractReply, verifyReply)
	h.repo.Write("README.md", "x\n")
	h.repo.Add(".")
	h.repo.Commit("Initial commit")
	h.writeTranscript(t, "add limiting", "a.go")
	h.repo.Write("a.go", "package a\n")
	h.repo.Add(".")

	msgFile := filepath.Join(h.repo.GitDir, "COMMIT_EDITMSG")
	existing := "Add limiting\n\n" + record.TrailerAgent + ": claude-code/x\n"
	os.WriteFile(msgFile, []byte(existing), 0o644)
	if err := cmdHook(h.env, []string{"prepare-commit-msg", msgFile, "commit"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(msgFile)
	if string(b) != existing {
		t.Errorf("an amend of an already-recorded commit must be untouched:\n%s", b)
	}
	if h.engine.calls != 0 {
		t.Error("no engine call on an already-recorded message")
	}
}

func TestHookRespectsSkipAndDisable(t *testing.T) {
	for _, key := range []string{"CAIRN_SKIP", "CAIRN_ENABLED"} {
		h := newHarness(t, extractReply, verifyReply)
		if key == "CAIRN_SKIP" {
			h.envs[key] = "1"
		} else {
			h.envs[key] = "false"
		}
		h.repo.Write("README.md", "x\n")
		h.repo.Add(".")
		h.repo.Commit("Initial commit")
		h.writeTranscript(t, "add limiting", "a.go")
		h.repo.Write("a.go", "package a\n")
		h.repo.Add(".")

		msgFile := filepath.Join(h.repo.GitDir, "COMMIT_EDITMSG")
		os.WriteFile(msgFile, []byte("Subject\n"), 0o644)
		if err := cmdHook(h.env, []string{"prepare-commit-msg", msgFile, "message"}); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(msgFile)
		if strings.Contains(string(b), "Cairn-") {
			t.Errorf("%s must switch cairn off: %s", key, b)
		}
	}
}

func TestHookPreservesGitCommentBlock(t *testing.T) {
	h := newHarness(t, extractReply, verifyReply)
	h.repo.Write("README.md", "x\n")
	h.repo.Add(".")
	h.repo.Commit("Initial commit")
	if err := cmdInit(h.env, nil); err != nil {
		t.Fatal(err)
	}
	h.writeTranscript(t, "add limiting", "a.go")
	h.repo.Write("a.go", "package a\n")
	h.repo.Add(".")

	msgFile := filepath.Join(h.repo.GitDir, "COMMIT_EDITMSG")
	tail := "# Please enter the commit message for your changes.\n# On branch main\n"
	os.WriteFile(msgFile, []byte("Add limiting\n\n"+tail), 0o644)
	if err := cmdHook(h.env, []string{"prepare-commit-msg", msgFile, "message"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(msgFile)
	if !strings.Contains(string(got), tail) {
		t.Errorf("git's comment block must survive:\n%s", got)
	}
	// The record must be inserted before the comments, or git strips it.
	if idx := strings.Index(string(got), record.TrailerAgent); idx < 0 || idx > strings.Index(string(got), "# Please") {
		t.Errorf("record must sit above the comment block:\n%s", got)
	}
}

func TestNotesModeKeepsMessageCleanAndAttachesNote(t *testing.T) {
	h := newHarness(t, extractReply, verifyReply)
	h.repo.Write("README.md", "x\n")
	h.repo.Add(".")
	h.repo.Commit("Initial commit")
	if err := cmdInit(h.env, []string{"--mode", "notes"}); err != nil {
		t.Fatal(err)
	}
	h.writeTranscript(t, "add rate limiting", "internal/auth/limit.go")
	h.repo.Write("internal/auth/limit.go", "package auth\n")
	h.repo.Add(".")

	msg := h.commitViaHook(t, "Add rate limiting")
	if strings.Contains(msg, "Cairn-") {
		t.Errorf("notes mode must leave the commit message clean:\n%s", msg)
	}
	note := h.repo.Note(h.repo.HeadSHA())
	if note == "" {
		t.Fatal("no note attached")
	}
	for _, want := range []string{"Rejected: Redis-backed sliding window", record.TrailerConfidence + ": verified"} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q:\n%s", want, note)
		}
	}

	// A reader must not need to know which mode the repo uses.
	out := &bytes.Buffer{}
	h.env.Out = out
	if err := cmdShow(h.env, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Redis-backed sliding window") {
		t.Errorf("cairn show must read notes-mode records too:\n%s", out)
	}
}

func TestInitRefusesToClobberAForeignHook(t *testing.T) {
	h := newHarness(t)
	hooks := filepath.Join(h.repo.GitDir, "hooks")
	os.MkdirAll(hooks, 0o755)
	foreign := "#!/bin/sh\necho someone else's hook\n"
	path := filepath.Join(hooks, "prepare-commit-msg")
	if err := os.WriteFile(path, []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cmdInit(h.env, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != foreign {
		t.Error("init must not overwrite a hook it does not own")
	}
	if !strings.Contains(h.out.String(), "--force") {
		t.Errorf("the user must be told how to proceed:\n%s", h.out)
	}

	// With --force the original is backed up, not lost.
	if err := cmdInit(h.env, []string{"--force"}); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".cairn-backup")
	if err != nil || string(backup) != foreign {
		t.Errorf("backup missing or wrong: %v", err)
	}
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), marker) {
		t.Error("--force should install cairn's hook")
	}
}

func TestWhyAndRejectedReadBackTheRecord(t *testing.T) {
	h := newHarness(t, extractReply, verifyReply)
	h.repo.Write("README.md", "x\n")
	h.repo.Add(".")
	h.repo.Commit("Initial commit")
	if err := cmdInit(h.env, nil); err != nil {
		t.Fatal(err)
	}
	h.writeTranscript(t, "add rate limiting", "internal/auth/limit.go")
	h.repo.Write("internal/auth/limit.go", "package auth\n")
	h.repo.Add(".")
	h.commitViaHook(t, "Add rate limiting")

	out := &bytes.Buffer{}
	h.env.Out = out
	if err := cmdWhy(h.env, []string{"internal/auth/limit.go"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "credential stuffing") {
		t.Errorf("cairn why lost the intent:\n%s", out)
	}

	out.Reset()
	if err := cmdRejected(h.env, []string{"redis"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(out.String()), "redis-backed sliding window") {
		t.Errorf("cairn rejected did not find the negative:\n%s", out)
	}

	out.Reset()
	if err := cmdRejected(h.env, []string{"kafka"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(out.String()), "redis") {
		t.Errorf("query must actually filter:\n%s", out)
	}
}

func TestStalePendingIsNotAppliedToAnotherCommit(t *testing.T) {
	h := newHarness(t, extractReply, verifyReply)
	h.repo.Write("README.md", "x\n")
	h.repo.Add(".")
	h.repo.Commit("Initial commit")
	if err := cmdInit(h.env, []string{"--mode", "notes"}); err != nil {
		t.Fatal(err)
	}
	h.neutralizeHooks(t)
	h.writeTranscript(t, "add rate limiting", "internal/auth/limit.go")
	h.repo.Write("internal/auth/limit.go", "package auth\n")
	h.repo.Add(".")

	// Prepare a commit, then abandon it — as an author quitting the editor does.
	msgFile := filepath.Join(h.repo.GitDir, "COMMIT_EDITMSG")
	os.WriteFile(msgFile, []byte("Add rate limiting\n"), 0o644)
	if err := cmdHook(h.env, []string{"prepare-commit-msg", msgFile, "message"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pendingPath(h.repo.Repo)); err != nil {
		t.Fatalf("pending state should exist: %v", err)
	}

	// An unrelated commit lands next. The stale pending must not attach its note
	// here, and must not consume the transcript.
	h.repo.Write("unrelated.md", "hi\n")
	h.repo.Add(".")
	h.repo.Commit("Something else entirely")
	if err := cmdHook(h.env, []string{"post-commit"}); err != nil {
		t.Fatal(err)
	}
	if note := h.repo.Note(h.repo.HeadSHA()); note != "" {
		t.Errorf("a note was attached to the wrong commit:\n%s", note)
	}
	if b, err := os.ReadFile(filepath.Join(h.repo.CairnDir(), "offsets.json")); err == nil {
		if strings.Contains(string(b), "claude-code:") {
			t.Error("the transcript must stay unconsumed so the next commit records it")
		}
	}
}
