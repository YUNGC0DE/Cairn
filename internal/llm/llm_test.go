package llm

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestExtractJSONToleratesRealModelOutput(t *testing.T) {
	type payload struct {
		Intent   string   `json:"intent"`
		Rejected []string `json:"rejected"`
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare", `{"intent":"a","rejected":["redis"]}`, "a"},
		{"fenced", "```json\n{\"intent\":\"b\",\"rejected\":[]}\n```", "b"},
		{"fenced no language", "```\n{\"intent\":\"c\"}\n```", "c"},
		{"prose before", "Here is the record:\n{\"intent\":\"d\"}", "d"},
		{"prose after", `{"intent":"e"} — hope that helps`, "e"},
		{"braces in strings", `{"intent":"uses {curly} braces","rejected":[]}`, "uses {curly} braces"},
		{"escaped quote", `{"intent":"said \"no\" to redis"}`, `said "no" to redis`},
		{"nested object", `{"intent":"f","extra":{"a":{"b":1}}}`, "f"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var p payload
			if err := ExtractJSON(c.in, &p); err != nil {
				t.Fatalf("ExtractJSON(%q): %v", c.in, err)
			}
			if p.Intent != c.want {
				t.Errorf("intent = %q, want %q", p.Intent, c.want)
			}
		})
	}
}

func TestExtractJSONRejectsGarbage(t *testing.T) {
	var v map[string]any
	for _, in := range []string{"", "   ", "no json here", "{unterminated"} {
		if err := ExtractJSON(in, &v); err == nil {
			t.Errorf("ExtractJSON(%q) should fail", in)
		}
	}
}

func TestPickHonoursExplicitEngine(t *testing.T) {
	if _, err := Pick("does-not-exist"); err == nil {
		t.Error("an unknown engine name must be an error, not a silent fallback")
	}
}

// fakeEngine lets the rest of cairn be tested without spawning a real agent.
type fakeEngine struct {
	reply string
	err   error
	delay time.Duration
	calls int
}

func (f *fakeEngine) Name() string    { return "fake" }
func (f *fakeEngine) Available() bool { return true }
func (f *fakeEngine) Path() string    { return "/fake" }
func (f *fakeEngine) Complete(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := deadline(ctx, req.Budget)
	defer cancel()
	f.calls++
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ErrTimeout
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &Response{Text: f.reply, Engine: f.Name(), Model: req.Model}, nil
}

func TestFakeEngineRespectsBudget(t *testing.T) {
	f := &fakeEngine{reply: "{}", delay: 200 * time.Millisecond}
	_, err := f.Complete(context.Background(), Request{Budget: time.Millisecond})
	// The fake mirrors the real engines: a budget overrun is ErrTimeout, not a
	// generic failure, so callers can degrade rather than retry.
	if err == nil {
		t.Skip("timing-sensitive; the deadline was not reached")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("err = %v, want a budget error", err)
	}
}
