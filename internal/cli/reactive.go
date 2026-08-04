package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/YUNGC0DE/git-cairn/internal/gitx"
)

// The delivery half of the reactive channel: the harness tells cairn that an
// agent is about to touch a file, and cairn answers with the file's history.
//
// Nothing here may ever fail a tool call. A memory system that breaks the agent
// it is supposed to help gets uninstalled within the hour, so every error path
// returns nil and says nothing — the same rule prepare-commit-msg follows for
// commits.

// What counts as "opening or editing a file" is decided by the payload, not by
// a list of tool names: the harnesses name their tools differently (Read/Edit in
// Claude Code, read_file/edit_file in Cursor) and both will rename them again.
// A tool call that carries a file path is a file touch; one that does not — a
// shell command, a search — is not. A Read counts on purpose: the rule is first
// *open* or edit, and an agent that reads before it writes should be told before
// it decides, not after.

// hookEvent is the union of the payloads the harnesses send on stdin. The
// harnesses disagree about names — Claude Code nests the path under tool_input
// and calls the session session_id, Cursor puts the path at the top level and
// calls it conversation_id — so both spellings are accepted and the reader picks
// whichever is present.
type hookEvent struct {
	SessionID      string   `json:"session_id"`
	ConversationID string   `json:"conversation_id"`
	CWD            string   `json:"cwd"`
	WorkspaceRoots []string `json:"workspace_roots"`
	HookEventName  string   `json:"hook_event_name"`
	ToolName       string   `json:"tool_name"`
	// ToolInput is left untyped: it is one tool's arguments, and which key holds
	// the path is the tool's business, not the harness's.
	ToolInput  map[string]any `json:"tool_input"`
	FilePath   string         `json:"file_path"`
	Path       string         `json:"path"`
	TargetFile string         `json:"target_file"`
	Trigger    string         `json:"trigger"`
}

func (e hookEvent) session() string {
	if e.SessionID != "" {
		return e.SessionID
	}
	return e.ConversationID
}

// pathKeys are the spellings a tool uses for "the file this call is about",
// across the harnesses cairn reads. Claude Code's Read/Edit send file_path;
// Cursor's read_file and edit_file send target_file or path.
//
// Reading only file_path — which is what this did — meant Cursor's hook fired,
// found no path, and returned silence on every file the agent opened. The same
// list already exists in both Cursor transcript parsers, which is where it came
// from.
var pathKeys = []string{
	"file_path", "target_file", "targetFile", "path", "filePath",
	"notebook_path", "notebookPath",
}

