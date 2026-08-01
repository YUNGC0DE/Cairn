// Package llm runs the two distillation calls through whatever coding agent the
// user already has installed (spec §5): the user's own subscription does the
// work, so cairn needs no API key and costs its operator nothing.
//
// Every engine is a subprocess in headless mode with a hard deadline. A slow or
// broken engine degrades the record; it never blocks or fails a commit.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Request is one single-turn completion.
type Request struct {
	System string        // system prompt; engines that cannot set one prepend it
	Prompt string        // user prompt
	Model  string        // engine-specific alias ("sonnet", "haiku", …); "" = engine default
	Budget time.Duration // hard wall-clock limit
}

// Response is what an engine produced.
type Response struct {
	Text    string
	Engine  string
	Model   string
	Elapsed time.Duration
}

// Engine is one headless agent backend.
type Engine interface {
	// Name is the stable engine id, e.g. "claude-code".
	Name() string
	// Available reports whether this engine can run right now.
	Available() bool
	// Path is where the engine was found, for `cairn doctor`. Empty when absent.
	Path() string
	// Complete runs one turn. It must respect ctx and Request.Budget.
	Complete(ctx context.Context, req Request) (*Response, error)
}

// ErrNoEngine means no headless agent is installed.
var ErrNoEngine = errors.New("llm: no usable engine (install claude or cursor-agent)")

// ErrTimeout means the engine exceeded its budget. Callers degrade rather than fail.
var ErrTimeout = errors.New("llm: engine exceeded its time budget")

// Engines lists all engines in preference order.
//
// spec §5 also lists an API-key fallback. It is deliberately absent from v0.1:
// it cannot be exercised from the development sandbox (no network), and an
// untested network path in a git hook is worse than no path at all. See ROADMAP.
func Engines() []Engine {
	return []Engine{&ClaudeCode{}, &CursorAgent{}}
}

// Pick selects an engine. name "" or "auto" picks the first available one.
// CAIRN_ENGINE overrides when name is empty.
func Pick(name string) (Engine, error) {
	if name == "" {
		name = os.Getenv("CAIRN_ENGINE")
	}
	if name != "" && name != "auto" {
		for _, e := range Engines() {
			if e.Name() == name {
				if !e.Available() {
					return nil, fmt.Errorf("llm: engine %q is not available here", name)
				}
				return e, nil
			}
		}
		return nil, fmt.Errorf("llm: unknown engine %q", name)
	}
	for _, e := range Engines() {
		if e.Available() {
			return e, nil
		}
	}
	return nil, ErrNoEngine
}

// ExtractJSON pulls the first complete JSON object out of a model response,
// tolerating code fences and surrounding prose. Models are asked for bare JSON
// but do not reliably comply, and a record must not be lost to a stray fence.
func ExtractJSON(s string, v any) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("llm: empty response")
	}
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if j := strings.IndexByte(rest, '\n'); j >= 0 {
			rest = rest[j+1:]
		}
		if k := strings.Index(rest, "```"); k >= 0 {
			rest = rest[:k]
		}
		if obj := balancedObject(rest); obj != "" {
			if err := json.Unmarshal([]byte(obj), v); err == nil {
				return nil
			}
		}
	}
	obj := balancedObject(s)
	if obj == "" {
		return fmt.Errorf("llm: no JSON object in response: %s", clip(s, 200))
	}
	if err := json.Unmarshal([]byte(obj), v); err != nil {
		return fmt.Errorf("llm: bad JSON: %w: %s", err, clip(obj, 300))
	}
	return nil
}

// balancedObject returns the first brace-balanced object, ignoring braces inside
// strings.
func balancedObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// deadline returns a context honouring the request budget.
func deadline(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, budget)
}
