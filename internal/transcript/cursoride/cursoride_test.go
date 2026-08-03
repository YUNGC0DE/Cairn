package cursoride

import (
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

// conversation is one composer as the fixture builder takes it.
type conversation struct {
	id        string
	workspace string
	name      string
	updated   time.Time
	subagent  bool
	bubbles   []map[string]any
}

// profile builds a Cursor User directory matching the real on-disk layout: a
// workspaceStorage entry per window mapping to a folder, and one global
// state.vscdb holding every conversation keyed by composer and bubble id.
func profile(t *testing.T, folders map[string]string, convs []conversation, opts ...func(*fixture)) string {
	t.Helper()
	f := fixture{root: t.TempDir(), headers: true}
	for _, o := range opts {
		o(&f)
	}

	var sql strings.Builder
	sql.WriteString("CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB);\n")
	sql.WriteString("CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB);\n")
	if f.headers {
		sql.WriteString("CREATE TABLE composerHeaders (composerId TEXT PRIMARY KEY, workspaceId TEXT," +
			" createdAt INTEGER, lastUpdatedAt INTEGER, isArchived INTEGER, isSubagent INTEGER," +
			" recency INTEGER, checkpointAt INTEGER, value TEXT);\n")
	}

	for hash, folder := range folders {
		dir := filepath.Join(f.root, "workspaceStorage", hash)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		ws, _ := json.Marshal(map[string]string{"folder": folder})
		if err := os.WriteFile(filepath.Join(dir, "workspace.json"), ws, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range convs {
		var headers []bubbleHeader
		for _, b := range c.bubbles {
			id := b["bubbleId"].(string)
			headers = append(headers, bubbleHeader{BubbleID: id, Type: intOf(b["type"])})
			raw, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&sql, "INSERT INTO cursorDiskKV VALUES ('bubbleId:%s:%s', X'%s');\n",
				c.id, id, hex.EncodeToString(raw))
		}
		data, err := json.Marshal(map[string]any{
			"composerId":                  c.id,
			"name":                        c.name,
			"lastUpdatedAt":               c.updated.UnixMilli(),
			"fullConversationHeadersOnly": headers,
			"modelConfig":                 map[string]any{"modelName": "grok-4.5"},
		})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sql, "INSERT INTO cursorDiskKV VALUES ('composerData:%s', X'%s');\n",
			c.id, hex.EncodeToString(data))

		if f.headers {
			head, _ := json.Marshal(map[string]any{"type": "head", "composerId": c.id, "name": c.name})
			fmt.Fprintf(&sql, "INSERT INTO composerHeaders VALUES ('%s','%s',%d,%d,0,%d,%d,%d,'%s');\n",
				c.id, c.workspace, c.updated.UnixMilli(), c.updated.UnixMilli(),
				boolInt(c.subagent), c.updated.UnixMilli(), c.updated.UnixMilli(), head)
		} else {
			// Older builds record the conversations of a window in that window's own
			// store instead of a global index.
			var wsSQL strings.Builder
			wsSQL.WriteString("CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB);\n")
			ids, _ := json.Marshal(map[string]any{"selectedComposerIds": []string{c.id}})
			fmt.Fprintf(&wsSQL, "INSERT INTO ItemTable VALUES ('composer.composerData', X'%s');\n",
				hex.EncodeToString(ids))
			build(t, filepath.Join(f.root, "workspaceStorage", c.workspace, "state.vscdb"), wsSQL.String())
		}
	}

	global := filepath.Join(f.root, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(global), 0o755); err != nil {
		t.Fatal(err)
	}
	build(t, global, sql.String())
	return f.root
}

type fixture struct {
	root    string
	headers bool
}

// withoutComposerHeaders builds a store from an older Cursor, which has no
// global conversation index.
func withoutComposerHeaders(f *fixture) { f.headers = false }

func build(t *testing.T, path, sql string) {
	t.Helper()
	cmd := exec.Command("sqlite3", path)
	cmd.Stdin = strings.NewReader(sql)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlite3 %s: %v: %s", path, err, out)
	}
}

