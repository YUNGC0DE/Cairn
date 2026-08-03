package cursorcli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YUNGC0DE/git-cairn/internal/sqlitex"
	"github.com/YUNGC0DE/git-cairn/internal/transcript"
)

// store builds a Cursor chat directory matching the real on-disk layout: a
// meta.json, a prompt_history.json, and a store.db whose blobs table holds
// content-addressed messages plus a protobuf root listing them in order.
func store(t *testing.T, cwd string, messages []any) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "workspacehash", "11be0d94-347f-49d6-82a3-b448b9f01ef3")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(metaFile{
		SchemaVersion: 1, Title: "Cat Story", CWD: cwd,
		CreatedAtMs: time.Now().Add(-time.Hour).UnixMilli(),
		UpdatedAtMs: time.Now().UnixMilli(), HasConversation: true,
	})
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}
	prompts, _ := json.Marshal([]string{"add rate limiting\n"})
	if err := os.WriteFile(filepath.Join(dir, "prompt_history.json"), prompts, 0o644); err != nil {
		t.Fatal(err)
	}

	var sql strings.Builder
	sql.WriteString("CREATE TABLE blobs (id TEXT PRIMARY KEY, data BLOB);\n")
	sql.WriteString("CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);\n")

	// field 1, wire type 2, length 32, per message id — the shape the real root
	// blob uses.
	var rootBody []byte
	for _, m := range messages {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		id := hex.EncodeToString(sum[:])
		fmt.Fprintf(&sql, "INSERT INTO blobs VALUES ('%s', X'%s');\n", id, hex.EncodeToString(b))
		rootBody = append(rootBody, 0x0a, 0x20)
		rootBody = append(rootBody, sum[:]...)
	}
	// Trailing bookkeeping fields the parser must stop at rather than misread.
	rootBody = append(rootBody, 0x2a, 0x04, 0x08, 0x97, 0x78, 0x10)

	rootSum := sha256.Sum256(rootBody)
	rootID := hex.EncodeToString(rootSum[:])
	fmt.Fprintf(&sql, "INSERT INTO blobs VALUES ('%s', X'%s');\n", rootID, hex.EncodeToString(rootBody))

	sess, _ := json.Marshal(sessionMeta{
		AgentID: "11be0d94", LatestRootBlobID: rootID, Name: "Cat Story", LastUsedModel: "grok-4.5",
	})
	// Cursor stores this value hex-encoded inside a TEXT column.
	fmt.Fprintf(&sql, "INSERT INTO meta VALUES ('0', '%s');\n", hex.EncodeToString(sess))

	cmd := exec.Command("sqlite3", filepath.Join(dir, "store.db"))
	cmd.Stdin = strings.NewReader(sql.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlite3: %v: %s", err, out)
	}
	return root
}

func TestLoadParsesAISDKMessages(t *testing.T) {
	if !sqlitex.Available() {
		t.Skip("sqlite3 not installed")
	}
	cwd := t.TempDir()
	root := store(t, cwd, []any{
		map[string]any{"role": "system", "content": "You are an AI coding assistant."},
		map[string]any{"role": "user", "content": "<user_info>\nWorkspace Path: " + cwd + "\n</user_info>"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "<user_query>\nadd rate limiting\n</user_query>"},
		}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "reasoning", "text": "redis would add a datastore"},
			map[string]any{"type": "text", "text": "Using an in-memory bucket."},
			map[string]any{"type": "tool-call", "toolCallId": "c1", "toolName": "Write",
				"args": map[string]any{"path": "internal/auth/limit.go", "contents": "..."}},
		}},
		map[string]any{"role": "tool", "content": []any{
			map[string]any{"type": "tool-result", "toolCallId": "c1", "toolName": "Write"},
		}},
	})

	p := &Parser{Root: root}
	refs, err := p.Discover(cwd, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("want 1 ref, got %d", len(refs))
	}
	if refs[0].Agent != Name || refs[0].Title != "Cat Story" {
		t.Fatalf("unexpected ref %+v", refs[0])
	}

	s, err := p.Load(refs[0], transcript.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Degraded {
		t.Fatalf("store.db is readable, should not degrade: %s", s.DegradedReason)
	}
	if s.Model != "grok-4.5" {
		t.Errorf("model = %q", s.Model)
	}
	if got := s.LastHumanPrompt(); !strings.Contains(got, "add rate limiting") {
		t.Errorf("LastHumanPrompt = %q", got)
	}
	if files := s.TouchedFiles(); len(files) != 1 || files[0] != "internal/auth/limit.go" {
		t.Errorf("TouchedFiles = %v", files)
	}
	// The system prompt is enormous and never useful; it must be dropped.
	for _, m := range s.Messages {
		if strings.Contains(m.Text, "You are an AI coding assistant") {
			t.Error("the harness system prompt must not be kept")
		}
	}
	// Reasoning carries the rejected-alternative argument.
	var sawReasoning bool
	for _, m := range s.Messages {
		if strings.Contains(m.Thinking, "redis") {
			sawReasoning = true
		}
	}
	if !sawReasoning {
		t.Error("reasoning parts must be kept")
	}
	if s.Cursor.Count != 5 || s.Cursor.Token == "" {
		t.Errorf("cursor = %+v, want count 5 and a root token", s.Cursor)
	}
}

