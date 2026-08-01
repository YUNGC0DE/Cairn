package distill

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YUNGC0DE/Cairn/internal/llm"
	"github.com/YUNGC0DE/Cairn/internal/transcript"
)

// scripted returns canned replies in order, so a two-pass run can be driven
// without spawning an agent.
type scripted struct {
	replies []string
	errs    []error
	delay   time.Duration
	calls   int
	prompts []string
	systems []string
}

func (s *scripted) Name() string    { return "scripted" }
func (s *scripted) Available() bool { return true }
func (s *scripted) Path() string    { return "/scripted" }

func (s *scripted) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	i := s.calls
	s.calls++
	s.prompts = append(s.prompts, req.Prompt)
	s.systems = append(s.systems, req.System)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, llm.ErrTimeout
		}
	}
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	if i >= len(s.replies) {
		return nil, errors.New("scripted: no reply left")
	}
	return &llm.Response{Text: s.replies[i], Engine: s.Name(), Model: "test-model"}, nil
}

func input() Input {
	return Input{
		Sessions: []*transcript.Session{{
			Ref:   transcript.Ref{Agent: "claude-code", ID: "abcd1234"},
			Model: "claude-opus-5",
			Messages: []transcript.Message{
				{Role: transcript.RoleUser, Text: "stop credential stuffing on /login"},
				{Role: transcript.RoleAssistant, Text: "in-memory token bucket", Thinking: "redis adds a datastore"},
			},
		}},
		Diff:    "--- a/internal/auth/limit.go\n+++ b/internal/auth/limit.go\n+// token bucket\n",
		Files:   []string{"internal/auth/limit.go"},
		Subject: "Add rate limiting to auth endpoints",
	}
}

const goodExtraction = `{
  "subject": "",
  "intent": "Rate limits on /login and /token stop credential stuffing seen in production logs.",
  "decision": "An in-memory token bucket is enough at current QPS.",
  "rejected": [{"option": "Redis-backed sliding window", "reason": "introduces an external datastore ruled out in #412"}],
  "invariants": [{"text": "No new external datastores without an ADR", "scope": ["internal/**"]}],
  "claims": ["The limiter keeps state in memory, not Redis", "Peak traffic is 340 req/s"]
}`

