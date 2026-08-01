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
//
// Model and Effort are both engine-specific and both optional: an engine that
// receives neither answers on its own defaults. `claude` takes an alias plus an
// `--effort` flag; `cursor-agent` bakes the effort level into the model id and
// has no flag for it, so it ignores Effort entirely.
type Request struct {
	System string        // system prompt; engines that cannot set one prepend it
	Prompt string        // user prompt
	Model  string        // engine-specific alias; "" lets the engine choose
	Effort string        // low|medium|high|xhigh|max, where the engine supports it
	Budget time.Duration // hard wall-clock limit
}

// Response is what an engine produced.
type Response struct {
	Text    string
	Engine  string
	Model   string
	Elapsed time.Duration

	// Notes carry anything the caller should put in the record — today, that a
	// first-choice engine failed and a fallback answered instead. A degradation
	// nobody is told about is the one that erodes trust in the whole record.
	Notes []string
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

// Pick selects what to distil with. A named engine is used alone — naming one
// is a choice, and silently using another would defeat it. "auto" (or nothing)
// returns every available engine as a Chain, which falls back when the first
// one fails outright.
//
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
	var found []Engine
	for _, e := range Engines() {
		if e.Available() {
			found = append(found, e)
		}
	}
	switch len(found) {
	case 0:
		return nil, ErrNoEngine
	case 1:
		return found[0], nil
	default:
		return &Chain{Engines: found}, nil
	}
}

// minFallbackBudget is the least time worth handing a second engine. Below it
// the fallback would only convert one failure into two and spend the commit's
// remaining budget doing it.
const minFallbackBudget = 5 * time.Second

// Chain tries engines in order, moving on when one fails outright.
//
// "Available" only means the binary is on PATH — it cannot tell whether the CLI
// is logged in, in date, or within its quota. Those failures are instant, so
// the cost of trying the next engine is a process spawn, while the cost of not
// trying is every commit degrading to metadata-only with a working agent
// installed alongside.
//
// Two failures are deliberately *not* retried: a timeout, because the budget
// that would pay for the retry is exactly what ran out, and a run that left
// less than minFallbackBudget on the clock.
type Chain struct{ Engines []Engine }

// Name lists the chain in order, so a record says what could have answered.
func (c *Chain) Name() string {
	var names []string
	for _, e := range c.Engines {
		names = append(names, e.Name())
	}
	return strings.Join(names, "→")
}

// Available reports whether any engine in the chain can run.
func (c *Chain) Available() bool {
	for _, e := range c.Engines {
		if e.Available() {
			return true
		}
	}
	return false
}

// Path is where the first available engine was found.
func (c *Chain) Path() string {
	for _, e := range c.Engines {
		if p := e.Path(); p != "" {
			return p
		}
	}
	return ""
}

// Complete runs the first engine that answers.
func (c *Chain) Complete(ctx context.Context, req Request) (*Response, error) {
	var notes, failures []string
	var lastErr error
	start := time.Now()
	for i, e := range c.Engines {
		if !e.Available() {
			continue
		}
		attempt := req
		if i > 0 {
			// A model alias belongs to the engine it was configured for: "haiku"
			// means nothing to cursor-agent. The fallback runs on its own defaults,
			// and says so rather than failing on a name it cannot resolve.
			if attempt.Model != "" {
				notes = append(notes, fmt.Sprintf(
					"model %q was configured for %s; %s ran on its own default instead",
					attempt.Model, c.Engines[0].Name(), e.Name()))
				attempt.Model = ""
			}
			if req.Budget > 0 {
				attempt.Budget = req.Budget - time.Since(start)
				if attempt.Budget < minFallbackBudget {
					notes = append(notes, fmt.Sprintf(
						"no time left to try %s after %s failed", e.Name(), c.Engines[i-1].Name()))
					break
				}
			}
		}
		resp, err := e.Complete(ctx, attempt)
		if err == nil {
			resp.Notes = append(notes, resp.Notes...)
			return resp, nil
		}
		lastErr = err
		failures = append(failures, fmt.Sprintf("%s: %v", e.Name(), err))
		if errors.Is(err, ErrTimeout) || ctx.Err() != nil {
			// The budget is what ran out; a retry cannot fit in what is left.
			// Wrapping keeps errors.Is(…, ErrTimeout) true for the caller, which
			// is what distinguishes "too slow" from "broken" in the record.
			if len(failures) > 1 {
				return nil, fmt.Errorf("%w (%s)", err, strings.Join(failures[:len(failures)-1], "; "))
			}
			return nil, err
		}
		notes = append(notes, fmt.Sprintf("engine %s failed: %v", e.Name(), err))
	}
	switch {
	case lastErr == nil:
		return nil, ErrNoEngine
	case len(failures) > 1:
		// Every engine is named. Reporting only the last one hides that the
		// preferred engine is broken, which is the thing worth fixing.
		return nil, fmt.Errorf("all engines failed — %s", strings.Join(failures, "; "))
	default:
		return nil, lastErr
	}
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
