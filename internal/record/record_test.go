package record

import (
	"strings"
	"testing"

	"github.com/YUNGC0DE/Cairn/internal/distill"
	"github.com/YUNGC0DE/Cairn/internal/gitx"
	"github.com/YUNGC0DE/Cairn/internal/testutil"
)

func sampleResult() *distill.Result {
	return &distill.Result{
		Confidence: distill.Verified,
		Extraction: &distill.Extraction{
			Intent:   "Rate limits on /login and /token stop credential stuffing observed in production logs.",
			Decision: "An in-memory token bucket is sufficient at current QPS.",
			Rejected: []distill.Rejected{{
				Option: "Redis-backed sliding window",
				Reason: "introduces an external datastore ruled out in #412",
			}},
			Invariants: []distill.InvariantCandidate{{
				Text:  "No new external datastores without an ADR",
				Scope: []string{"internal/**"},
			}},
			Claims: []string{"State is kept in memory"},
		},
	}
}

func sampleMeta() Meta {
	return Meta{
		Agent:       "claude-code/claude-opus-5",
		Sessions:    []string{"a3f2c91d"},
		Files:       []string{"internal/auth/limit.go", "internal/auth/handler.go"},
		Transcripts: []string{"sha256:9f2a1c"},
		Confidence:  distill.Verified,
	}
}

