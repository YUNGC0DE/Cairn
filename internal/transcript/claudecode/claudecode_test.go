package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YUNGC0DE/Cairn/internal/transcript"
)

// fixture builds a Claude Code transcript directory the way the real client
// lays one out, so Discover exercises the mangled-name prefilter as well.
func fixture(t *testing.T, repoRoot string, lines []string) *Parser {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, Slug(repoRoot))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var body string
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "sess-1234abcd.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Parser{Root: root}
}

func userLine(cwd, text, ts string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "user", "cwd": cwd, "sessionId": "sess-1234abcd", "timestamp": ts,
		"promptId": "p1", "gitBranch": "main",
		"message": map[string]any{"role": "user", "content": text},
	})
	return string(b)
}

func assistantLine(cwd, ts string, content []any) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant", "cwd": cwd, "sessionId": "sess-1234abcd", "timestamp": ts,
		"message": map[string]any{"role": "assistant", "model": "claude-opus-5", "content": content},
	})
	return string(b)
}

func TestDiscoverMatchesRepoByCWD(t *testing.T) {
	repo := "/tmp/proj.name"
	p := fixture(t, repo, []string{userLine(repo, "hello", "2026-08-01T10:00:00Z")})

	refs, err := p.Discover(repo, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("want 1 ref, got %d", len(refs))
	}
	if refs[0].CWD != repo || refs[0].Agent != Name {
		t.Fatalf("unexpected ref %+v", refs[0])
	}

	// A session from a different repository must not be picked up, even though
	// the mangled-name prefilter would fall back to scanning every directory.
	refs, err = p.Discover("/tmp/other", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("want no refs for a foreign repo, got %d", len(refs))
	}
}

func TestDiscoverFindsSubdirectorySessions(t *testing.T) {
	repo := "/tmp/proj"
	sub := repo + "/internal/auth"
	root := t.TempDir()
	dir := filepath.Join(root, Slug(sub))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := userLine(sub, "hi", "2026-08-01T10:00:00Z")
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refs, err := (&Parser{Root: root}).Discover(repo, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("a session started in a subdirectory belongs to the repo; got %d refs", len(refs))
	}
}

func TestLoadNormalisesMessages(t *testing.T) {
	repo := "/tmp/proj"
	lines := []string{
		userLine(repo, "add rate limiting", "2026-08-01T10:00:00Z"),
		assistantLine(repo, "2026-08-01T10:00:05Z", []any{
			map[string]any{"type": "thinking", "thinking": "redis would need a new datastore"},
			map[string]any{"type": "text", "text": "Using an in-memory token bucket."},
			map[string]any{"type": "tool_use", "name": "Edit",
				"input": map[string]any{"file_path": "internal/auth/limit.go"}},
		}),
		// A tool result: metadata, not authored content.
		func() string {
			b, _ := json.Marshal(map[string]any{
				"type": "user", "cwd": repo, "sessionId": "sess-1234abcd",
				"timestamp": "2026-08-01T10:00:06Z",
				"message": map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
				}},
				"toolUseResult": map[string]any{"ok": true},
			})
			return string(b)
		}(),
		// A subagent turn: excluded.
		func() string {
			b, _ := json.Marshal(map[string]any{
				"type": "assistant", "cwd": repo, "isSidechain": true,
				"timestamp": "2026-08-01T10:00:07Z",
				"message": map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "text", "text": "subagent noise"},
				}},
			})
			return string(b)
		}(),
	}
	p := fixture(t, repo, lines)
	refs, err := p.Discover(repo, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := p.Load(refs[0], transcript.Cursor{})
	if err != nil {
		t.Fatal(err)
	}

	if s.Model != "claude-opus-5" {
		t.Errorf("model = %q, want claude-opus-5", s.Model)
	}
	if s.Branch != "main" {
		t.Errorf("branch = %q, want main", s.Branch)
	}
	if got := s.LastHumanPrompt(); got != "add rate limiting" {
		t.Errorf("LastHumanPrompt = %q", got)
	}
	if files := s.TouchedFiles(); len(files) != 1 || files[0] != "internal/auth/limit.go" {
		t.Errorf("TouchedFiles = %v", files)
	}
	for _, m := range s.Messages {
		if m.Text == "subagent noise" {
			t.Error("sidechain messages must be excluded")
		}
	}
	var assistant transcript.Message
	for _, m := range s.Messages {
		if m.Role == transcript.RoleAssistant {
			assistant = m
		}
	}
	if assistant.Thinking == "" {
		t.Error("thinking must be kept: it is where rejected alternatives are argued")
	}
}

