package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ClaudeCode runs `claude -p` in headless mode.
//
// The flag set is tuned for latency, which is the binding constraint inside a
// git hook. Measured on a 2026 laptop with a ~10k-char payload: ~11s with
// default flags, ~7s with these. The bulk of what remains is Claude Code's own
// ~25k-token preamble, which the API caches — so repeat commits are faster than
// the first one after an idle period.
type ClaudeCode struct {
	// Bin overrides the executable (tests).
	Bin string
}

// Name identifies the engine.
func (c *ClaudeCode) Name() string { return "claude-code" }

func (c *ClaudeCode) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "claude"
}

// Available reports whether the claude CLI is on PATH.
func (c *ClaudeCode) Available() bool { return c.Path() != "" }

// Path is where the claude CLI was found.
func (c *ClaudeCode) Path() string { return lookPath(c.bin()) }

// DefaultModel is a deliberate pin rather than the user's configured default:
// distillation is a small extraction job, and inheriting an opus-class default
// would make every commit slower and costlier for no gain in record quality.
const DefaultModel = "sonnet"

// DefaultVerifyModel is the model for the verification pass. Checking whether a
// diff supports a claim is narrow and mechanical, so the small fast model both
// does it well and keeps the pass inside a commit's time budget — measured, the
// difference is what makes a "verified" record possible at the default timeout.
const DefaultVerifyModel = "haiku"

type claudeEnvelope struct {
	IsError    bool            `json:"is_error"`
	Result     string          `json:"result"`
	StopReason string          `json:"stop_reason"`
	SessionID  string          `json:"session_id"`
	Usage      json.RawMessage `json:"usage"`
}

// Complete runs one turn.
func (c *ClaudeCode) Complete(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := deadline(ctx, req.Budget)
	defer cancel()

	model := req.Model
	if model == "" {
		model = DefaultModel
	}
	args := []string{
		"-p",
		"--output-format", "json",
		"--model", model,
		// No MCP servers: each one costs a startup handshake we cannot afford.
		"--strict-mcp-config",
		// Do not litter the user's session history with hook-driven calls.
		"--no-session-persistence",
		"--exclude-dynamic-system-prompt-sections",
		// Tool definitions cannot be removed from the CLI's context, so the model
		// sometimes reaches for one anyway. Denied calls come back as errors it can
		// recover from — but only if it gets another turn, so this is not 1.
		"--max-turns", "4",
	}
	if req.System != "" {
		args = append(args, "--system-prompt", req.System)
	}
	// Distillation is pure text-in/text-out. Denying the tools keeps a stray
	// tool call from burning the time budget or touching the repo.
	args = append(args, "--disallowedTools",
		"Bash,Read,Write,Edit,NotebookEdit,Glob,Grep,WebFetch,WebSearch,Task,TodoWrite,AskUserQuestion")

	start := time.Now()
	out, err := runCommand(ctx, c.bin(), args, req.Prompt)
	elapsed := time.Since(start)
	if err != nil {
		return nil, err
	}

	var env claudeEnvelope
	if json.Unmarshal(out, &env) != nil {
		// Older or future CLI versions may not wrap the result; use raw stdout.
		return &Response{Text: string(out), Engine: c.Name(), Model: model, Elapsed: elapsed}, nil
	}
	if env.IsError {
		return nil, fmt.Errorf("%s: %s", c.Name(), strings.TrimSpace(env.Result))
	}
	return &Response{Text: env.Result, Engine: c.Name(), Model: model, Elapsed: elapsed}, nil
}

// CursorAgent runs `cursor-agent -p` in headless mode. It has no system-prompt
// flag, so the system prompt is folded into the user prompt.
type CursorAgent struct {
	Bin string
}

// Name identifies the engine.
func (c *CursorAgent) Name() string { return "cursor-agent" }

func (c *CursorAgent) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "cursor-agent"
}

// Available reports whether the cursor-agent CLI is on PATH.
func (c *CursorAgent) Available() bool { return c.Path() != "" }

// Path is where the cursor-agent CLI was found.
func (c *CursorAgent) Path() string { return lookPath(c.bin()) }

// Complete runs one turn.
func (c *CursorAgent) Complete(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := deadline(ctx, req.Budget)
	defer cancel()

	prompt := req.Prompt
	if req.System != "" {
		prompt = req.System + "\n\n" + prompt
	}
	args := []string{"-p", prompt, "--output-format", "text"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}

	start := time.Now()
	out, err := runCommand(ctx, c.bin(), args, "")
	elapsed := time.Since(start)
	if err != nil {
		return nil, err
	}
	return &Response{
		Text: string(out), Engine: c.Name(), Model: req.Model, Elapsed: elapsed,
	}, nil
}

// lookPath resolves a binary, returning "" when it is absent.
func lookPath(bin string) string {
	p, err := exec.LookPath(bin)
	if err != nil {
		return ""
	}
	return p
}

// runCommand executes an engine, enforcing the context deadline so a hung agent
// cannot outlive the hook.
func runCommand(ctx context.Context, bin string, args []string, stdin string) ([]byte, error) {
	dir, cleanup := scratchDir()
	defer cleanup()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.Env = scrubEnv(os.Environ())
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%w (%s)", ErrTimeout, bin)
	}
	if err != nil {
		var ee *exec.ExitError
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("%s exited %d: %s", bin, ee.ExitCode(), clip(msg, 300))
		}
		return nil, fmt.Errorf("%s: %w: %s", bin, err, clip(msg, 300))
	}
	return out.Bytes(), nil
}

// scrubEnv removes markers that tell a nested agent it is running inside
// another agent's session, so the child starts clean.
func scrubEnv(env []string) []string {
	drop := map[string]bool{
		"CLAUDECODE":              true,
		"CLAUDE_CODE_ENTRYPOINT":  true,
		"CLAUDE_CODE_SSE_PORT":    true,
		"CLAUDE_CODE_SESSION_ID":  true,
		"CURSOR_AGENT":            true,
		"CAIRN_IN_HOOK":           true,
		"GIT_INDEX_FILE":          true,
		"GIT_DIR":                 true,
		"GIT_WORK_TREE":           true,
		"GIT_AUTHOR_DATE":         true,
		"GIT_COMMITTER_DATE":      true,
		"GIT_EDITOR":              true,
		"GIT_PREFIX":              true,
		"GIT_EXEC_PATH":           true,
		"GIT_REFLOG_ACTION":       true,
		"GIT_INTERNAL_GETTEXT_SH": true,
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && drop[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// scratchDir is where engines run: never the repository, so they cannot pick up
// the project's CLAUDE.md, settings or hooks, and cannot touch the work tree.
func scratchDir() (string, func()) {
	d, err := os.MkdirTemp("", "cairn-llm-")
	if err != nil {
		return os.TempDir(), func() {}
	}
	return d, func() { _ = os.RemoveAll(d) }
}
