package distill

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YUNGC0DE/git-cairn/internal/llm"
	"github.com/YUNGC0DE/git-cairn/internal/transcript"
)

// perSession answers each extraction with a record naming the session it saw,
// so a test can tell whose intent survived.
type perSession struct {
	calls   atomic.Int32
	prompts []string
}

func (p *perSession) Name() string    { return "per-session" }
func (p *perSession) Available() bool { return true }
func (p *perSession) Path() string    { return "/fake" }

func (p *perSession) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.calls.Add(1)
	if strings.Contains(req.System, "verification") {
		return &llm.Response{Text: `{"claims":[]}`, Engine: p.Name()}, nil
	}
	who := "unknown"
	for _, tag := range []string{"ALPHA", "BETA"} {
		if strings.Contains(req.Prompt, tag) {
			who = tag
		}
	}
	p.prompts = append(p.prompts, who)
	return &llm.Response{Engine: p.Name(), Text: fmt.Sprintf(`{
	  "intent": "%s intent",
	  "decision": "%s decision",
	  "rejected": [{"option": "%s option", "reason": "%s reason"}],
	  "invariants": [{"text": "shared rule", "scope": ["internal/**"]}],
	  "open_items": ["%s open"],
	  "next_step": "%s next",
	  "claims": ["%s claim"]
	}`, who, who, who, who, who, who, who)}, nil
}

func sessionSaying(id, tag, file string) *transcript.Session {
	return &transcript.Session{
		Ref: transcript.Ref{Agent: "claude-code", ID: id},
		Messages: []transcript.Message{
			{Role: transcript.RoleUser, Text: tag + " please do the thing"},
			{Role: transcript.RoleAssistant, Thinking: tag + " reasoning",
				Tools: []transcript.ToolCall{{Name: "Edit", Files: []string{file}}}},
		},
	}
}

// TestCommitGranularityDoesNotChangeTheRecord is the rule behind per-session
// extraction: committing after every session and committing everything at once
// must carry the same information. One shared prompt budget could not do that —
// the second session's intent was squeezed out by the first.
func TestCommitGranularityDoesNotChangeTheRecord(t *testing.T) {
	in := Input{
		Sessions: []*transcript.Session{
			sessionSaying("sess-alpha", "ALPHA", "a.go"),
			sessionSaying("sess-beta", "BETA", "b.go"),
		},
		Diff:  "diff --git a/a.go b/a.go\n+package a\n",
		Files: []string{"a.go", "b.go"},
	}
	eng := &perSession{}
	res, err := Run(context.Background(), eng, in, Options{Budget: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	// Both sessions were distilled on their own.
	if got := len(eng.prompts); got != 2 {
		t.Fatalf("extraction calls = %d, want one per session: %v", got, eng.prompts)
	}
	for _, tag := range []string{"ALPHA", "BETA"} {
		if !strings.Contains(res.Extraction.Intent, tag+" intent") {
			t.Errorf("%s's intent was lost:\n%s", tag, res.Extraction.Intent)
		}
		var found bool
		for _, r := range res.Extraction.Rejected {
			if strings.Contains(r.Option, tag) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s's rejected alternative was lost: %+v", tag, res.Extraction.Rejected)
		}
	}
	// A rule both sessions stated is one rule, not two.
	if n := len(res.Extraction.Invariants); n != 1 {
		t.Errorf("invariants = %d, want the duplicate merged: %+v", n, res.Extraction.Invariants)
	}
	// The newest session owns the next step.
	if res.Extraction.NextStep != "BETA next" {
		t.Errorf("next step = %q, want the newest session's", res.Extraction.NextStep)
	}
}

// Time budget is per session, like promptBudget: two sessions must each get the
// solo-commit extract slice, not half of one shared pool.
func TestTimeBudgetScalesPerSession(t *testing.T) {
	in := Input{
		Sessions: []*transcript.Session{
			sessionSaying("sess-alpha", "ALPHA", "a.go"),
			sessionSaying("sess-beta", "BETA", "b.go"),
		},
		Diff:  "diff --git a/a.go b/a.go\n+package a\n",
		Files: []string{"a.go", "b.go"},
	}
	eng := &budgetProbe{}
	const perSession = 20 * time.Second
	_, err := Run(context.Background(), eng, in, Options{Budget: perSession, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	want := perSession - minVerifyBudget
	if len(eng.budgets) != 2 {
		t.Fatalf("extraction calls = %d, want 2: %v", len(eng.budgets), eng.budgets)
	}
	for i, got := range eng.budgets {
		if got != want {
			t.Errorf("call %d budget = %s, want %s (solo extract slice)", i, got, want)
		}
	}
}

// budgetProbe records the Budget each Complete call was given.
type budgetProbe struct {
	budgets []time.Duration
	mu      sync.Mutex
}

func (b *budgetProbe) Name() string    { return "budget-probe" }
func (b *budgetProbe) Available() bool { return true }
func (b *budgetProbe) Path() string    { return "/fake" }

func (b *budgetProbe) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	b.mu.Lock()
	b.budgets = append(b.budgets, req.Budget)
	b.mu.Unlock()
	return &llm.Response{Engine: b.Name(), Text: `{"intent":"x","claims":[]}`}, nil
}

// A session whose call fails must not take the others' records down with it.
func TestOneFailedSessionDoesNotLoseTheOthers(t *testing.T) {
	in := Input{
		Sessions: []*transcript.Session{
			sessionSaying("sess-alpha", "ALPHA", "a.go"),
			sessionSaying("sess-beta", "BETA", "b.go"),
		},
		Diff:  "diff --git a/a.go b/a.go\n",
		Files: []string{"a.go", "b.go"},
	}
	eng := &flaky{}
	res, err := Run(context.Background(), eng, in, Options{Budget: 30 * time.Second, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Extraction == nil || !strings.Contains(res.Extraction.Intent, "survivor") {
		t.Fatalf("the session that answered must still produce a record: %+v", res.Extraction)
	}
	var said bool
	for _, n := range res.Notes {
		if strings.Contains(n, "not distilled") {
			said = true
		}
	}
	if !said {
		t.Errorf("a session that could not be distilled must be named in the notes: %v", res.Notes)
	}
}

type flaky struct{ n atomic.Int32 }

func (f *flaky) Name() string    { return "flaky" }
func (f *flaky) Available() bool { return true }
func (f *flaky) Path() string    { return "/fake" }
func (f *flaky) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	if f.n.Add(1) == 1 {
		return nil, fmt.Errorf("engine exploded")
	}
	return &llm.Response{Engine: f.Name(), Text: `{"intent":"survivor intent","claims":[]}`}, nil
}
