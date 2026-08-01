package sqlitex

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// db builds a small database inside a directory whose name has a space in it —
// the real Cursor store lives under "Application Support", and a path that is
// not escaped never opens.
func db(t *testing.T) string {
	t.Helper()
	if !Available() {
		t.Skip("sqlite3 not installed")
	}
	dir := filepath.Join(t.TempDir(), "Application Support")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.vscdb")
	cmd := exec.Command("sqlite3", path)
	cmd.Stdin = strings.NewReader(
		"PRAGMA journal_mode=WAL;\n" +
			"CREATE TABLE kv (key TEXT, value BLOB);\n" +
			"INSERT INTO kv VALUES ('composerData:26369f10', X'7B7D');\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlite3: %v: %s", err, out)
	}
	return path
}

func TestQueryReadsBothWays(t *testing.T) {
	path := db(t)
	for name, query := range map[string]func(string, string) ([][]string, error){
		"snapshot": Query,
		"in place": QueryReadOnly,
	} {
		rows, err := query(path, "SELECT key, hex(value) FROM kv;")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(rows) != 1 || rows[0][0] != "composerData:26369f10" || rows[0][1] != "7B7D" {
			t.Errorf("%s: rows = %v", name, rows)
		}
	}
}

// TestReadOnlyDoesNotWriteTheDatabase is the promise that lets Cairn read a
// store Cursor is writing to at that moment: too large to copy, so it is opened
// in place instead.
func TestReadOnlyDoesNotWriteTheDatabase(t *testing.T) {
	path := db(t)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := QueryReadOnly(path, "SELECT count(*) FROM kv;"); err != nil {
		t.Fatal(err)
	}
	if _, err := QueryReadOnly(path, "CREATE TABLE nope (x);"); err == nil {
		t.Error("a write must be refused, not silently applied")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		t.Error("reading must never modify the user's live database")
	}
}

func TestQueryReadOnlyReportsAMissingFile(t *testing.T) {
	if _, err := QueryReadOnly(filepath.Join(t.TempDir(), "absent.vscdb"), "SELECT 1;"); err == nil {
		t.Error("a missing database must be an error, not an empty result")
	}
}

func TestFileURIEscapesWhatWouldTruncateThePath(t *testing.T) {
	cases := map[string]string{
		"/Users/you/Application Support/state.vscdb": "file:/Users/you/Application%20Support/state.vscdb",
		// '?' and '#' start the query and fragment: unescaped, the path ends there.
		"/tmp/od#d?b/x.db": "file:/tmp/od%23d%3Fb/x.db",
		"/tmp/100%/x.db":   "file:/tmp/100%25/x.db",
	}
	for path, want := range cases {
		if got := fileURI(path); got != want {
			t.Errorf("fileURI(%q) = %q, want %q", path, got, want)
		}
	}
}
