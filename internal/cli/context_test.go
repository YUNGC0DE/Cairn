package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/YUNGC0DE/git-cairn/internal/testutil"
)

// recordedCommit writes a commit carrying a full record, without going anywhere
// near an engine — the reactive channel reads what is already in history, so the
// distillation half is not under test here.
func recordedCommit(t *testing.T, r *testutil.Repo, path, content, subject string) {
	t.Helper()
	r.Write(path, content)
	r.Add(path)
	r.Commit(subject + `

Cache the rendered page so the list endpoint stops recomputing it.

Rejected: an in-process LRU cache — it goes stale across workers and the
staleness is invisible.
Invariant: the cache key must include the tenant id.
Open: eviction on tenant deletion is not wired up.
Next: wire eviction into the delete path.

Cairn-Agent: claude-code/opus
Cairn-Confidence: verified
Cairn-Files: ` + path)
}

func TestContextServesOncePerSessionPerFile(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")

	req := serveRequest{Path: "cache.go", Session: "s1", Hooked: true}

	first, err := serveContext(r.Repo, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "in-process LRU cache") {
		t.Fatalf("first touch did not recall the rejected alternative:\n%s", first)
	}
	if !strings.Contains(first, "tenant id") {
		t.Fatalf("first touch did not recall the invariant:\n%s", first)
	}

	// The rule: the same file in the same session says nothing more.
	second, err := serveContext(r.Repo, req)
	if err != nil {
		t.Fatal(err)
	}
	if second != "" {
		t.Fatalf("second touch of the same file should be silent, got:\n%s", second)
	}

	// A different session has not been told anything yet.
	other, err := serveContext(r.Repo, serveRequest{Path: "cache.go", Session: "s2", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	if other == "" {
		t.Fatal("a fresh session should be served")
	}
}

func TestContextResetAfterCompaction(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")
	req := serveRequest{Path: "cache.go", Session: "s1", Hooked: true}

	if _, err := serveContext(r.Repo, req); err != nil {
		t.Fatal(err)
	}
	if out, _ := serveContext(r.Repo, req); out != "" {
		t.Fatal("expected silence before the reset")
	}
	// Compaction drops the injected block out of the agent's context, so the
	// served set must not outlive it.
	if err := clearServed(r.Repo, "s1"); err != nil {
		t.Fatal(err)
	}
	out, err := serveContext(r.Repo, req)
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("after a compaction reset the file must be served again")
	}
}

// Under a budget the newest commits are the ones kept — the agent is about to
// edit the file as it stands.
func TestContextBudgetKeepsNewest(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")
	recordedCommit(t, r, "cache.go", "package cache // v2\n", "Key the cache by tenant")

	full, err := serveContext(r.Repo, serveRequest{Path: "cache.go", Session: "s0", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full, "Add the page cache") || !strings.Contains(full, "Key the cache by tenant") {
		t.Fatalf("both commits should be present unbudgeted:\n%s", full)
	}
	if strings.Index(full, "Add the page cache") > strings.Index(full, "Key the cache by tenant") {
		t.Fatalf("history must read oldest first:\n%s", full)
	}

	// Room for the newest commit but not for both.
	tight := len(full) - 20
	out, err := serveContext(r.Repo, serveRequest{
		Path: "cache.go", Session: "s1", Budget: tight, Hooked: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > tight {
		t.Fatalf("injection overran its budget: %d > %d bytes\n%s", len(out), tight, out)
	}
	if !strings.Contains(out, "Key the cache by tenant") {
		t.Fatalf("the newest commit should survive the squeeze:\n%s", out)
	}
	if strings.Contains(out, "Add the page cache") {
		t.Fatalf("the oldest commit should have been dropped first:\n%s", out)
	}
	if !strings.Contains(out, "1 commit earlier, not shown here") {
		t.Fatalf("what did not fit must be declared:\n%s", out)
	}
}

// The block has to say what it is. Handed over bare, a "Rejected:" line reads
// like a suggestion rather than a closed decision.
func TestContextExplainsItselfToTheAgent(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")

	out, err := serveContext(r.Repo, serveRequest{Path: "cache.go", Session: "s1", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"do not propose it again", // what rejected means
		"a rule this code must",   // what invariant means
		"can be out of date",      // that memory is not authority
		"the code is what is true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the instructions are missing %q:\n%s", want, out)
		}
	}
	// The explanation must come before what it explains.
	if strings.Index(out, "Each entry is one commit") > strings.Index(out, "Rejected:") {
		t.Fatalf("instructions must precede the history:\n%s", out)
	}
}

// A commit with no record carries nothing the diff does not already say.
func TestContextSkipsCommitsWithoutARecord(t *testing.T) {
	r := testutil.NewRepo(t)
	r.Write("plain.go", "package plain\n")
	r.Add("plain.go")
	r.Commit("Add a plain file with no cairn record")

	out, err := serveContext(r.Repo, serveRequest{Path: "plain.go", Session: "s1", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("an unrecorded commit must not be served:\n%s", out)
	}
}

func TestContextSilentOnUnknownFile(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")

	out, err := serveContext(r.Repo, serveRequest{Path: "untouched.go", Session: "s1", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("a file with no record must produce nothing, got:\n%s", out)
	}
}

func TestPreToolUseHookSpeaksClaudeCodeJSON(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")

	event := `{"session_id":"h1","cwd":"` + r.Root + `","hook_event_name":"PreToolUse",` +
		`"tool_name":"Edit","tool_input":{"file_path":"` + r.Root + `/cache.go"}}`
	var out bytes.Buffer
	env := &Env{Out: &out, Err: &bytes.Buffer{}, Dir: r.Root,
		Getenv: func(string) string { return "" }, Stdin: strings.NewReader(event)}

	if err := cmdHook(env, []string{"pre-tool-use"}); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("hook did not emit valid JSON (%v): %s", err, out.String())
	}
	if resp.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Fatalf("wrong event name: %q", resp.HookSpecificOutput.HookEventName)
	}
	if !strings.Contains(resp.HookSpecificOutput.AdditionalContext, "in-process LRU cache") {
		t.Fatalf("context did not reach the payload:\n%s", out.String())
	}
}

func TestPreToolUseHookIgnoresNonFileTools(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")

	event := `{"session_id":"h2","cwd":"` + r.Root + `","tool_name":"Bash","tool_input":{"command":"ls"}}`
	var out bytes.Buffer
	env := &Env{Out: &out, Err: &bytes.Buffer{}, Dir: r.Root,
		Getenv: func(string) string { return "" }, Stdin: strings.NewReader(event)}

	if err := cmdHook(env, []string{"pre-tool-use"}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("a Bash call must not trigger recall, got: %s", out.String())
	}
}

// A hook that breaks the agent it is meant to help gets uninstalled, so garbage
// on stdin has to be survivable.
func TestPreToolUseHookSurvivesGarbage(t *testing.T) {
	for _, in := range []string{"", "not json", `{"tool_name":"Edit"}`, `{"tool_input":{}}`} {
		var out bytes.Buffer
		env := &Env{Out: &out, Err: &bytes.Buffer{}, Dir: t.TempDir(),
			Getenv: func(string) string { return "" }, Stdin: strings.NewReader(in)}
		if err := cmdHook(env, []string{"pre-tool-use"}); err != nil {
			t.Fatalf("input %q returned an error: %v", in, err)
		}
	}
}

// The commit message travels verbatim — every line of it, trailers and
// co-authorship included. Deciding on the agent's behalf which parts of a commit
// message are worth reading is exactly the judgement this channel must not make.
func TestContextPassesCommitMessagesVerbatim(t *testing.T) {
	r := testutil.NewRepo(t)
	tail := "The endpoint had been recomputing the same page for every request, and " +
		"profiling put ninety percent of the latency in that render, which is why " +
		"the cache exists at all rather than a faster renderer."
	r.Write("cache.go", "package cache\n")
	r.Add("cache.go")
	r.Commit(`Add the page cache

Cache the rendered page so the list endpoint stops recomputing it.

Co-authored-by: Someone <a@b.c>

` + tail + `

Rejected: an in-process LRU cache — it goes stale across workers.

Cairn-Agent: claude-code/opus
Cairn-Confidence: verified
Cairn-Files: cache.go`)

	out, err := serveContext(r.Repo, serveRequest{Path: "cache.go", Session: "s1", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	// The reasoning travels whole, including prose that sits after a stray
	// co-authorship line.
	for _, want := range []string{
		"Add the page cache",
		"Cache the rendered page",
		"ninety percent of the latency",
		"rather than a faster renderer.",
		"Rejected: an in-process LRU cache",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("reasoning was lost — %q is missing from:\n%s", want, out)
		}
	}
	// The bookkeeping does not.
	for _, unwanted := range []string{
		"Co-authored-by", "a@b.c", "Cairn-Agent", "Cairn-Files", "Cairn-Confidence",
	} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("bookkeeping reached the agent — %q in:\n%s", unwanted, out)
		}
	}
}

// The record ends at the last invariant: what came after it is the state of the
// work at that moment and the machine addressing, neither of which a model
// reading for intent has any use for.
func TestContextCutsOpenNextAndTrailers(t *testing.T) {
	r := testutil.NewRepo(t)
	r.Write("cache.go", "package cache\n")
	r.Add("cache.go")
	r.Commit(`Add the page cache

Cache the rendered page so the list endpoint stops recomputing it.

Rejected: an in-process LRU cache — it goes stale across workers.
Invariant: the cache key must always include the tenant id.
Open: eviction on tenant deletion is not wired up.
Next: wire eviction into the delete path.

Cairn-Agent: claude-code/opus
Cairn-Session: 26369f10,4d857431
Cairn-Confidence: verified
Cairn-Files: cache.go
Cairn-Transcript: sha256:63e3e4a33ad29d88b7b278f4e5d4e8e5668f6f6cbe15dd245ddcd6b5be468f72`)

	out, err := serveContext(r.Repo, serveRequest{Path: "cache.go", Session: "s1", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "Invariant: the cache key must always include the tenant id.") {
		t.Fatalf("the message should end at the last invariant:\n%s", out)
	}
	for _, unwanted := range []string{"Open:", "Next:", "Cairn-", "sha256:"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("%q survived the cut:\n%s", unwanted, out)
		}
	}
}

// The per-file budget has to fit what the harness will actually inline.
//
// This test used to assert the opposite — a floor of 20 kB, locking in the
// decision that the budget should be generous because half a decision is worse
// than none. Measurement overruled it: Claude Code inlines a hook's
// additionalContext only to about 10 kB and spills the rest to a file with a 2 kB
// preview, so a 12 kB injection reached an agent as four commits it never saw. A
// budget the harness truncates is worse than a smaller one that arrives, because
// truncation upstream carries no warning while cairn's own says what it cut.
func TestContextDefaultBudgetFitsWhatTheHarnessInlines(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")
	out, err := serveContext(r.Repo, serveRequest{Path: "cache.go", Session: "s1", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Invariant: the cache key must include the tenant id.") {
		t.Fatalf("a single ordinary record should arrive whole:\n%s", out)
	}
	// Measured ceiling: 9255 bytes arrived inline, 10.3 kB did not. Stay clear of it.
	const harnessInlineLimit = 10000
	if defaultContextBudget >= harnessInlineLimit {
		t.Errorf("per-file budget %d will be truncated by the harness (inlines under %d)",
			defaultContextBudget, harnessInlineLimit)
	}
	// A floor too: shrink this far enough and a record arrives as a header and a
	// promise, which is the failure this whole budget exists to avoid.
	if defaultContextBudget < 4000 {
		t.Errorf("per-file budget %d is too small to carry one record's reasoning", defaultContextBudget)
	}
	if defaultSessionBudget < 10*defaultContextBudget {
		t.Errorf("session budget %d leaves room for fewer than ten files", defaultSessionBudget)
	}
}
