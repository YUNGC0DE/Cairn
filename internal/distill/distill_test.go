package distill

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YUNGC0DE/git-cairn/internal/llm"
	"github.com/YUNGC0DE/git-cairn/internal/transcript"
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
  "rejected": [{"rule": "No Redis-backed sliding window — the bucket stays in process",
                "why": "introduces an external datastore ruled out in #412",
                "files": ["internal/auth/limit.go"]}],
  "invariants": [{"rule": "No new external datastores without an ADR",
                  "why": "#412; the deployment is single-instance with nobody to operate one",
                  "files": ["internal/auth/limit.go"]}],
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
// the conversation will simply agree with it, which defeats the whole point.
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
	  "rejected": [{"rule": "Redis", "why": "", "files": ["internal/auth/limit.go"]},
	               {"rule": "", "why": "too slow", "files": ["internal/auth/limit.go"]},
	               {"rule": "Kafka", "why": "not chosen", "files": ["internal/auth/limit.go"]},
	               {"rule": "  Memcached   for the   counters ", "why": "same datastore problem",
	                "files": ["/Users/x/repo/internal/auth/limit.go"]}],
	  "invariants": [{"rule": "", "why": "x", "files": ["internal/auth/limit.go"]},
	                 {"rule": "Rate limiting was added to /login", "why": "y", "files": ["internal/auth/limit.go"]},
	                 {"rule": "Rule", "why": "a real reason", "files": ["docs/nothing-staged.md"]},
	                 {"rule": "Second rule", "why": "another real reason", "files": ["limit.go"]}],
	  "claims": ["ok", "  ", "none"]
	}`}}
	res, err := Run(context.Background(), e, input(), Options{Budget: time.Minute, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	ex := res.Extraction
	// Half a rule invites the exact re-litigation the record exists to stop, and
	// "not chosen" is not a reason. The survivor also has its spacing collapsed
	// and its absolute path bound back to the staged one.
	if len(ex.Rejected) != 1 || ex.Rejected[0].Rule != "Memcached for the counters" {
		t.Fatalf("rejected = %+v, want only the complete, reasoned entry", ex.Rejected)
	}
	if got := ex.Rejected[0].Files; len(got) != 1 || got[0] != "internal/auth/limit.go" {
		t.Errorf("files = %v, want the staged spelling of the path", got)
	}
	// "was added to /login" reports this commit; it constrains no future work.
	// A rule bound to a file this commit does not stage can never be served, so
	// it is dropped rather than written. A bare base name still binds.
	if len(ex.Invariants) != 1 || ex.Invariants[0].Rule != "Second rule" {
		t.Fatalf("invariants = %+v", ex.Invariants)
	}
	if got := ex.Invariants[0].Files; len(got) != 1 || got[0] != "internal/auth/limit.go" {
		t.Errorf("files = %v", got)
	}
	if len(ex.Claims) != 1 || ex.Claims[0] != "ok" {
		t.Errorf("claims = %+v", ex.Claims)
	}
}

// A commit that stages exactly one file leaves no room for ambiguity, so a rule
// that forgot to name it is bound rather than discarded.
func TestSingleStagedFileIsAssumed(t *testing.T) {
	e := &scripted{replies: []string{`{"rejected":[{"rule":"No Redis","why":"an external datastore #412 rules out","files":[]}],"invariants":[],"claims":[]}`}}
	res, err := Run(context.Background(), e, input(), Options{Budget: time.Minute, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Extraction.Rejected) != 1 {
		t.Fatalf("rejected = %+v", res.Extraction.Rejected)
	}
	if got := res.Extraction.Rejected[0].Files; len(got) != 1 || got[0] != "internal/auth/limit.go" {
		t.Errorf("files = %v, want the only staged file", got)
	}
}

// The prompt asks for at most three rejections and two invariants. A model that
// ignores that ignores it by a wide margin, so the cap is enforced here too.
func TestPerSessionCapsAreEnforced(t *testing.T) {
	e := &scripted{replies: []string{`{
	  "rejected": [{"rule":"alpha","why":"reason one","files":["internal/auth/limit.go"]},
	               {"rule":"beta","why":"reason two","files":["internal/auth/limit.go"]},
	               {"rule":"gamma","why":"reason three","files":["internal/auth/limit.go"]},
	               {"rule":"delta","why":"reason four","files":["internal/auth/limit.go"]}],
	  "invariants": [{"rule":"first rule that must hold","why":"reason one","files":["internal/auth/limit.go"]},
	                 {"rule":"second rule that must hold","why":"reason two","files":["internal/auth/limit.go"]},
	                 {"rule":"third rule that must hold","why":"reason three","files":["internal/auth/limit.go"]}],
	  "claims": []
	}`}}
	res, err := Run(context.Background(), e, input(), Options{Budget: time.Minute, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Extraction.Rejected) != maxRejectedPerSession {
		t.Errorf("rejected = %d, want %d", len(res.Extraction.Rejected), maxRejectedPerSession)
	}
	if len(res.Extraction.Invariants) != maxInvariantsPerSession {
		t.Errorf("invariants = %d, want %d", len(res.Extraction.Invariants), maxInvariantsPerSession)
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
	in.Sessions[0].DegradedReason = "transcript unreadable"
	e := &scripted{replies: []string{goodExtraction}}
	res, err := Run(context.Background(), e, in, Options{Budget: time.Minute, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "transcript unreadable") {
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
