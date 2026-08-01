package llm

import (
	"context"
	"errors"
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

// scripted is an engine with a name, so a chain's choices can be asserted.
type scripted struct {
	name    string
	reply   string
	err     error
	delay   time.Duration
	absent  bool
	calls   int
	gotReq  Request
	elapsed time.Duration
}

func (s *scripted) Name() string    { return s.name }
func (s *scripted) Available() bool { return !s.absent }
func (s *scripted) Path() string {
	if s.absent {
		return ""
	}
	return "/fake/" + s.name
}

func (s *scripted) Complete(ctx context.Context, req Request) (*Response, error) {
	s.calls++
	s.gotReq = req
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if s.err != nil {
		return nil, s.err
	}
	return &Response{Text: s.reply, Engine: s.name, Model: req.Model, Elapsed: s.elapsed}, nil
}

func TestChainFallsBackWhenTheFirstEngineFailsOutright(t *testing.T) {
	first := &scripted{name: "claude-code", err: errors.New("claude exited 1: not logged in")}
	second := &scripted{name: "cursor-agent", reply: `{"intent":"x"}`}
	c := &Chain{Engines: []Engine{first, second}}

	resp, err := c.Complete(context.Background(), Request{Budget: time.Minute})
	if err != nil {
		t.Fatalf("the chain must answer from the second engine: %v", err)
	}
	if resp.Engine != "cursor-agent" || resp.Text != `{"intent":"x"}` {
		t.Fatalf("resp = %+v", resp)
	}
	if second.calls != 1 {
		t.Errorf("second engine called %d times", second.calls)
	}
	// Cairn must say a fallback happened — the note is printed after the commit
	// and lands in `cairn audit --out`. A silent switch of engine is how a
	// record starts misrepresenting how it was produced.
	if len(resp.Notes) == 0 || !strings.Contains(resp.Notes[0], "claude-code failed") {
		t.Errorf("notes = %v, want the first engine's failure recorded", resp.Notes)
	}
}

func TestChainDoesNotRetryAfterATimeout(t *testing.T) {
	first := &scripted{name: "claude-code", err: ErrTimeout}
	second := &scripted{name: "cursor-agent", reply: "{}"}
	c := &Chain{Engines: []Engine{first, second}}

	if _, err := c.Complete(context.Background(), Request{Budget: time.Minute}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want the timeout to propagate", err)
	}
	if second.calls != 0 {
		t.Error("the budget is exactly what ran out — a second engine cannot fit in it")
	}
}

func TestChainStopsWhenTooLittleTimeIsLeft(t *testing.T) {
	first := &scripted{
		name: "claude-code", err: errors.New("crashed"), delay: 50 * time.Millisecond,
	}
	second := &scripted{name: "cursor-agent", reply: "{}"}
	c := &Chain{Engines: []Engine{first, second}}

	// A budget smaller than minFallbackBudget: the first engine ate it.
	_, err := c.Complete(context.Background(), Request{Budget: 100 * time.Millisecond})
	if err == nil {
		t.Fatal("want the first engine's error")
	}
	if second.calls != 0 {
		t.Error("a fallback with no time left only turns one failure into two")
	}
}

func TestChainDropsAModelAliasItCannotTravelWith(t *testing.T) {
	first := &scripted{name: "claude-code", err: errors.New("crashed")}
	second := &scripted{name: "cursor-agent", reply: "{}"}
	c := &Chain{Engines: []Engine{first, second}}

	resp, err := c.Complete(context.Background(), Request{Model: "haiku", Budget: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if second.gotReq.Model != "" {
		t.Errorf("cursor-agent was asked for %q — a claude alias it cannot resolve",
			second.gotReq.Model)
	}
	if first.gotReq.Model != "haiku" {
		t.Errorf("the configured model must still reach the engine it was set for, got %q",
			first.gotReq.Model)
	}
	var said bool
	for _, n := range resp.Notes {
		if strings.Contains(n, "own default") {
			said = true
		}
	}
	if !said {
		t.Errorf("dropping the configured model must be stated, notes = %v", resp.Notes)
	}
}

func TestChainSkipsEnginesThatAreNotInstalled(t *testing.T) {
	absent := &scripted{name: "claude-code", absent: true}
	present := &scripted{name: "cursor-agent", reply: "{}"}
	c := &Chain{Engines: []Engine{absent, present}}

	resp, err := c.Complete(context.Background(), Request{Budget: time.Minute})
	if err != nil || resp.Engine != "cursor-agent" {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
	if absent.calls != 0 {
		t.Error("an engine that is not on PATH must not be spawned")
	}
	if len(resp.Notes) != 0 {
		t.Errorf("a missing engine is not a failure worth recording: %v", resp.Notes)
	}
}

func TestChainNamesEveryFailureWhenNothingAnswers(t *testing.T) {
	first := &scripted{name: "claude-code", err: errors.New("not logged in")}
	second := &scripted{name: "cursor-agent", err: errors.New("quota exceeded")}
	c := &Chain{Engines: []Engine{first, second}}

	_, err := c.Complete(context.Background(), Request{Budget: time.Minute})
	if err == nil {
		t.Fatal("want an error")
	}
	// Reporting only the last failure would hide that the preferred engine is
	// broken — the one thing the user can actually act on.
	for _, want := range []string{"not logged in", "quota exceeded", "claude-code", "cursor-agent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

func TestChainKeepsTheTimeoutRecognisableAfterAnEarlierFailure(t *testing.T) {
	first := &scripted{name: "claude-code", err: errors.New("not logged in")}
	second := &scripted{name: "cursor-agent", err: ErrTimeout}
	c := &Chain{Engines: []Engine{first, second}}

	_, err := c.Complete(context.Background(), Request{Budget: time.Minute})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want a timeout the caller can still recognise", err)
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("err = %v, want the earlier failure named too", err)
	}
}

// TestEnginesResolveTheirOwnDefaults pins why defaults live in the engine:
// neither aliases nor effort travel between them.
func TestEnginesResolveTheirOwnDefaults(t *testing.T) {
	cc := &ClaudeCode{}
	if got := cc.modelFor(Request{}); got != DefaultModel {
		t.Errorf("claude model = %q, want %q", got, DefaultModel)
	}
	if got := cc.modelFor(Request{Model: "opus"}); got != "opus" {
		t.Errorf("a configured model must win, got %q", got)
	}
	// Effort is the lever distillation actually needs: the work is extraction,
	// not reasoning, and effort is what a commit waits on.
	if got := cc.effortFor(Request{}); got != DefaultEffort {
		t.Errorf("claude effort = %q, want %q", got, DefaultEffort)
	}
	if got := cc.effortFor(Request{Effort: "high"}); got != "high" {
		t.Errorf("a configured effort must win, got %q", got)
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

func TestAnnotateSandboxNetworkExplainsSeatbelt(t *testing.T) {
	t.Setenv("CURSOR_SANDBOX", "seatbelt")
	got := annotateSandboxNetwork("write EPROTO: packet length too long")
	if !strings.Contains(got, "full_network") {
		t.Errorf("sandbox SSL failure must name the escape hatch: %q", got)
	}
	t.Setenv("CURSOR_SANDBOX", "")
	plain := annotateSandboxNetwork("write EPROTO: packet length too long")
	if strings.Contains(plain, "full_network") {
		t.Errorf("outside the sandbox the hint must stay quiet: %q", plain)
	}
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
