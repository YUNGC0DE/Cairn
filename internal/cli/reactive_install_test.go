package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YUNGC0DE/git-cairn/internal/testutil"
)

func runInit(t *testing.T, r *testutil.Repo, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	env := &Env{Out: &out, Err: &bytes.Buffer{}, Dir: r.Root,
		Getenv: func(string) string { return "" }}
	if err := cmdInit(env, args); err != nil {
		t.Fatalf("init: %v\n%s", err, out.String())
	}
	return out.String()
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s is not valid JSON: %v\n%s", path, err, b)
	}
	return m
}

// Installing the reactive channel must never cost the user a setting they had
// already made. These files belong to their editor, not to cairn.
func TestInitPreservesExistingHarnessConfig(t *testing.T) {
	r := testutil.NewRepo(t)
	r.Write(".claude/settings.json", `{
  "model": "opus",
  "permissions": { "allow": ["Bash(npm test)"] },
  "hooks": {
    "PreToolUse": [ { "matcher": "Bash", "hooks": [{ "type": "command", "command": "/my/guard.sh" }] } ],
    "Stop": [ { "hooks": [{ "type": "command", "command": "/my/notify.sh" }] } ]
  }
}`)
	r.Write(".cursor/hooks.json", `{
  "version": 1,
  "hooks": {
    "stop": [ { "command": "/my/record-usage" } ],
    "preToolUse": [ { "command": "/my/other-pretool.sh" } ]
  }
}`)

	runInit(t, r)

	claude := readJSON(t, filepath.Join(r.Root, ".claude", "settings.json"))
	if claude["model"] != "opus" {
		t.Errorf("unrelated setting lost: model = %v", claude["model"])
	}
	if claude["permissions"] == nil {
		t.Error("permissions block was dropped")
	}
	claudeRaw, _ := json.Marshal(claude)
	for _, want := range []string{"/my/guard.sh", "/my/notify.sh", "hook pre-tool-use", "hook pre-compact", "hook session-end"} {
		if !strings.Contains(string(claudeRaw), want) {
			t.Errorf("claude settings missing %q:\n%s", want, claudeRaw)
		}
	}

	cursorRaw, _ := json.Marshal(readJSON(t, filepath.Join(r.Root, ".cursor", "hooks.json")))
	for _, want := range []string{"/my/record-usage", "/my/other-pretool.sh", "hook cursor-pre-tool-use", "hook session-end"} {
		if !strings.Contains(string(cursorRaw), want) {
			t.Errorf("cursor hooks missing %q:\n%s", want, cursorRaw)
		}
	}
}

// Re-running init upgrades cairn's own entries instead of stacking duplicates.
func TestInitIsIdempotent(t *testing.T) {
	r := testutil.NewRepo(t)
	runInit(t, r)
	first, err := os.ReadFile(filepath.Join(r.Root, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	runInit(t, r)
	second, _ := os.ReadFile(filepath.Join(r.Root, ".cursor", "hooks.json"))
	if !bytes.Equal(first, second) {
		t.Errorf("second init changed the file:\n--- first\n%s\n--- second\n%s", first, second)
	}
	if n := strings.Count(string(second), "cursor-pre-tool-use"); n != 1 {
		t.Errorf("hook entry duplicated %d times", n)
	}
}

// A config cairn cannot parse is a config cairn does not touch: rewriting it
// from a partial understanding would destroy settings it failed to read.
func TestInitLeavesUnparseableConfigAlone(t *testing.T) {
	r := testutil.NewRepo(t)
	broken := "{\n  // a comment json does not allow\n  \"model\": \"opus\",\n}\n"
	r.Write(".claude/settings.json", broken)

	out := runInit(t, r)

	after, err := os.ReadFile(filepath.Join(r.Root, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != broken {
		t.Errorf("an unparseable config was rewritten:\n%s", after)
	}
	if !strings.Contains(out, "not valid JSON") {
		t.Errorf("the refusal must be reported, not silent:\n%s", out)
	}
}

// Somebody else's commit hook is never overwritten without --force.
func TestInitDoesNotClobberForeignGitHook(t *testing.T) {
	r := testutil.NewRepo(t)
	mine := "#!/bin/sh\necho my own hook\n"
	path := filepath.Join(r.GitDir, "hooks", "prepare-commit-msg")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(mine), 0o755); err != nil {
		t.Fatal(err)
	}

	out := runInit(t, r)

	after, _ := os.ReadFile(path)
	if string(after) != mine {
		t.Errorf("a foreign hook was replaced:\n%s", after)
	}
	if !strings.Contains(out, "already exists and is not cairn's") {
		t.Errorf("the skip must be reported:\n%s", out)
	}
	// The reactive channel still gets wired: the two are independent.
	if _, err := os.Stat(filepath.Join(r.Root, ".cursor", "hooks.json")); err != nil {
		t.Errorf("a blocked git hook stopped the reactive wiring: %v", err)
	}
}
