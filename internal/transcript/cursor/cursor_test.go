package cursor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YUNGC0DE/git-cairn/internal/transcript"
)

// writeSession lays out one transcript the way Cursor does:
// <root>/<project>/agent-transcripts/<id>/<id>.jsonl
func writeSession(t *testing.T, root, project, id, body string) string {
	t.Helper()
	dir := filepath.Join(root, project, transcriptsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const oneTurn = `{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Sat</timestamp>\n<user_query>\nadd rate limiting to /login\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"in-memory token bucket"},{"type":"tool_use","name":"StrReplace","input":{"path":"/repo/internal/auth/limit.go","old_string":"a","new_string":"b"}}]}}
{"type":"turn_ended","status":"success"}
`

func TestDiscoverFindsTheRepositorysProject(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	writeSession(t, root, slug(repo), "aaaa-1111", oneTurn)

	p := &Parser{Root: root}
	refs, err := p.Discover(repo, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %+v, want the one session", refs)
	}
	if refs[0].ID != "aaaa-1111" || refs[0].Agent != Name {
		t.Errorf("ref = %+v", refs[0])
	}
	if refs[0].CWD != repo {
		t.Errorf("cwd = %q, want %q", refs[0].CWD, repo)
	}
}

// A session started in a subdirectory belongs to the repository too, and its
// project name is the repo's with more segments on the end.
func TestDiscoverFindsSessionsStartedInASubdirectory(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "bench", "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSession(t, root, slug(filepath.Join(repo, "bench", "runs")), "bbbb-2222", oneTurn)

	refs, err := (&Parser{Root: root}).Discover(repo, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("a session from a subdirectory was lost: %+v", refs)
	}
	if want := filepath.Join(repo, "bench", "runs"); refs[0].CWD != want {
		t.Errorf("cwd = %q, want %q", refs[0].CWD, want)
	}
}

// '-' in a project name spells both '/' and a literal hyphen, so a sibling
// directory looks exactly like a subdirectory until the path is checked. Getting
// this wrong distils another project's sessions into this one's commits.
func TestDiscoverIgnoresASiblingWithTheSamePrefix(t *testing.T) {
	root := t.TempDir()
	parent := t.TempDir()
	repo := filepath.Join(parent, "cairn")
	sibling := filepath.Join(parent, "cairn-bench")
	for _, d := range []string{repo, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeSession(t, root, slug(sibling), "cccc-3333", oneTurn)

	refs, err := (&Parser{Root: root}).Discover(repo, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("a sibling repository's sessions were claimed: %+v", refs)
	}
}

func TestDiscoverHonoursSince(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	path := writeSession(t, root, slug(repo), "dddd-4444", oneTurn)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	refs, err := (&Parser{Root: root}).Discover(repo, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("a session older than the last commit was offered: %+v", refs)
	}
}

func TestLoadReadsTextAndToolCalls(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	writeSession(t, root, slug(repo), "eeee-5555", oneTurn)
	p := &Parser{Root: root}
	refs, err := p.Discover(repo, time.Time{})
	if err != nil || len(refs) != 1 {
		t.Fatalf("discover: %v %+v", err, refs)
	}
	s, err := p.Load(refs[0], transcript.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 {
		t.Fatalf("messages = %+v", s.Messages)
	}
	// The human's words arrive without Cursor's envelope: distillation weighs the
	// requests above everything else, so a prompt still wrapped in <user_query>
	// and a timestamp banner is a prompt the record explains badly.
	if got := s.Messages[0].Text; got != "add rate limiting to /login" {
		t.Errorf("user text = %q", got)
	}
	if s.Messages[1].Role != transcript.RoleAssistant {
		t.Errorf("role = %q", s.Messages[1].Role)
	}
	if n := len(s.Messages[1].Tools); n != 1 {
		t.Fatalf("tools = %+v", s.Messages[1].Tools)
	}
	// The staged-file match decides which sessions produced a commit, so a write
	// has to be recognisable as one.
	if got := s.Messages[1].Tools[0].Files; len(got) != 1 || got[0] != "/repo/internal/auth/limit.go" {
		t.Errorf("files = %v", got)
	}
	if !s.Complete || s.Cursor.Bytes != int64(len(oneTurn)) {
		t.Errorf("cursor = %+v, complete = %v", s.Cursor, s.Complete)
	}
}

// A turn Cursor injected itself carries no <user_query>, and reading it as the
// human's request would hand distillation someone else's words.
func TestLoadDropsInjectedUserTurns(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	writeSession(t, root, slug(repo), "ffff-6666",
		`{"role":"user","message":{"content":[{"type":"text","text":"<environment>cwd=/repo</environment>"}]}}`+"\n"+oneTurn)
	p := &Parser{Root: root}
	refs, _ := p.Discover(repo, time.Time{})
	s, err := p.Load(refs[0], transcript.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.LastHumanPrompt(); got != "add rate limiting to /login" {
		t.Errorf("last human prompt = %q", got)
	}
	for _, m := range s.Messages {
		if m.Role == transcript.RoleUser && m.Text == "" {
			t.Error("an empty injected turn was kept")
		}
	}
}

// A transcript being written this instant ends mid-line. Consuming it would
// advance the offset past a turn that was never read.
func TestLoadStopsShortOfAPartialLastLine(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	partial := oneTurn + `{"role":"assistant","message":{"content":[{"type":"text","tex`
	writeSession(t, root, slug(repo), "9999-7777", partial)
	p := &Parser{Root: root}
	refs, _ := p.Discover(repo, time.Time{})
	s, err := p.Load(refs[0], transcript.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Cursor.Bytes != int64(len(oneTurn)) {
		t.Errorf("cursor = %d, want the offset to stop at the last whole line (%d)",
			s.Cursor.Bytes, len(oneTurn))
	}

	// Resuming from there reads the rest once it is complete, and nothing twice.
	rest := `{"role":"assistant","message":{"content":[{"type":"text","text":"done"}]}}` + "\n"
	if err := os.WriteFile(refs[0].Path, []byte(oneTurn+rest), 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := p.Load(refs[0], s.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Messages) != 1 || again.Messages[0].Text != "done" {
		t.Errorf("resumed read = %+v", again.Messages)
	}
	if again.Complete {
		t.Error("a resumed slice is not the whole session")
	}
}