func (e hookEvent) path() string {
	for _, k := range pathKeys {
		if s, ok := e.ToolInput[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	// Some events carry the path at the top level instead of under tool_input.
	for _, s := range []string{e.FilePath, e.TargetFile, e.Path} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// dir resolves which repository the event happened in. The hook process starts
// in the harness's working directory, which is usually right but not always.
func (e hookEvent) dir(fallback string) string {
	if e.CWD != "" {
		return e.CWD
	}
	if len(e.WorkspaceRoots) > 0 && e.WorkspaceRoots[0] != "" {
		return e.WorkspaceRoots[0]
	}
	if p := e.path(); filepath.IsAbs(p) {
		return filepath.Dir(p)
	}
	return fallback
}

func readEvent(env *Env) (hookEvent, bool) {
	var e hookEvent
	b, err := io.ReadAll(io.LimitReader(env.In(), 1<<20))
	if err != nil || len(b) == 0 {
		return e, false
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, false
	}
	return e, true
}

// contextFor is the shared body of every reactive hook: resolve the repository,
// honour the once-per-file-per-session rule, and render.
func contextFor(env *Env, e hookEvent) string {
	path := e.path()
	repo, err := gitx.Open(e.dir(env.Dir))
	if err != nil || path == "" {
		traceEvent(env, repo, e, path, 0, "no path in the payload")
		return ""
	}
	out, err := serveContext(repo, serveRequest{
		Path: path, Session: e.session(), Hooked: true,
	})
	if err != nil {
		traceEvent(env, repo, e, path, 0, "error: "+err.Error())
		return ""
	}
	why := "served"
	if out == "" {
		why = "silent (already served this session, budget spent, or no records)"
	}
	traceEvent(env, repo, e, path, len(out), why)
	return out
}

// traceEvent records one touch when CAIRN_DEBUG is set, and nothing otherwise.
//
// It exists because a harness that swallows an injection is indistinguishable,
// from the outside, from cairn never having produced one — and that ambiguity
// cost a real afternoon: Cursor reported receiving nothing for a file cairn had
// answered with 12 kB. Cairn can only prove its own half, so its own half is
// what this writes down: which event arrived, under which session id, which key
// carried the path, and what went back.
func traceEvent(env *Env, repo *gitx.Repo, e hookEvent, path string, n int, why string) {
	if env.Getenv == nil || env.Getenv("CAIRN_DEBUG") == "" || repo == nil {
		return
	}
	event := e.HookEventName
	if event == "" {
		event = "(unnamed)"
	}
	logRun(repo, fmt.Sprintf("%s %s tool=%s session=%s path=%s → %d bytes, %s",
		event, pathKeyUsed(e), orNone(e.ToolName), orNone(e.session()), orNone(path), n, why), nil)
}

// pathKeyUsed names which spelling the harness used, which is the detail that
// makes a payload mismatch obvious instead of invisible.
func pathKeyUsed(e hookEvent) string {
	for _, k := range pathKeys {
		if s, ok := e.ToolInput[k].(string); ok && strings.TrimSpace(s) != "" {
			return "tool_input." + k
		}
	}
	switch {
	case e.FilePath != "":
		return "file_path"
	case e.TargetFile != "":
		return "target_file"
	case e.Path != "":
		return "path"
	}
	return "(none)"
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// hookPreToolUse answers Claude Code's PreToolUse. Silence — exit 0, no output —
// means "nothing to add", which is the common case once a file has been served.
func hookPreToolUse(env *Env, _ []string) error {
	e, ok := readEvent(env)
	if !ok {
		return nil
	}
	out := contextFor(env, e)
	if out == "" {
		return nil
	}
	resp := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "PreToolUse",
			"additionalContext": out,
		},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return nil
	}
	fmt.Fprintln(env.Out, string(b))
	return nil
}

// hookCursorPreToolUse answers Cursor's preToolUse.
//
// Cursor's contract is the same idea in different words — snake_case, and
// additional_context at the top level rather than nested under a per-event key.
// Two things about it decided the design:
//
//   - beforeReadFile, the obvious-looking event, is useless here: its response
//     only carries permission and user_message, and user_message goes to the
//     human. preToolUse is the only file-touch event whose response can reach
//     the model;
//   - permission is deliberately omitted from the response. It is optional, and
//     answering it would mean cairn silently overriding the user's own approval
//     settings on every file the agent opens. Cairn observes; it does not gate.
func hookCursorPreToolUse(env *Env, _ []string) error {
	e, ok := readEvent(env)
	if !ok {
		return nil
	}
	out := contextFor(env, e)
	if out == "" {
		return nil
	}
	b, err := json.Marshal(map[string]any{"additional_context": out})
	if err != nil {
		return nil
	}
	fmt.Fprintln(env.Out, string(b))
	return nil
}

// hookPreCompact clears the served set. Compaction is the one event that makes
// "it is already in the context" false: the transcript is summarised and the
// injected blocks go with it, so every file becomes unserved again.
func hookPreCompact(env *Env, _ []string) error {
	e, _ := readEvent(env)
	if e.session() == "" {
		return nil
	}
	repo, err := gitx.Open(e.dir(env.Dir))
	if err != nil {
		return nil
	}
	_ = clearServed(repo, e.session())
	return nil
}

// hookSessionEnd drops the state file so .git/cairn/sessions does not grow
// without bound.
func hookSessionEnd(env *Env, _ []string) error {
	e, _ := readEvent(env)
	if e.session() == "" {
		return nil
	}
	repo, err := gitx.Open(e.dir(env.Dir))
	if err != nil {
		return nil
	}
	_ = clearServed(repo, e.session())
	return nil
}

// --- installation -----------------------------------------------------------

// installClaudeHooks wires the reactive channel into a repository's
// .claude/settings.json, preserving anything already there.
func installClaudeHooks(env *Env, repo *gitx.Repo, bin string) error {
	path := filepath.Join(repo.Root, ".claude", "settings.json")
	root := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &root); err != nil {
			return fmt.Errorf("%s is not valid JSON, leaving it alone", rel(repo.Root, path))
		}
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	cmd := func(sub string) map[string]any {
		return map[string]any{"type": "command", "command": fmt.Sprintf("%q hook %s", bin, sub)}
	}
	set := func(event, matcher, sub string) {
		entry := map[string]any{"hooks": []any{cmd(sub)}}
		if matcher != "" {
			entry["matcher"] = matcher
		}
		existing, _ := hooks[event].([]any)
		kept := make([]any, 0, len(existing)+1)
		for _, x := range existing {
			if !strings.Contains(fmt.Sprint(x), "hook "+sub) {
				kept = append(kept, x)
			}
		}
		hooks[event] = append(kept, entry)
	}
	set("PreToolUse", "Read|Edit|Write|MultiEdit|NotebookEdit", "pre-tool-use")
	set("PreCompact", "", "pre-compact")
	set("SessionEnd", "", "session-end")
	root["hooks"] = hooks

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "✓ reactive recall wired into %s\n", rel(repo.Root, path))
	fmt.Fprintln(env.Out, "  (Claude Code reads hook settings at startup — restart the session to arm it)")
	return nil
}

// installCursorHooks does the same for Cursor's project hooks file.
func installCursorHooks(env *Env, repo *gitx.Repo, bin string) error {
	path := filepath.Join(repo.Root, ".cursor", "hooks.json")
	root := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &root); err != nil {
			return fmt.Errorf("%s is not valid JSON, leaving it alone", rel(repo.Root, path))
		}
	}
	root["version"] = 1
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	set := func(event, sub string) {
		existing, _ := hooks[event].([]any)
		kept := make([]any, 0, len(existing)+1)
		for _, x := range existing {
			if !strings.Contains(fmt.Sprint(x), "hook "+sub) {
				kept = append(kept, x)
			}
		}
		hooks[event] = append(kept, map[string]any{
			"command": fmt.Sprintf("%q hook %s", bin, sub),
		})
	}
	set("preToolUse", "cursor-pre-tool-use")
	set("preCompact", "pre-compact")
	set("sessionEnd", "session-end")
	root["hooks"] = hooks

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "✓ reactive recall wired into %s\n", rel(repo.Root, path))
	return nil
}
