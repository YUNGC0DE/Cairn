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
// so a test can tell whose decisions survived.
type perSession struct {
	calls   atomic.Int32
	prompts []string
}

func (p *perSession) Name() string    { return "per-session" }
func (p *perSession) Available() bool { return true }
func (p *perSession) Path() string    { return "/fake" }

func (p *perSession) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.calls.Add(1)
	if strings.Contains(req.System, "verification pass") {
		return &llm.Response{Text: `{"claims":[]}`, Engine: p.Name()}, nil
	}
	who, file := "unknown", "a.go"
	for tag, f := range map[string]string{"ALPHA": "a.go", "BETA": "b.go"} {
		if strings.Contains(req.Prompt, tag) {
			who, file = tag, f
		}
	}
	p.prompts = append(p.prompts, who)
	return &llm.Response{Engine: p.Name(), Text: fmt.Sprintf(`{
	  "rejected": [{"rule": "%s option", "why": "%s reason", "files": ["%s"]}],
	  "invariants": [{"rule": "the shared rule that must always hold",
	                  "why": "a reason both sessions gave", "files": ["%s"]}],
	  "claims": ["%s claim"]
	}`, who, who, file, file, who)}, nil
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
// the second session's decisions were squeezed out by the first.
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
		var found bool
		for _, r := range res.Extraction.Rejected {
			if strings.Contains(r.Rule, tag) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s's rejected alternative was lost: %+v", tag, res.Extraction.Rejected)
		}
	}
	// A rule both sessions stated is one rule, not two — and it binds both files,
	// because a rule does not stop binding a.go because the session that wrote
	// b.go phrased it better.
	if n := len(res.Extraction.Invariants); n != 1 {
		t.Fatalf("invariants = %d, want the duplicate merged: %+v", n, res.Extraction.Invariants)
	}
	if n := len(res.Extraction.Invariants[0].Files); n != 2 {
		t.Errorf("merged invariant binds %v, want both files", res.Extraction.Invariants[0].Files)
	}
}