func intOf(v any) int {
	if f, ok := v.(int); ok {
		return f
	}
	return 0
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func userBubble(id, text string) map[string]any {
	return map[string]any{
		"bubbleId": id, "type": 1, "text": text,
		"createdAt": "2026-07-31T23:39:02.932Z",
		"modelInfo": map[string]any{"modelName": "grok-4.5"},
	}
}

const repoWorkspace = "b9aea98cec6419d11d073ee7373a6b5b"

func session(t *testing.T, cwd string) []conversation {
	t.Helper()
	return []conversation{{
		id: "26369f10-2678-4c3c-a14f-b032df1df70e", workspace: repoWorkspace,
		name: "Rate limiting", updated: time.Now(),
		bubbles: []map[string]any{
			userBubble("d1982d5f-7e7c-4a53-9e25-fe85cd457eeb", "add rate limiting"),
			{
				"bubbleId": "78c3feeb-c731-4168-866a-96e09600ca29", "type": 2,
				"thinking":  map[string]any{"text": "redis would add a datastore"},
				"createdAt": "2026-07-31T23:39:06.705Z",
			},
			{
				"bubbleId": "bb8e1a30-67ee-4b54-a5c0-9948e8b7d6e1", "type": 2,
				"text": "Using an in-memory bucket.", "createdAt": "2026-07-31T23:39:07.000Z",
			},
			{
				"bubbleId": "0d1bc3e9-1eca-481e-b1ce-ce1b768d4f33", "type": 2,
				"createdAt": "2026-07-31T23:39:08.000Z",
				"toolFormerData": map[string]any{
					"name": "edit_file_v2", "status": "completed",
					"params": `{"relativeWorkspacePath":"` + cwd + `/internal/auth/limit.go","noCodeblock":true}`,
				},
			},
			{
				"bubbleId": "a6104ee4-76ad-4ebc-84ac-5f0ad9197933", "type": 2,
				"createdAt": "2026-07-31T23:39:09.000Z",
				"toolFormerData": map[string]any{
					"name": "run_terminal_command_v2", "status": "cancelled",
					"params": `{"command":"redis-server --daemonize yes","cwd":"` + cwd + `"}`,
				},
			},
		},
	}}
}

func TestLoadParsesBubbles(t *testing.T) {
	requireSQLite(t)
	cwd := t.TempDir()
	root := profile(t, map[string]string{repoWorkspace: "file://" + cwd}, session(t, cwd))

	p := &Parser{Root: root}
	refs, err := p.Discover(cwd, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("want 1 ref, got %d", len(refs))
	}
	if refs[0].Agent != Name || refs[0].Title != "Rate limiting" || refs[0].CWD != cwd {
		t.Fatalf("unexpected ref %+v", refs[0])
	}

	s, err := p.Load(refs[0], transcript.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Model != "grok-4.5" {
		t.Errorf("model = %q", s.Model)
	}
	if got := s.LastHumanPrompt(); got != "add rate limiting" {
		t.Errorf("LastHumanPrompt = %q", got)
	}
	if files := s.TouchedFiles(); len(files) != 1 || !strings.HasSuffix(files[0], "internal/auth/limit.go") {
		t.Errorf("TouchedFiles = %v", files)
	}
	// Reasoning carries the rejected-alternative argument.
	var sawReasoning, sawInterrupted bool
	for _, m := range s.Messages {
		if strings.Contains(m.Thinking, "redis") {
			sawReasoning = true
		}
		for _, tc := range m.Tools {
			if tc.Error != "" && strings.Contains(tc.Error, "cancelled") {
				sawInterrupted = true
			}
			if tc.Name == "run_terminal_command_v2" && !strings.Contains(tc.Summary, "redis-server") {
				t.Errorf("terminal summary = %q", tc.Summary)
			}
		}
	}
	if !sawReasoning {
		t.Error("reasoning must be kept: it is where a rejected alternative is argued")
	}
	if !sawInterrupted {
		t.Error("an interrupted tool call must be recorded as such")
	}
	// Cursor IDE stamps every bubble, unlike its CLI.
	if s.Messages[0].Time.IsZero() {
		t.Error("bubble timestamps must be parsed")
	}
	if s.Cursor.Count != 5 || s.Cursor.Token == "" {
		t.Errorf("cursor = %+v, want count 5 and a bubble token", s.Cursor)
	}
	if !s.Complete || s.Degraded {
		t.Errorf("a full read of a healthy store: complete=%v degraded=%v", s.Complete, s.Degraded)
	}
}

func TestLoadResumesFromMessageCount(t *testing.T) {
	requireSQLite(t)
	cwd := t.TempDir()
	root := profile(t, map[string]string{repoWorkspace: "file://" + cwd}, session(t, cwd))
	p := &Parser{Root: root}
	refs, _ := p.Discover(cwd, time.Time{})

	s, err := p.Load(refs[0], transcript.Cursor{Count: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || len(s.Messages[0].Tools) != 1 {
		t.Fatalf("want only the last bubble, got %+v", s.Messages)
	}
	if s.Complete {
		t.Error("a resumed read is not complete")
	}

	// A cursor past the end means the conversation was rewound or branched.
	s, err = p.Load(refs[0], transcript.Cursor{Count: 99})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 5 || !s.Complete {
		t.Fatalf("want a full re-read, got %d messages complete=%v", len(s.Messages), s.Complete)
	}
}

func TestDiscoverIgnoresOtherReposAndRemoteWindows(t *testing.T) {
	requireSQLite(t)
	cwd := t.TempDir()
	convs := session(t, cwd)
	// A remote window whose *remote* path is spelled exactly like the local repo:
	// matching it would attribute another machine's work to this commit.
	root := profile(t, map[string]string{
		repoWorkspace:                      "file://" + t.TempDir(),
		"0d362963264296b954f00c6b292ebc14": "vscode-remote://ssh-remote%2Bhost" + cwd,
	}, convs)

	refs, err := (&Parser{Root: root}).Discover(cwd, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("no conversation belongs to this repo, got %d: %+v", len(refs), refs)
	}
}

func TestDiscoverSkipsSubagentConversations(t *testing.T) {
	requireSQLite(t)
	cwd := t.TempDir()
	convs := session(t, cwd)
	sub := convs[0]
	sub.id, sub.name, sub.subagent = "9f2a1c34-0000-4c3c-a14f-b032df1df70e", "Subagent", true
	root := profile(t, map[string]string{repoWorkspace: "file://" + cwd}, append(convs, sub))

	refs, err := (&Parser{Root: root}).Discover(cwd, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Title == "Subagent" {
		t.Fatalf("subagent conversations are noise for distillation, got %+v", refs)
	}
}

func TestDiscoverFallsBackToTheWorkspaceIndex(t *testing.T) {
	requireSQLite(t)
	cwd := t.TempDir()
	root := profile(t, map[string]string{repoWorkspace: "file://" + cwd},
		session(t, cwd), withoutComposerHeaders)

	p := &Parser{Root: root}
	refs, err := p.Discover(cwd, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Title != "Rate limiting" {
		t.Fatalf("an older store without the global index must still be read, got %+v", refs)
	}
	s, err := p.Load(refs[0], transcript.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 5 {
		t.Errorf("want 5 messages, got %d", len(s.Messages))
	}
}

func TestDiscoverFiltersBySince(t *testing.T) {
	requireSQLite(t)
	cwd := t.TempDir()
	convs := session(t, cwd)
	convs[0].updated = time.Now().Add(-2 * time.Hour)
	root := profile(t, map[string]string{repoWorkspace: "file://" + cwd}, convs)

	refs, err := (&Parser{Root: root}).Discover(cwd, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("a conversation older than the last commit cannot have produced it, got %d", len(refs))
	}
}

// TestPointerIdentifiesTheConversation guards the reason Pointer exists: the
// store holds every conversation, so hashing the file would change the pointer
// whenever anything else on the machine did.
func TestPointerIdentifiesTheConversation(t *testing.T) {
	requireSQLite(t)
	cwd := t.TempDir()
	convs := session(t, cwd)
	root := profile(t, map[string]string{repoWorkspace: "file://" + cwd}, convs)
	p := &Parser{Root: root}
	refs, _ := p.Discover(cwd, time.Time{})

	first := p.Pointer(refs[0])
	if !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("pointer = %q", first)
	}

	// Another conversation lands in the same store: this one's pointer must not
	// move.
	other := convs[0]
	other.id, other.name = "11111111-2678-4c3c-a14f-b032df1df70e", "Something else"
	build(t, filepath.Join(root, "globalStorage", "state.vscdb"), fmt.Sprintf(
		"INSERT INTO cursorDiskKV VALUES ('composerData:%s', X'%s');",
		other.id, hex.EncodeToString([]byte(`{"name":"Something else"}`))))
	if again := p.Pointer(refs[0]); again != first {
		t.Errorf("pointer moved because another conversation changed:\n%s\n%s", first, again)
	}
}

// TestReadingNeverWritesTheStore is the rule the whole package is built around:
// the database being read is the one Cursor is writing to right now.
func TestReadingNeverWritesTheStore(t *testing.T) {
	requireSQLite(t)
	cwd := t.TempDir()
	root := profile(t, map[string]string{repoWorkspace: "file://" + cwd}, session(t, cwd))
	db := filepath.Join(root, "globalStorage", "state.vscdb")
	before, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}

	p := &Parser{Root: root}
	refs, _ := p.Discover(cwd, time.Time{})
	if _, err := p.Load(refs[0], transcript.Cursor{}); err != nil {
		t.Fatal(err)
	}
	p.Pointer(refs[0])

	after, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		t.Error("reading a Cursor conversation must never modify the user's live database")
	}
}

func TestLocalPathRejectsRemoteURIs(t *testing.T) {
	cases := map[string]string{
		"file:///Users/you/repo":                       "/Users/you/repo",
		"file:///Users/you/Obsidian%20Vault/repo":      "/Users/you/Obsidian Vault/repo",
		"vscode-remote://ssh-remote%2Bhost/home/u/api": "",
		"file://otherhost/srv/api":                     "",
		"":                                             "",
	}
	for uri, want := range cases {
		if got := localPath(uri); got != want {
			t.Errorf("localPath(%q) = %q, want %q", uri, got, want)
		}
	}
}

// TestAgainstRealStore runs the parser over the user's actual Cursor IDE
// database. Format drift at the vendor is a named risk and a
// synthetic fixture cannot catch it, so this is the canary. It is opt-in
// because it depends on data that is not in the repository:
//
//	CAIRN_TEST_REAL_CURSOR_IDE=1 go test ./internal/transcript/cursoride/ -run Real -v
func TestAgainstRealStore(t *testing.T) {
	if os.Getenv("CAIRN_TEST_REAL_CURSOR_IDE") == "" {
		t.Skip("set CAIRN_TEST_REAL_CURSOR_IDE=1 to check the parser against real Cursor IDE data")
	}
	requireSQLite(t)
	p := New()
	db := p.GlobalDB()
	if _, err := os.Stat(db); err != nil {
		t.Skipf("no Cursor IDE store: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(p.Root, "workspaceStorage"))
	if err != nil {
		t.Skipf("no workspace storage: %v", err)
	}

	checked, empty := 0, 0
	start := time.Now()
	for _, e := range entries {
		var ws struct {
			Folder string `json:"folder"`
		}
		if err := readJSON(filepath.Join(p.Root, "workspaceStorage", e.Name(), "workspace.json"), &ws); err != nil {
			continue
		}
		folder := localPath(ws.Folder)
		if folder == "" {
			continue
		}
		refs, err := p.Discover(folder, time.Time{})
		if err != nil {
			t.Fatalf("discover %s: %v", folder, err)
		}
		for _, ref := range refs {
			s, err := p.Load(ref, transcript.Cursor{})
			if err != nil {
				t.Errorf("load %s: %v", short(ref.ID), err)
				continue
			}
			checked++
			if len(s.Messages) == 0 {
				empty++
				continue
			}
			if p.Pointer(ref) == "" {
				t.Errorf("%s: no transcript pointer", short(ref.ID))
			}
			t.Logf("%s %-28s %3d messages model=%-12s prompt=%q", short(ref.ID),
				transcript.Truncate(ref.Title, 28), len(s.Messages), s.Model,
				transcript.Truncate(collapse(s.LastHumanPrompt()), 60))
		}
	}
	if checked == 0 {
		t.Skip("no Cursor IDE conversations on this machine")
	}
	// Empty conversations are normal — a window opened and never used — but if
	// nearly everything is empty the format has moved under us.
	if empty*2 > checked {
		t.Errorf("%d of %d real conversations parsed to nothing: format drift", empty, checked)
	}
	t.Logf("checked %d real conversations in %s", checked, time.Since(start).Round(time.Millisecond))
}

func requireSQLite(t *testing.T) {
	t.Helper()
	if !sqlitex.Available() {
		t.Skip("sqlite3 not installed")
	}
}
