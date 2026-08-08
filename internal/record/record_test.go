package record

import (
	"strings"
	"testing"

	"github.com/YUNGC0DE/git-cairn/internal/distill"
	"github.com/YUNGC0DE/git-cairn/internal/gitx"
	"github.com/YUNGC0DE/git-cairn/internal/testutil"
)

func sampleResult() *distill.Result {
	return &distill.Result{
		Confidence: distill.Verified,
		Extraction: &distill.Extraction{
			Rejected: []distill.Rule{{
				Rule:  "No Redis-backed sliding window here — keep the bucket in process",
				Why:   "introduces an external datastore ruled out in #412",
				Files: []string{"internal/auth/limit.go"},
			}},
			Invariants: []distill.Rule{{
				Rule:  "No new external datastores without an ADR",
				Why:   "#412 makes the deployment single-instance with nobody to operate one",
				Files: []string{"internal/auth/limit.go", "internal/auth/handler.go"},
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

// commitWith writes one commit carrying msg and parses the record back out of it,
// which is the only round trip that proves anything: git rewrites message bodies
// on the way in, so a format that survives a string comparison in memory can
// still come back mangled from `git log`.
func commitWith(t *testing.T, repo *testutil.Repo, msg string) *Record {
	t.Helper()
	repo.Write("f.go", "package f\n")
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
	return rec
}

func TestComposeThenParseRoundTrip(t *testing.T) {
	repo := testutil.NewRepo(t)
	msg, err := Compose(repo.Root, "Add rate limiting to auth endpoints", Body(sampleResult()), sampleMeta())
	if err != nil {
		t.Fatal(err)
	}
	rec := commitWith(t, repo, msg)

	if !rec.Has() {
		t.Fatal("record not recognised on its own output")
	}
	if rec.Subject != "Add rate limiting to auth endpoints" {
		t.Errorf("subject = %q", rec.Subject)
	}
	if len(rec.Rejected) != 1 || !strings.Contains(rec.Rejected[0].Rule, "Redis-backed sliding window") {
		t.Fatalf("rejected = %#v", rec.Rejected)
	}
	// The three parts stay apart. The rule is what an agent is shown on a file
	// touch, so a "why" folded into it would blow the character budget it exists
	// to respect, and files that did not survive make the entry undeliverable.
	if strings.Contains(rec.Rejected[0].Rule, "#412") {
		t.Errorf("the reason leaked into the rule: %q", rec.Rejected[0].Rule)
	}
	if !strings.Contains(rec.Rejected[0].Why, "#412") {
		t.Errorf("why = %q", rec.Rejected[0].Why)
	}
	if got := rec.Rejected[0].Files; len(got) != 1 || got[0] != "internal/auth/limit.go" {
		t.Errorf("files = %v", got)
	}
	if len(rec.Invariants) != 1 || !strings.Contains(rec.Invariants[0].Rule, "ADR") {
		t.Fatalf("invariants = %#v", rec.Invariants)
	}
	if len(rec.Invariants[0].Files) != 2 {
		t.Errorf("an invariant binding two files kept %v", rec.Invariants[0].Files)
	}
	if rec.Confidence != string(distill.Verified) {
		t.Errorf("confidence = %q", rec.Confidence)
	}
	if len(rec.Files) != 2 {
		t.Errorf("files = %v", rec.Files)
	}
}

// Which rules reach a reader is decided by the file it opened, and by nothing
// else. Before this, delivery was decided by which commits touched the file, so
// every rule in a commit was served to whoever opened any file it changed.
func TestRulesAreServedOnlyToTheFilesTheyBind(t *testing.T) {
	rec := &Record{
		Rejected: []Entry{
			{Rule: "no redis", Files: []string{"internal/auth/limit.go"}},
			{Rule: "no cron", Files: []string{"internal/jobs/run.go"}},
		},
		Invariants: []Entry{
			{Rule: "hooks never fail a commit", Files: []string{"internal/auth/limit.go"}},
			{Rule: "an unbound rule", Files: nil},
		},
	}
	rejected, invariants := rec.Rules("internal/auth/limit.go")
	if len(rejected) != 1 || rejected[0].Rule != "no redis" {
		t.Errorf("rejected = %#v", rejected)
	}
	if len(invariants) != 1 || invariants[0].Rule != "hooks never fail a commit" {
		t.Errorf("a rule binding no file must reach nobody: %#v", invariants)
	}
	if r, i := rec.Rules("README.md"); len(r)+len(i) != 0 {
		t.Errorf("an unrelated file was served %d rejected and %d invariants", len(r), len(i))
	}
	// A file that moved keeps its rules: the caller only asks this of commits
	// `git log --follow` already attributed to this one file.
	if r, _ := rec.Rules("internal/ratelimit/limit.go"); len(r) != 1 {
		t.Errorf("a moved file lost its rules: %#v", r)
	}
}

// The block's delimiters are what keep the record and git's trailers from being
// the same kind of line. Without them a short "invariant: …" is trailer-shaped
// and git folds it into the trailer block.
func TestRecordIsFencedByItsOwnTags(t *testing.T) {
	repo := testutil.NewRepo(t)
	msg, err := Compose(repo.Root, "Subject", Body(sampleResult()), sampleMeta())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, OpenTag) || !strings.Contains(msg, CloseTag) {
		t.Fatalf("record is not fenced:\n%s", msg)
	}
	if strings.Index(msg, CloseTag) > strings.Index(msg, TrailerAgent) {
		t.Errorf("the block must close before the trailers begin:\n%s", msg)
	}
	trailers, err := gitx.ParseTrailers(repo.Root, msg)
	if err != nil {
		t.Fatal(err)
	}
	// git must see exactly Cairn's trailers, and none of the record's own keys.
	for _, tr := range trailers {
		if !strings.HasPrefix(tr[0], "Cairn-") {
			t.Errorf("git read a record line as a trailer: %q: %q", tr[0], tr[1])
		}
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
	// This is the timeout degradation: no rules, but the pointer to the
	// transcript and the file list survive.
	if !strings.Contains(msg, TrailerTranscript+": sha256:9f2a1c") {
		t.Errorf("metadata-only record lost its transcript pointer:\n%s", msg)
	}
	if !strings.Contains(msg, "metadata-only") {
		t.Errorf("confidence missing:\n%s", msg)
	}
	// An empty block would tell a reader the reasoning was looked for and found.
	if strings.Contains(msg, OpenTag) {
		t.Errorf("an empty record must not be fenced as though it had content:\n%s", msg)
	}
}

func TestDisputedClaimRoundTrips(t *testing.T) {
	repo := testutil.NewRepo(t)
	meta := sampleMeta()
	meta.Confidence = distill.Disputed
	meta.Disputed = []string{"State is kept in memory"}
	msg, err := Compose(repo.Root, "Subject", Body(sampleResult()), meta)
	if err != nil {
		t.Fatal(err)
	}
	rec := commitWith(t, repo, msg)
	if len(rec.Disputed) != 1 {
		t.Errorf("the contradicted claim did not round-trip: %+v", rec.Disputed)
	}
}

func TestBodyEmptyForNoRecord(t *testing.T) {
	if Body(nil) != "" {
		t.Error("nil result must render nothing")
	}
	if Body(&distill.Result{}) != "" {
		t.Error("a result with no extraction must render nothing")
	}
	if Body(&distill.Result{Extraction: &distill.Extraction{}}) != "" {
		t.Error("an extraction with no content must render nothing, not an empty block")
	}
}

func TestWrapKeepsLinesWithinGitWidth(t *testing.T) {
	long := strings.Repeat("word ", 60)
	for _, line := range strings.Split(wrapIndent(long, "", "  "), "\n") {
		if len(line) > wrapAt {
			t.Fatalf("line of %d chars exceeds %d: %q", len(line), wrapAt, line)
		}
	}
}

// A wrapped "why" is the common case, and the indent is the whole grammar: a
// continuation line must never be readable as a new field or as a git trailer.
func TestParseReassemblesWrappedEntries(t *testing.T) {
	repo := testutil.NewRepo(t)
	res := &distill.Result{
		Confidence: distill.Verified,
		Extraction: &distill.Extraction{
			Rejected: []distill.Rule{{
				Rule:  "No Redis-backed sliding window limiter — the bucket stays in process",
				Why:   "it would introduce a new external datastore, which ADR-412 disallows, and is unnecessary precision for a single instance at 340 requests per second",
				Files: []string{"internal/auth/limit.go"},
			}},
			Invariants: []distill.Rule{{
				Rule:  "No new datastore — Redis, Memcached, DynamoDB — without an accepted ADR",
				Why:   "every one of them has to be operated, and this deployment has nobody to operate it",
				Files: []string{"internal/auth/handler.go"},
			}},
		},
	}
	body := Body(res)
	for _, line := range strings.Split(body, "\n") {
		// The file line is the one that never wraps, since a wrapped path list
		// cannot be read back unambiguously.
		if strings.Contains(line, fileKey) {
			continue
		}
		if len(line) > wrapAt {
			t.Fatalf("line of %d chars exceeds %d: %q", len(line), wrapAt, line)
		}
	}
	msg, err := Compose(repo.Root, "Add rate limiting", body, sampleMeta())
	if err != nil {
		t.Fatal(err)
	}
	rec := commitWith(t, repo, msg)

	if len(rec.Rejected) != 1 {
		t.Fatalf("Rejected = %#v, want one reassembled entry", rec.Rejected)
	}
	if !strings.Contains(rec.Rejected[0].Why, "unnecessary precision") {
		t.Errorf("the tail of the reason was lost: %q", rec.Rejected[0].Why)
	}
	// A continuation must not bleed into the field above or below it.
	if strings.Contains(rec.Rejected[0].Rule, "external datastore") {
		t.Errorf("the rule absorbed its own why: %q", rec.Rejected[0].Rule)
	}
	if len(rec.Rejected[0].Files) != 1 {
		t.Errorf("files = %v", rec.Rejected[0].Files)
	}
	if len(rec.Invariants) != 1 || !strings.Contains(rec.Invariants[0].Rule, "accepted ADR") {
		t.Fatalf("Invariants = %#v", rec.Invariants)
	}
	if strings.Contains(rec.Invariants[0].Rule, "nobody to operate") {
		t.Errorf("the invariant absorbed its own why: %q", rec.Invariants[0].Rule)
	}
}

// Several rules of the same kind stay separate entries: two decisions folded
// into one line are a decision lost.
func TestSeveralRulesOfOneKindRoundTrip(t *testing.T) {
	repo := testutil.NewRepo(t)
	res := sampleResult()
	res.Extraction.Rejected = append(res.Extraction.Rejected, distill.Rule{
		Rule:  "No per-request database lookup for the limit",
		Why:   "the limit is static and the lookup doubled p99 in the staging run",
		Files: []string{"internal/auth/limit.go"},
	})
	msg, err := Compose(repo.Root, "Add rate limiting", Body(res), sampleMeta())
	if err != nil {
		t.Fatal(err)
	}
	rec := commitWith(t, repo, msg)
	if len(rec.Rejected) != 2 {
		t.Fatalf("Rejected = %#v, want both", rec.Rejected)
	}
	if !strings.Contains(rec.Rejected[1].Rule, "per-request database lookup") {
		t.Errorf("the second rejection was mangled: %q", rec.Rejected[1].Rule)
	}
}