func TestLoadResumesFromMessageCount(t *testing.T) {
	if !sqlitex.Available() {
		t.Skip("sqlite3 not installed")
	}
	cwd := t.TempDir()
	root := store(t, cwd, []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "one"}}},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "two"}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "three"}}},
	})
	p := &Parser{Root: root}
	refs, _ := p.Discover(cwd, time.Time{})

	s, err := p.Load(refs[0], transcript.Cursor{Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].Text != "three" {
		t.Fatalf("want only the third message, got %+v", s.Messages)
	}

	// A cursor past the end means the session was rewound: re-read it all.
	s, err = p.Load(refs[0], transcript.Cursor{Count: 99})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 3 || !s.Complete {
		t.Fatalf("want a full re-read, got %d messages complete=%v", len(s.Messages), s.Complete)
	}
}

func TestLoadDegradesToPromptHistory(t *testing.T) {
	cwd := t.TempDir()
	root := store(t, cwd, []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "one"}}},
	})
	// Simulate an unreadable store: the documented degradation path keeps the
	// human prompts, which carry most of the intent signal.
	dir := filepath.Join(root, "workspacehash", "11be0d94-347f-49d6-82a3-b448b9f01ef3")
	if err := os.Remove(filepath.Join(dir, "store.db")); err != nil {
		t.Fatal(err)
	}

	p := &Parser{Root: root}
	refs, err := p.Discover(cwd, time.Time{})
	if err != nil || len(refs) != 1 {
		t.Fatalf("discover: %v (%d refs)", err, len(refs))
	}
	s, err := p.Load(refs[0], transcript.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Degraded || s.DegradedReason == "" {
		t.Error("a prompts-only read must be marked degraded, with a reason")
	}
	if got := s.LastHumanPrompt(); !strings.Contains(got, "add rate limiting") {
		t.Errorf("LastHumanPrompt = %q", got)
	}
}

func TestDiscoverSkipsForeignRepos(t *testing.T) {
	cwd := t.TempDir()
	root := store(t, cwd, []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "x"}}},
	})
	refs, err := (&Parser{Root: root}).Discover(t.TempDir(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("a session from another cwd must not match, got %d", len(refs))
	}
}

func TestDecodeChildListStopsAtOtherFields(t *testing.T) {
	var b []byte
	id := make([]byte, 32)
	for i := range id {
		id[i] = byte(i)
	}
	b = append(b, 0x0a, 0x20)
	b = append(b, id...)
	b = append(b, 0x2a, 0x02, 0x08, 0x01) // field 5: must terminate the scan
	b = append(b, 0x0a, 0x20)
	b = append(b, id...) // must not be reached
	if got := decodeChildList(b); len(got) != 1 {
		t.Fatalf("want 1 id, got %d", len(got))
	}
}

func TestSnapshotDoesNotTouchTheLiveStore(t *testing.T) {
	if !sqlitex.Available() {
		t.Skip("sqlite3 not installed")
	}
	cwd := t.TempDir()
	root := store(t, cwd, []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "x"}}},
	})
	db := filepath.Join(root, "workspacehash", "11be0d94-347f-49d6-82a3-b448b9f01ef3", "store.db")
	before, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	p := &Parser{Root: root}
	refs, _ := p.Discover(cwd, time.Time{})
	if _, err := p.Load(refs[0], transcript.Cursor{}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		t.Error("reading a Cursor session must never modify the user's live database")
	}
}

// TestAgainstRealStore runs the parser over the user's actual Cursor chats.
// Format drift at the vendor is a named risk, and a synthetic fixture
// cannot catch it — so this is the canary. It is opt-in because it depends on
// data that is not in the repository:
//
//	CAIRN_TEST_REAL_CURSOR=1 go test ./internal/transcript/cursorcli/ -run Real -v
func TestAgainstRealStore(t *testing.T) {
	if os.Getenv("CAIRN_TEST_REAL_CURSOR") == "" {
		t.Skip("set CAIRN_TEST_REAL_CURSOR=1 to check the parser against real Cursor data")
	}
	if !sqlitex.Available() {
		t.Skip("sqlite3 not installed")
	}
	p := New()
	spaces, err := os.ReadDir(p.Root)
	if err != nil {
		t.Skipf("no Cursor chats: %v", err)
	}
	checked := 0
	for _, space := range spaces {
		sessions, err := os.ReadDir(filepath.Join(p.Root, space.Name()))
		if err != nil {
			continue
		}
		for _, sd := range sessions {
			dir := filepath.Join(p.Root, space.Name(), sd.Name())
			var mf metaFile
			if err := readJSON(filepath.Join(dir, "meta.json"), &mf); err != nil || mf.CWD == "" {
				continue
			}
			refs, err := p.Discover(mf.CWD, time.Time{})
			if err != nil {
				t.Fatalf("discover %s: %v", dir, err)
			}
			for _, ref := range refs {
				s, err := p.Load(ref, transcript.Cursor{})
				if err != nil {
					t.Errorf("load %s: %v", ref.Path, err)
					continue
				}
				checked++
				t.Logf("%s: %d messages, model=%q degraded=%v prompt=%q",
					short(ref.ID), len(s.Messages), s.Model, s.Degraded,
					transcript.Truncate(strings.ReplaceAll(s.LastHumanPrompt(), "\n", " "), 60))
				if s.Degraded {
					t.Errorf("%s degraded unexpectedly: %s", ref.Path, s.DegradedReason)
				}
				if len(s.Messages) == 0 && mf.HasConversation {
					t.Errorf("%s: meta claims a conversation but no messages parsed", ref.Path)
				}
			}
		}
	}
	if checked == 0 {
		t.Skip("no Cursor sessions on this machine")
	}
	t.Logf("checked %d real sessions", checked)
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