func TestLoadResumesFromCursor(t *testing.T) {
	repo := "/tmp/proj"
	first := userLine(repo, "first prompt", "2026-08-01T10:00:00Z")
	p := fixture(t, repo, []string{first})
	refs, _ := p.Discover(repo, time.Time{})

	s1, err := p.Load(refs[0], transcript.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s1.Messages) != 1 || !s1.Complete {
		t.Fatalf("first read: %d messages, complete=%v", len(s1.Messages), s1.Complete)
	}

	// Append, then read from the stored cursor: only the new line comes back.
	f, err := os.OpenFile(refs[0].Path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(userLine(repo, "second prompt", "2026-08-01T11:00:00Z") + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	s2, err := p.Load(refs[0], s1.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Messages) != 1 {
		t.Fatalf("want only the appended message, got %d", len(s2.Messages))
	}
	if got := s2.LastHumanPrompt(); got != "second prompt" {
		t.Errorf("got %q", got)
	}
	if s2.Complete {
		t.Error("a resumed read is not complete")
	}
}

func TestLoadRestartsWhenTranscriptShrinks(t *testing.T) {
	repo := "/tmp/proj"
	p := fixture(t, repo, []string{userLine(repo, "only prompt", "2026-08-01T10:00:00Z")})
	refs, _ := p.Discover(repo, time.Time{})

	// A cursor beyond EOF means the file was rotated or rewritten.
	s, err := p.Load(refs[0], transcript.Cursor{Bytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || !s.Complete {
		t.Fatalf("want a full re-read, got %d messages complete=%v", len(s.Messages), s.Complete)
	}
}

func TestLoadIgnoresPartialTrailingLine(t *testing.T) {
	repo := "/tmp/proj"
	p := fixture(t, repo, []string{userLine(repo, "done", "2026-08-01T10:00:00Z")})
	refs, _ := p.Discover(repo, time.Time{})

	// Simulate an agent mid-write: a line with no terminating newline.
	f, _ := os.OpenFile(refs[0].Path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"type":"user","cwd":"/tmp/proj","message":{"role":"user","content":"half`)
	f.Close()

	s, err := p.Load(refs[0], transcript.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(refs[0].Path)
	if s.Cursor.Bytes >= st.Size() {
		t.Errorf("cursor %d must stop before the incomplete line (size %d)", s.Cursor.Bytes, st.Size())
	}
}

func TestSlugMatchesClaudeCodeNaming(t *testing.T) {
	if got := Slug("/Users/me/Desktop/Cache"); got != "-Users-me-Desktop-Cache" {
		t.Errorf("Slug = %q", got)
	}
	if got := Slug("/tmp/a.b_c"); got != "-tmp-a-b-c" {
		t.Errorf("Slug = %q", got)
	}
}

// TestAgainstRealTranscripts runs the parser over the user's actual Claude Code
// transcripts. Vendor format drift is a named risk (spec §9) and a fixture cannot
// catch it, so this is the canary. Opt-in, because it needs data that is not in
// the repository:
//
//	CAIRN_TEST_REAL_CLAUDE=1 go test ./internal/transcript/claudecode/ -run Real -v
func TestAgainstRealTranscripts(t *testing.T) {
	if os.Getenv("CAIRN_TEST_REAL_CLAUDE") == "" {
		t.Skip("set CAIRN_TEST_REAL_CLAUDE=1 to check the parser against real transcripts")
	}
	p := New()
	dirs, err := os.ReadDir(p.Root)
	if err != nil {
		t.Skipf("no transcripts: %v", err)
	}
	checked, withPrompt, withTools := 0, 0, 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(p.Root, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(p.Root, d.Name(), f.Name())
			cwd, _ := peek(path)
			if cwd == "" {
				// Stub transcripts (a lone ai-title line, nothing else) legitimately
				// have no cwd; discovery falls back to the directory name and the
				// session is dropped later for having no content.
				continue
			}
			refs, err := p.Discover(cwd, time.Time{})
			if err != nil {
				t.Fatalf("discover %s: %v", cwd, err)
			}
			for _, ref := range refs {
				if ref.Path != path {
					continue
				}
				s, err := p.Load(ref, transcript.Cursor{})
				if err != nil {
					t.Errorf("load %s: %v", path, err)
					continue
				}
				checked++
				if s.LastHumanPrompt() != "" {
					withPrompt++
				}
				if len(s.TouchedFiles()) > 0 {
					withTools++
				}
				st, _ := os.Stat(path)
				if s.Cursor.Bytes > st.Size() {
					t.Errorf("%s: cursor %d past EOF %d", path, s.Cursor.Bytes, st.Size())
				}
				t.Logf("%-22s %4d messages, model=%-16s files=%d prompt=%q",
					shortName(f.Name()), len(s.Messages), s.Model, len(s.TouchedFiles()),
					transcript.Truncate(strings.ReplaceAll(s.LastHumanPrompt(), "\n", " "), 50))
			}
		}
	}
	if checked == 0 {
		t.Skip("no transcripts on this machine")
	}
	// A parser that finds sessions but never a prompt or a file is silently broken.
	if withPrompt == 0 {
		t.Errorf("parsed %d real sessions but found no human prompt in any of them", checked)
	}
	if withTools == 0 {
		t.Errorf("parsed %d real sessions but extracted no file paths", checked)
	}
	t.Logf("checked %d real transcripts (%d with a prompt, %d with file edits)", checked, withPrompt, withTools)
}

func shortName(s string) string {
	if len(s) > 22 {
		return s[:22]
	}
	return s
}