// Two sessions arguing the same option down produce one entry, not two. Exact
// dedup caught none of these: measured over this repository's history, 39
// rejected alternatives across 7 commits included four pairs that were the same
// option said twice in different words.
func TestRewordedDuplicatesAreFoldedTogether(t *testing.T) {
	in := Input{
		Sessions: []*transcript.Session{
			sessionSaying("sess-a", "ALPHA", "a.go"),
			sessionSaying("sess-b", "BETA", "b.go"),
		},
		Diff:  "diff --git a/a.go b/a.go\n",
		Files: []string{"a.go", "b.go"},
	}
	res, err := Run(context.Background(), &reworder{}, in, Options{Budget: 30 * time.Second, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	ex := res.Extraction
	if len(ex.Rejected) != 1 {
		t.Fatalf("rejected = %+v, want the same option counted once", ex.Rejected)
	}
	// Of two wordings of one rejection, the one that explains more survives.
	if !strings.Contains(ex.Rejected[0].Why, "5.4GB") {
		t.Errorf("kept the thinner reason: %q", ex.Rejected[0].Why)
	}
	if len(ex.Invariants) != 1 {
		t.Errorf("invariants = %+v, want one", ex.Invariants)
	}
	// The merge says what it folded away, so a thinner record is explainable.
	if !strings.Contains(strings.Join(res.Notes, " "), "worded twice") {
		t.Errorf("the fold was silent: %v", res.Notes)
	}
}

// Rules about the same property often share an identifier but little else.
// Plain token overlap at dupThreshold misses them; the heavy-token check must
// still leave genuinely different rules ("first" vs "second") alone.
func TestRestatementsSharingAnIdentifierAreFolded(t *testing.T) {
	in := []Rule{
		{Rule: "The installed binary name is git-cairn so git discovers it as a subcommand; help follows argv[0].", Files: []string{"internal/cli/cli.go"}},
		{Rule: "Ship the binary as git-cairn so git finds it as a subcommand, and derive the program name from argv[0].", Files: []string{"Makefile"}},
		{Rule: "Help and usage strings must take the program name from argv[0] when the base name is git-cairn.", Files: []string{"internal/cli/cli.go"}},
		{Rule: "The hook ownership marker must remain # cairn:managed-hook across renames.", Files: []string{"internal/cli/init.go"}},
		{Rule: "first rule that must hold", Files: []string{"a.go"}},
		{Rule: "second rule that must hold", Files: []string{"b.go"}},
	}
	out := dedupRules(in)
	if len(out) != 4 {
		t.Fatalf("rules = %d, want 4 (one git-cairn/argv rule, marker, first, second): %+v", len(out), out)
	}
	var gitCairn int
	for _, r := range out {
		if strings.Contains(r.Rule, "git-cairn") || strings.Contains(r.Rule, "argv") {
			gitCairn++
		}
	}
	if gitCairn != 1 {
		t.Errorf("git-cairn/argv restatements kept = %d, want 1", gitCairn)
	}
}

// reworder answers each session with the same content in different words — the
// pairs are taken from what this repository's own history actually produced.
type reworder struct{}

func (r *reworder) Name() string    { return "reworder" }
func (r *reworder) Available() bool { return true }
func (r *reworder) Path() string    { return "/fake" }
func (r *reworder) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	if strings.Contains(req.Prompt, "ALPHA") {
		return &llm.Response{Engine: r.Name(), Text: `{
		  "rejected": [{"rule": "Snapshot/copy state.vscdb before every read",
		                "why": "the store measured 5.4GB and copying would dominate the commit hook",
		                "files": ["a.go"]}],
		  "invariants": [{"rule": "Large stores are opened read-only in place, not snapshotted in a hook",
		                  "why": "a copy inside the hook costs more than every other step together",
		                  "files": ["a.go"]}],
		  "claims": []
		}`}, nil
	}
	return &llm.Response{Engine: r.Name(), Text: `{
	  "rejected": [{"rule": "Snapshot/copy Cursor's state.vscdb before every read",
	                "why": "copying is too slow inside a hook",
	                "files": ["b.go"]}],
	  "invariants": [{"rule": "Large stores are opened read-only in place, never snapshotted in a hook",
	                  "why": "copying dominates the hook",
	                  "files": ["b.go"]}],
	  "claims": []
	}`}, nil
}

// Rules are never capped across a merge: a commit that really spans six sessions
// carries what six commits would have, and a rule dropped here is stored nowhere
// else.
func TestMergedRulesAreNotCapped(t *testing.T) {
	var sessions []*transcript.Session
	tags := []string{"A", "B", "C", "D", "E", "F"}
	files := make([]string, 0, len(tags))
	for _, tag := range tags {
		sessions = append(sessions, sessionSaying("sess-"+tag, tag, tag+".go"))
		files = append(files, tag+".go")
	}
	in := Input{Sessions: sessions, Diff: "diff --git a/A.go b/A.go\n", Files: files}
	res, err := Run(context.Background(), &distinctRules{}, in, Options{Budget: 30 * time.Second, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Extraction.Rejected); n != len(sessions) {
		t.Errorf("rejected = %d, want all %d kept", n, len(sessions))
	}
}

// distinctRules gives every session a different rejection, so nothing is folded
// away as a duplicate.
type distinctRules struct{ n atomic.Int32 }

func (d *distinctRules) Name() string    { return "distinct" }
func (d *distinctRules) Available() bool { return true }
func (d *distinctRules) Path() string    { return "/fake" }
func (d *distinctRules) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	i := d.n.Add(1)
	file := "A.go"
	for _, tag := range []string{"A", "B", "C", "D", "E", "F"} {
		if strings.Contains(req.Prompt, tag+" please do the thing") {
			file = tag + ".go"
		}
	}
	return &llm.Response{Engine: d.Name(), Text: fmt.Sprintf(`{
	  "rejected": [{"rule": "alternative approach %d to widget %d",
	                "why": "measured %d times slower on the same fixture",
	                "files": ["%s"]}],
	  "invariants": [],
	  "claims": []
	}`, i, i, i, file)}, nil
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
	return &llm.Response{Engine: b.Name(), Text: `{"rejected":[],"invariants":[],"claims":[]}`}, nil
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
	if res.Extraction == nil || len(res.Extraction.Rejected) != 1 ||
		!strings.Contains(res.Extraction.Rejected[0].Rule, "survivor") {
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
	return &llm.Response{Engine: f.Name(), Text: `{
	  "rejected": [{"rule": "the survivor rule", "why": "a reason that survived", "files": ["b.go"]}],
	  "invariants": [], "claims": []
	}`}, nil
}