func TestRunVerifiedPath(t *testing.T) {
	e := &scripted{replies: []string{goodExtraction,
		`{"claims":[{"index":0,"status":"supported","note":"limit.go"},{"index":1,"status":"supported","note":""}]}`}}
	res, err := Run(context.Background(), e, input(), Options{Budget: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if res.Confidence != Verified {
		t.Errorf("confidence = %s, want verified", res.Confidence)
	}
	if len(res.Extraction.Rejected) != 1 {
		t.Errorf("rejected = %+v", res.Extraction.Rejected)
	}
	if e.calls != 2 {
		t.Errorf("want two passes, got %d", e.calls)
	}
}

// The verification pass must not see the transcript. A verifier that has read
// the conversation will simply agree with it, which defeats the whole point
// (spec §2, P5).
func TestVerifyPassNeverSeesTheTranscript(t *testing.T) {
	e := &scripted{replies: []string{goodExtraction, `{"claims":[{"index":0,"status":"supported"}]}`}}
	if _, err := Run(context.Background(), e, input(), Options{Budget: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if len(e.prompts) != 2 {
		t.Fatalf("want two prompts, got %d", len(e.prompts))
	}
	if !strings.Contains(e.prompts[0], "credential stuffing") {
		t.Error("the extraction prompt must contain the session")
	}
	if strings.Contains(e.prompts[1], "credential stuffing") {
		t.Error("the verification prompt leaked the session — verification would be circular")
	}
	if !strings.Contains(e.prompts[1], "token bucket") {
		t.Error("the verification prompt must contain the diff")
	}
}

func TestContradictedClaimYieldsDisputed(t *testing.T) {
	e := &scripted{replies: []string{goodExtraction,
		`{"claims":[{"index":0,"status":"contradicted","note":"diff imports redis"},{"index":1,"status":"unverifiable","note":""}]}`}}
	res, err := Run(context.Background(), e, input(), Options{Budget: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if res.Confidence != Disputed {
		t.Errorf("confidence = %s, want disputed", res.Confidence)
	}
	d := res.DisputedClaims()
	if len(d) != 1 || !strings.Contains(d[0].Claim, "in memory") {
		t.Errorf("DisputedClaims = %+v", d)
	}
}

func TestUnverifiableClaimsYieldPartial(t *testing.T) {
	e := &scripted{replies: []string{goodExtraction,
		`{"claims":[{"index":0,"status":"supported"},{"index":1,"status":"unverifiable","note":"no load data in a diff"}]}`}}
	res, err := Run(context.Background(), e, input(), Options{Budget: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if res.Confidence != Partial {
		t.Errorf("confidence = %s, want partial", res.Confidence)
	}
}

func TestVerificationFailureLeavesExtractionStanding(t *testing.T) {
	e := &scripted{
		replies: []string{goodExtraction, ""},
		errs:    []error{nil, errors.New("engine exploded")},
	}
	res, err := Run(context.Background(), e, input(), Options{Budget: time.Minute})
	if err != nil {
		t.Fatalf("a failed verification must not fail the run: %v", err)
	}
	if res.Extraction == nil {
		t.Fatal("extraction was lost")
	}
	if res.Confidence != Unverified {
		t.Errorf("confidence = %s, want unverified", res.Confidence)
	}
	if len(res.Notes) == 0 {
		t.Error("the degradation must be explained in Notes, not swallowed")
	}
}

func TestExtractionFailureDegradesToMetadataOnly(t *testing.T) {
	e := &scripted{replies: []string{"I'm afraid I can't do that."}}
	res, err := Run(context.Background(), e, input(), Options{Budget: time.Minute})
	if err == nil {
		t.Fatal("unparseable extraction should be reported")
	}
	if res.Confidence != MetadataOnly {
		t.Errorf("confidence = %s, want metadata-only", res.Confidence)
	}
}

func TestBudgetExhaustionSkipsVerification(t *testing.T) {
	// Extraction consumes the whole budget; verification must be skipped rather
	// than left to run past the deadline and stall the commit.
	e := &scripted{replies: []string{goodExtraction, `{"claims":[]}`}, delay: 60 * time.Millisecond}
	res, err := Run(context.Background(), e, input(), Options{Budget: 70 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if e.calls != 1 {
		t.Errorf("verification ran with no budget left (%d calls)", e.calls)
	}
	if res.Confidence != Unverified {
		t.Errorf("confidence = %s, want unverified", res.Confidence)
	}
	joined := strings.Join(res.Notes, " ")
	if !strings.Contains(joined, "verification skipped") {
		t.Errorf("Notes = %v, want an explanation", res.Notes)
	}
}

func TestSanitizeDropsJunk(t *testing.T) {
	e := &scripted{replies: []string{`{
	  "subject": "Add rate limiting to auth endpoints",
	  "intent": "  Intent   with   loose  spacing ",
	  "decision": "N/A",
	  "rejected": [{"option": "Redis", "reason": ""}, {"option": "", "reason": "too slow"},
	               {"option": "Memcached", "reason": "same datastore problem"}],
	  "invariants": [{"text": "", "scope": []}, {"text": "Rule", "scope": ["*", "internal/**"]}],
	  "claims": ["ok", "  ", "none"]
	}`}}
	res, err := Run(context.Background(), e, input(), Options{Budget: time.Minute, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	ex := res.Extraction
	// A subject identical to the author's is noise.
	if ex.Subject != "" {
		t.Errorf("subject = %q, want empty (it repeats the author's)", ex.Subject)
	}
	if ex.Intent != "Intent with loose spacing" {
		t.Errorf("intent = %q", ex.Intent)
	}
	if ex.Decision != "" {
		t.Errorf("decision = %q, want empty (N/A is not a decision)", ex.Decision)
	}
	// Half a rejection invites the exact re-litigation the field exists to stop.
	if len(ex.Rejected) != 1 || ex.Rejected[0].Option != "Memcached" {
		t.Errorf("rejected = %+v, want only the complete entry", ex.Rejected)
	}
	if len(ex.Invariants) != 1 || len(ex.Invariants[0].Scope) != 1 {
		t.Errorf("invariants = %+v, want one rule scoped to internal/** ('*' carries no information)", ex.Invariants)
	}
	if len(ex.Claims) != 1 || ex.Claims[0] != "ok" {
		t.Errorf("claims = %+v", ex.Claims)
	}
}

func TestVerdictsWithInventedIndexAreDropped(t *testing.T) {
	e := &scripted{replies: []string{goodExtraction,
		`{"claims":[{"index":0,"status":"supported"},{"index":7,"status":"contradicted"},{"index":1,"status":"weird"}]}`}}
	res, err := Run(context.Background(), e, input(), Options{Budget: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Verification.Claims) != 2 {
		t.Fatalf("want 2 verdicts kept, got %+v", res.Verification.Claims)
	}
	// An unknown status must not be read as agreement.
	if res.Verification.Claims[1].Status != Unverifiable {
		t.Errorf("unknown status became %q, want unverifiable", res.Verification.Claims[1].Status)
	}
	if res.Confidence != Partial {
		t.Errorf("confidence = %s, want partial", res.Confidence)
	}
}

func TestDegradedSessionIsReported(t *testing.T) {
	in := input()
	in.Sessions[0].Degraded = true
	in.Sessions[0].DegradedReason = "sqlite3 missing"
	e := &scripted{replies: []string{goodExtraction}}
	res, err := Run(context.Background(), e, in, Options{Budget: time.Minute, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "sqlite3 missing") {
		t.Errorf("a degraded read must be surfaced: %v", res.Notes)
	}
}

func TestEmptySessionIsAnError(t *testing.T) {
	e := &scripted{replies: []string{goodExtraction}}
	_, err := Run(context.Background(), e, Input{Sessions: nil}, Options{Budget: time.Minute})
	if err == nil {
		t.Error("distilling nothing should be an error, not an empty record")
	}
	if e.calls != 0 {
		t.Error("no engine call should be made with nothing to distil")
	}
}

func TestOpenItemsAndNextStepAreExtractedAndCleaned(t *testing.T) {
	e := &scripted{replies: []string{`{
	  "intent": "Rate limits on /login stop credential stuffing.",
	  "decision": "",
	  "rejected": [],
	  "invariants": [],
	  "open_items": ["X-RateLimit headers not implemented", "  ", "none",
	                 "a", "b", "c", "d", "e"],
	  "next_step": "  Add per-IP limits for /register  ",
	  "claims": []
	}`}}
	res, err := Run(context.Background(), e, input(), Options{Budget: time.Minute, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	ex := res.Extraction
	if len(ex.OpenItems) != maxOpenItems {
		t.Errorf("OpenItems = %#v, want %d after dropping junk and capping", ex.OpenItems, maxOpenItems)
	}
	if ex.OpenItems[0] != "X-RateLimit headers not implemented" {
		t.Errorf("OpenItems[0] = %q", ex.OpenItems[0])
	}
	for _, o := range ex.OpenItems {
		if o == "none" || strings.TrimSpace(o) == "" {
			t.Errorf("junk survived: %q", o)
		}
	}
	if ex.NextStep != "Add per-IP limits for /register" {
		t.Errorf("NextStep = %q", ex.NextStep)
	}
}

func TestNextStepIsNotInvented(t *testing.T) {
	// The prompt tells the model to leave it empty; the schema must not turn a
	// placeholder into a plan.
	e := &scripted{replies: []string{`{"intent":"x","open_items":[],"next_step":"N/A","claims":[]}`}}
	res, err := Run(context.Background(), e, input(), Options{Budget: time.Minute, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Extraction.NextStep != "" {
		t.Errorf("NextStep = %q, want empty", res.Extraction.NextStep)
	}
}

// Extraction must get nearly the whole budget. A proportional split starved it on a
// cold prompt cache and collapsed real records to metadata-only.
func TestExtractionGetsMostOfTheBudget(t *testing.T) {
	e := &scripted{replies: []string{goodExtraction, verifyOK}, delay: 120 * time.Millisecond}
	// A budget that a proportional 65/35 split would have failed on: extraction
	// needs 120ms, and 65% of 150ms is only 97ms.
	res, err := Run(context.Background(), e, input(), Options{Budget: 150 * time.Millisecond})
	if err != nil {
		t.Fatalf("extraction should have fitted: %v", err)
	}
	if res.Extraction == nil {
		t.Fatal("extraction was starved by the budget split")
	}
	// Verification had no room left, which costs only the confidence label.
	if res.Confidence != Unverified {
		t.Logf("confidence = %s (verification happened to fit)", res.Confidence)
	}
}

const verifyOK = `{"claims":[{"index":0,"status":"supported"},{"index":1,"status":"supported"}]}`
