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
			Why: []string{"Credential stuffing hit /login and /token, and the author wanted repeated attempts from one client stopped without adding infrastructure."},
			Rejected: []distill.Rejected{{
				Option:  "Redis-backed sliding window",
				Because: "introduces an external datastore ruled out in #412",
			}},
			Invariants: []distill.Invariant{{
				Rule:  "No new external datastores without an ADR",
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
	if len(rec.Why) != 1 || !strings.Contains(rec.Why[0], "credential stuffing") &&
		!strings.Contains(rec.Why[0], "Credential stuffing") {
		t.Errorf("why = %q", rec.Why)
	}
	if len(rec.Rejected) != 1 || !strings.Contains(rec.Rejected[0], "Redis-backed sliding window") {
		t.Errorf("rejected = %v", rec.Rejected)
	}
	// The reason travels with the option: without it a rejection reads as a bare
	// prohibition and gets re-opened by the first agent that disagrees.
	if !strings.Contains(rec.Rejected[0], "#412") {
		t.Errorf("the reason was dropped from the rejection: %q", rec.Rejected[0])
	}
	if len(rec.Invariants) != 1 || !strings.Contains(rec.Invariants[0], "ADR") {
		t.Errorf("invariants = %v", rec.Invariants)
	}
	if !strings.Contains(rec.Invariants[0], "internal/**") {
		t.Errorf("the scope was dropped from the invariant: %q", rec.Invariants[0])
	}
	if rec.Confidence != string(distill.Verified) {
		t.Errorf("confidence = %q", rec.Confidence)
	}
	if len(rec.Files) != 2 {
		t.Errorf("files = %v", rec.Files)
	}
	// Nothing outside the block belongs to the record.
	for _, w := range rec.Why {
		if strings.Contains(w, "Cairn-") {
			t.Errorf("trailers leaked into why: %q", w)
		}
	}
}

// The block's delimiters are what keep the record and git's trailers from being
// the same kind of line. Before them a short "Invariant: …" was trailer-shaped
// and git folded it into the trailer block.
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
	// This is the timeout degradation: no prose, but the pointer
	// to the transcript and the file list survive.
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

func TestDisputedClaimIsNamedNotHidden(t *testing.T) {
	res := sampleResult()
	res.Confidence = distill.Disputed
	res.Verification = &distill.Verification{Claims: []distill.ClaimVerdict{{
		Index: 0, Status: distill.Contradicted, Claim: "State is kept in memory", Note: "diff imports redis",
	}}}
	body := Body(res)
	if !strings.Contains(body, unconfirmedKey) {
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
	rec := commitWith(t, repo, msg)
	if len(rec.Disputed) != 1 {
		t.Errorf("the contradicted claim did not round-trip: %+v", rec.Disputed)
	}
}

func TestBodyCapsRejectedList(t *testing.T) {
	res := sampleResult()
	for i := 0; i < maxRejectedRendered+5; i++ {
		res.Extraction.Rejected = append(res.Extraction.Rejected, distill.Rejected{
			Option: "option", Because: "reason",
		})
	}
	body := Body(res)
	if n := strings.Count(body, rejectedKey); n > maxRejectedRendered+1 {
		t.Errorf("rendered %d rejected lines, cap is %d", n, maxRejectedRendered)
	}
	if !strings.Contains(body, "more not recorded") {
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
	if Body(&distill.Result{Extraction: &distill.Extraction{}}) != "" {
		t.Error("an extraction with no content must render nothing, not an empty block")
	}
}

func TestWrapKeepsLinesWithinGitWidth(t *testing.T) {
	long := strings.Repeat("word ", 60)
	for _, line := range strings.Split(wrapIndent(long, "  "), "\n") {
		if len(line) > wrapAt {
			t.Fatalf("line of %d chars exceeds %d: %q", len(line), wrapAt, line)
		}
	}
}

// Wrapped entries are the common case, and the indent is the whole grammar: a
// continuation line must never be readable as a new field or as a git trailer.
func TestParseReassemblesWrappedEntries(t *testing.T) {
	repo := testutil.NewRepo(t)
	res := &distill.Result{
		Confidence: distill.Verified,
		Extraction: &distill.Extraction{
			Why: []string{"Credential-stuffing traffic hit /login overnight, so the author wanted the endpoints limited before the next attempt wave."},
			Rejected: []distill.Rejected{{
				Option:  "Redis-backed sliding window rate limiter",
				Because: "would introduce a new external datastore, which ADR-412 disallows, and is unnecessary precision for a single instance at 340 requests per second",
			}},
			Invariants: []distill.Invariant{{
				Rule:  "No new external datastores such as Redis, Memcached or DynamoDB without an accepted ADR",
				Scope: []string{"internal/auth/**"},
			}},
		},
	}
	body := Body(res)
	for _, line := range strings.Split(body, "\n") {
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
	if !strings.Contains(rec.Rejected[0], "unnecessary precision") {
		t.Errorf("the tail of the rejection was lost: %q", rec.Rejected[0])
	}
	if len(rec.Invariants) != 1 || !strings.Contains(rec.Invariants[0], "accepted ADR") {
		t.Errorf("Invariants = %#v", rec.Invariants)
	}
	if len(rec.Why) != 1 {
		t.Fatalf("Why = %#v, want one entry", rec.Why)
	}
	// A continuation must not bleed into the field above or below it.
	for _, leak := range []string{"unnecessary precision", "accepted ADR", "DynamoDB"} {
		if strings.Contains(rec.Why[0], leak) {
			t.Errorf("why absorbed a continuation line (%q): %q", leak, rec.Why[0])
		}
	}
	if !strings.Contains(rec.Why[0], "Credential-stuffing") {
		t.Errorf("why lost: %q", rec.Why[0])
	}
	if !strings.Contains(rec.Why[0], "attempt wave") {
		t.Errorf("a wrapped why lost its tail: %q", rec.Why[0])
	}
}

// Several sessions behind one commit each state their own purpose, and each stays
// its own entry: two sessions that wanted different things did not want one
// blended thing.
func TestMultipleWhyEntriesRoundTrip(t *testing.T) {
	repo := testutil.NewRepo(t)
	res := sampleResult()
	res.Extraction.Why = []string{
		"The author wanted credential stuffing on /login stopped without new infrastructure.",
		"A later session wanted the limiter's counters exposed so the on-call could see them.",
	}
	msg, err := Compose(repo.Root, "Add rate limiting", Body(res), sampleMeta())
	if err != nil {
		t.Fatal(err)
	}
	rec := commitWith(t, repo, msg)
	if len(rec.Why) != 2 {
		t.Fatalf("Why = %#v, want both intentions", rec.Why)
	}
	if !strings.Contains(rec.Why[1], "on-call") {
		t.Errorf("the second intention was mangled: %q", rec.Why[1])
	}
}

// Records written before the <git-cairn> block are the reason the tool exists in
// the repositories that already use it. They must keep reading, and the fields
// that were cut must be dropped rather than dumped into the prose.
func TestLegacyRecordsStillParse(t *testing.T) {
	repo := testutil.NewRepo(t)
	legacy := `Ship reactive path recall and wire it into harness init.

Agents get file history on first touch via harness hooks.

Rejected: Cursor beforeReadFile as the injection event — Its response
cannot reach the model; only preToolUse can deliver additional_context.

Invariant: Reactive hooks must never fail the agent tool call: on error or
nothing to say, exit quietly with no output. (internal/cli/**)

Open: A Cursor skill packaging the commit rule has not been written yet.
Next: Run doctor to confirm both engines answer.

Cairn-Agent: claude-code/opus-5
Cairn-Confidence: partial
`
	rec := commitWith(t, repo, legacy)
	if !rec.Has() {
		t.Fatal("a legacy record stopped being recognised")
	}
	if len(rec.Rejected) != 1 || !strings.Contains(rec.Rejected[0], "additional_context") {
		t.Errorf("legacy rejected = %#v", rec.Rejected)
	}
	if len(rec.Invariants) != 1 || !strings.Contains(rec.Invariants[0], "exit quietly") {
		t.Errorf("legacy invariants = %#v", rec.Invariants)
	}
	if len(rec.Why) != 1 || !strings.Contains(rec.Why[0], "file history on first touch") {
		t.Errorf("legacy prose = %#v", rec.Why)
	}
	// Open:/Next: are recognised only so they can be discarded — not re-read as
	// part of the reasoning.
	for _, gone := range []string{"Cursor skill", "Run doctor"} {
		if strings.Contains(strings.Join(rec.Why, " "), gone) {
			t.Errorf("a dropped field leaked into the prose: %q", rec.Why)
		}
	}
}