func TestComposeThenParseRoundTrip(t *testing.T) {
	repo := testutil.NewRepo(t)
	msg, err := Compose(repo.Root, "Add rate limiting to auth endpoints", Body(sampleResult()), sampleMeta())
	if err != nil {
		t.Fatal(err)
	}

	repo.Write("internal/auth/limit.go", "package auth\n")
	repo.Add(".")
	repo.Git("commit", "--no-verify", "-m", msg)

	commits, err := repo.Log([]string{"-n", "1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := Parse(repo.Root, commits[0])
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Has() {
		t.Fatal("record not recognised on its own output")
	}
	if rec.Subject != "Add rate limiting to auth endpoints" {
		t.Errorf("subject = %q", rec.Subject)
	}
	if !strings.Contains(rec.Intent, "credential stuffing") {
		t.Errorf("intent = %q", rec.Intent)
	}
	if len(rec.Rejected) != 1 || !strings.Contains(rec.Rejected[0], "Redis-backed sliding window") {
		t.Errorf("rejected = %v", rec.Rejected)
	}
	if len(rec.Invariants) != 1 || !strings.Contains(rec.Invariants[0], "ADR") {
		t.Errorf("invariants = %v", rec.Invariants)
	}
	if rec.Confidence != string(distill.Verified) {
		t.Errorf("confidence = %q", rec.Confidence)
	}
	if len(rec.Files) != 2 {
		t.Errorf("files = %v", rec.Files)
	}
	// The intent prose must not absorb the trailers.
	if strings.Contains(rec.Intent, "Cairn-") {
		t.Errorf("trailers leaked into intent: %q", rec.Intent)
	}
}

func TestTrailersAreParsedByGitItself(t *testing.T) {
	repo := testutil.NewRepo(t)
	msg, err := Compose(repo.Root, "Subject", Body(sampleResult()), sampleMeta())
	if err != nil {
		t.Fatal(err)
	}
	// If git's own parser does not see them as trailers, `git log --grep` and
	// third-party tooling will not either (spec §4.1).
	trailers, err := gitx.ParseTrailers(repo.Root, msg)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitx.Trailer(trailers, TrailerAgent); got != "claude-code/claude-opus-5" {
		t.Errorf("git interpret-trailers found Cairn-Agent = %q", got)
	}
	if got := gitx.Trailer(trailers, TrailerConfidence); got != "verified" {
		t.Errorf("Cairn-Confidence = %q", got)
	}
}

func TestComposePreservesTheAuthorsMessage(t *testing.T) {
	repo := testutil.NewRepo(t)
	existing := "Add rate limiting to auth endpoints\n\nA body the human wrote themselves.\nWith two lines."
	msg, err := Compose(repo.Root, existing, Body(sampleResult()), sampleMeta())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(msg, existing) {
		t.Errorf("cairn must append, never rewrite. got:\n%s", msg)
	}
}

func TestComposeWithoutProseStillWritesTrailers(t *testing.T) {
	repo := testutil.NewRepo(t)
	meta := sampleMeta()
	meta.Confidence = distill.MetadataOnly
	msg, err := Compose(repo.Root, "Subject only", "", meta)
	if err != nil {
		t.Fatal(err)
	}
	// This is the timeout degradation from spec §3.2: no prose, but the pointer
	// to the transcript and the file list survive.
	if !strings.Contains(msg, TrailerTranscript+": sha256:9f2a1c") {
		t.Errorf("metadata-only record lost its transcript pointer:\n%s", msg)
	}
	if !strings.Contains(msg, "metadata-only") {
		t.Errorf("confidence missing:\n%s", msg)
	}
}

func TestDisputedClaimIsNamedNotHidden(t *testing.T) {
	res := sampleResult()
	res.Confidence = distill.Disputed
	res.Verification = &distill.Verification{Claims: []distill.ClaimVerdict{{
		Index: 0, Status: distill.Contradicted, Claim: "State is kept in memory", Note: "diff imports redis",
	}}}
	body := Body(res)
	if !strings.Contains(body, "could not confirm") {
		t.Errorf("a contradicted claim must appear in the open:\n%s", body)
	}

	repo := testutil.NewRepo(t)
	meta := sampleMeta()
	meta.Confidence = distill.Disputed
	meta.Disputed = []string{"State is kept in memory"}
	msg, err := Compose(repo.Root, "Subject", body, meta)
	if err != nil {
		t.Fatal(err)
	}
	repo.Write("f.go", "package f\n")
	repo.Add(".")
	repo.Git("commit", "--no-verify", "-m", msg)
	commits, _ := repo.Log([]string{"-n", "1"}, nil)
	rec, err := Parse(repo.Root, commits[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Disputed) != 1 {
		t.Errorf("Cairn-Disputed did not round-trip: %+v", rec.Disputed)
	}
}

func TestBodyCapsRejectedList(t *testing.T) {
	res := sampleResult()
	for i := 0; i < maxRejectedRendered+5; i++ {
		res.Extraction.Rejected = append(res.Extraction.Rejected, distill.Rejected{
			Option: "option", Reason: "reason",
		})
	}
	body := Body(res)
	if n := strings.Count(body, rejectedPrefix); n > maxRejectedRendered+1 {
		t.Errorf("rendered %d rejected lines, cap is %d", n, maxRejectedRendered)
	}
	if !strings.Contains(body, "more, see `cairn rejected`") {
		t.Error("truncation must be visible, not silent")
	}
}

func TestBodyEmptyForNoRecord(t *testing.T) {
	if Body(nil) != "" {
		t.Error("nil result must render nothing")
	}
	if Body(&distill.Result{}) != "" {
		t.Error("a result with no extraction must render nothing")
	}
}

func TestWrapKeepsLinesWithinGitWidth(t *testing.T) {
	long := strings.Repeat("word ", 60)
	for _, line := range strings.Split(wrap(long), "\n") {
		if len(line) > wrapAt {
			t.Fatalf("line of %d chars exceeds %d: %q", len(line), wrapAt, line)
		}
	}
}

// Wrapped entries are the common case: a Rejected line long enough to wrap has
// its prefix only on the first line. Reading the continuation as prose is what
// makes `cairn why` look shuffled.
func TestParseReassemblesWrappedEntries(t *testing.T) {
	repo := testutil.NewRepo(t)
	res := &distill.Result{
		Confidence: distill.Verified,
		Extraction: &distill.Extraction{
			Intent: "Credential-stuffing traffic hit /login, so the endpoints needed a limiter.",
			Rejected: []distill.Rejected{{
				Option: "Redis-backed sliding window rate limiter",
				Reason: "would introduce a new external datastore, which ADR-412 disallows without an ADR, and is unnecessary precision for a single instance at 340 requests per second",
			}},
			Invariants: []distill.InvariantCandidate{{
				Text:  "No new external datastores such as Redis, Memcached or DynamoDB without an accepted ADR",
				Scope: []string{"internal/auth/**"},
			}},
		},
	}
	body := Body(res)
	if !strings.Contains(body, "\n") {
		t.Fatal("test needs a wrapped body")
	}
	msg, err := Compose(repo.Root, "Add rate limiting", body, sampleMeta())
	if err != nil {
		t.Fatal(err)
	}
	repo.Write("f.go", "package f\n")
	repo.Add(".")
	repo.Git("commit", "--no-verify", "-m", msg)
	commits, _ := repo.Log([]string{"-n", "1"}, nil)
	rec, err := Parse(repo.Root, commits[0])
	if err != nil {
		t.Fatal(err)
	}

	if len(rec.Rejected) != 1 {
		t.Fatalf("Rejected = %#v, want one reassembled entry", rec.Rejected)
	}
	if !strings.Contains(rec.Rejected[0], "unnecessary precision") {
		t.Errorf("the tail of the rejection was lost: %q", rec.Rejected[0])
	}
	if len(rec.Invariants) != 1 || !strings.Contains(rec.Invariants[0], "accepted ADR") {
		t.Errorf("Invariants = %#v", rec.Invariants)
	}
	// The continuations must not bleed into the prose.
	for _, leak := range []string{"unnecessary precision", "accepted ADR", "DynamoDB"} {
		if strings.Contains(rec.Intent, leak) {
			t.Errorf("intent absorbed a continuation line (%q): %q", leak, rec.Intent)
		}
	}
	if !strings.Contains(rec.Intent, "Credential-stuffing") {
		t.Errorf("intent lost: %q", rec.Intent)
	}
}

// Open items and the next step are v0.1 scope so that `cairn resume` (spec §3.5)
// can be assembled from records without a model. Records written without them
// could never gain them retroactively.
func TestOpenItemsAndNextStepRoundTrip(t *testing.T) {
	repo := testutil.NewRepo(t)
	res := sampleResult()
	res.Extraction.OpenItems = []string{
		"X-RateLimit-* response headers not implemented",
		"/register endpoint still unprotected because it needs a separate per-IP budget decided with ops",
	}
	res.Extraction.NextStep = "Add per-IP limits for /register"

	body := Body(res)
	if !strings.Contains(body, "Open: X-RateLimit-*") || !strings.Contains(body, "Next: Add per-IP") {
		t.Fatalf("Open:/Next: lines not rendered:\n%s", body)
	}

	msg, err := Compose(repo.Root, "Add rate limiting", body, sampleMeta())
	if err != nil {
		t.Fatal(err)
	}
	repo.Write("f.go", "package f\n")
	repo.Add(".")
	repo.Git("commit", "--no-verify", "-m", msg)
	commits, _ := repo.Log([]string{"-n", "1"}, nil)
	rec, err := Parse(repo.Root, commits[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Open) != 2 {
		t.Fatalf("Open = %#v, want 2 entries", rec.Open)
	}
	if !strings.Contains(rec.Open[1], "decided with ops") {
		t.Errorf("a wrapped open item lost its tail: %q", rec.Open[1])
	}
	if rec.Next != "Add per-IP limits for /register" {
		t.Errorf("Next = %q", rec.Next)
	}
	for _, leak := range []string{"X-RateLimit", "per-IP", "unprotected"} {
		if strings.Contains(rec.Intent, leak) {
			t.Errorf("intent absorbed %q: %q", leak, rec.Intent)
		}
	}
}
