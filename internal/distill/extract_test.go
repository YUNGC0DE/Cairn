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
	if strings.Contains(req.System, "verification pass") {
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
	  "why": "%s wanted the %s thing done for the %s reason",
	  "rejected": [{"option": "%s option", "because": "%s reason"}],
	  "invariants": [{"rule": "the shared rule that must always hold", "scope": ["internal/**"]}],
	  "claims": ["%s claim"]
	}`, who, who, who, who, who, who)}, nil
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
		if !strings.Contains(strings.Join(res.Extraction.Why, " "), tag+" wanted") {
			t.Errorf("%s's intent was lost:\n%s", tag, res.Extraction.Why)
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
	// Two sessions that wanted different things are two entries, not one blended
	// paragraph.
	if n := len(res.Extraction.Why); n != 2 {
		t.Errorf("why entries = %d, want one per distinct intention: %q", n, res.Extraction.Why)
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
		t.Errorf("rejected = %+v, want the same option counted once", ex.Rejected)
	}
	// Of two wordings of one rejection, the one that explains more survives.
	if !strings.Contains(ex.Rejected[0].Because, "5.4GB") {
		t.Errorf("kept the thinner reason: %q", ex.Rejected[0].Because)
	}
	if len(ex.Invariants) != 1 {
		t.Errorf("invariants = %+v, want one", ex.Invariants)
	}
	// Of two wordings of one rule, the scoped one is served to fewer agents.
	if len(ex.Invariants[0].Scope) == 0 {
		t.Errorf("kept the unscoped wording, which is served to every file: %+v", ex.Invariants[0])
	}
	// The merge says what it folded away, so a thinner record is explainable.
	if !strings.Contains(strings.Join(res.Notes, " "), "worded twice") {
		t.Errorf("the fold was silent: %v", res.Notes)
	}
}

// Invariants about the same property often share an identifier but little else.
// Plain token overlap at dupThreshold misses them; the heavy-token check must
// still leave genuinely different rules ("first" vs "second") alone.
func TestInvariantRestatementsSharingAnIdentifierAreFolded(t *testing.T) {
	in := []Invariant{
		{Rule: "The installed binary name is git-cairn so git discovers it as a subcommand; displayed help must follow argv[0].", Scope: []string{"internal/cli/**"}},
		{Rule: "Ship the binary as git-cairn so git finds it as a subcommand, keep a cairn symlink, and derive the program name from argv[0].", Scope: []string{"Makefile", "internal/cli/**"}},
		{Rule: "Help and usage strings must take the program name from argv[0] when the binary base name is git-cairn.", Scope: []string{"internal/cli/**"}},
		{Rule: "The hook ownership marker must remain # cairn:managed-hook across renames.", Scope: []string{"internal/cli/init.go"}},
		{Rule: "first rule that must hold", Scope: []string{"a.go"}},
		{Rule: "second rule that must hold", Scope: []string{"b.go"}},
	}
	out := dedupInvariants(in)
	if len(out) != 4 {
		t.Fatalf("invariants = %d, want 4 (one git-cairn/argv rule, marker, first, second): %+v", len(out), out)
	}
	var gitCairn int
	for _, inv := range out {
		if strings.Contains(inv.Rule, "git-cairn") || strings.Contains(inv.Rule, "argv") {
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
		  "why": "The budget must be per session so a ten-session commit is not thinner than ten commits.",
		  "rejected": [{"option": "Snapshot/copy state.vscdb before every read",
		                "because": "the store measured 5.4GB and copying would dominate the commit hook"}],
		  "invariants": [{"rule": "Large SQLite stores are opened read-only in place, not snapshotted in a hook",
		                  "scope": ["internal/sqlitex/**"]}],
		  "claims": []
		}`}, nil
	}
	return &llm.Response{Engine: r.Name(), Text: `{
	  "why": "The budget must be per session so a ten-session commit is not thinner than ten separate commits.",
	  "rejected": [{"option": "Snapshot/copy Cursor's state.vscdb before every read",
	                "because": "copying is too slow inside a hook"}],
	  "invariants": [{"rule": "Large SQLite stores are opened read-only in place, never snapshotted in a hook"}],
	  "claims": []
	}`}, nil
}

// N sessions with N different asks keep N why entries. A count/byte cap on why
// would drop a real intention; only near-verbatim restatements are folded.
func TestDistinctWhysAreAllKept(t *testing.T) {
	var sessions []*transcript.Session
	for _, tag := range []string{"A", "B", "C", "D", "E", "F"} {
		sessions = append(sessions, sessionSaying("sess-"+tag, tag, tag+".go"))
	}
	in := Input{Sessions: sessions, Diff: "diff --git a/A.go b/A.go\n",
		Files: []string{"A.go", "B.go", "C.go", "D.go", "E.go", "F.go"}}
	res, err := Run(context.Background(), &distinctWhy{}, in, Options{Budget: 30 * time.Second, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Extraction.Why); n != len(sessions) {
		t.Errorf("why entries = %d, want all %d kept: %q", n, len(sessions), res.Extraction.Why)
	}
	if n := len(res.Extraction.Rejected); n != len(sessions) {
		t.Errorf("rejected = %d, want all %d kept", n, len(sessions))
	}
}

// distinctWhy gives every session a different intention and a different
// rejection, so nothing is folded away as a duplicate.
type distinctWhy struct{ n atomic.Int32 }

func (d *distinctWhy) Name() string    { return "distinct" }
func (d *distinctWhy) Available() bool { return true }
func (d *distinctWhy) Path() string    { return "/fake" }
func (d *distinctWhy) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	i := d.n.Add(1)
	return &llm.Response{Engine: d.Name(), Text: fmt.Sprintf(`{
	  "why": "Intention number %d, which concerns a completely unrelated corner of the project and shares no vocabulary with the others: widget %d needed rewiring.",
	  "rejected": [{"option": "alternative approach %d to widget %d", "because": "measured %d times slower on the same fixture"}],
	  "invariants": [],
	  "claims": []
	}`, i, i, i, i, i)}, nil
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
	return &llm.Response{Engine: b.Name(), Text: `{"why":"x","claims":[]}`}, nil
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
	if res.Extraction == nil || !strings.Contains(strings.Join(res.Extraction.Why, " "), "survivor") {
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
	return &llm.Response{Engine: f.Name(), Text: `{"why":"survivor intent","claims":[]}`}, nil
}
