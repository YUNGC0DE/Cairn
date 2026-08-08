// Package distill turns a session tail plus a staged diff into a commit record.
//
// Two passes. The first extracts the record. The second re-reads the claims
// against the diff *without* the transcript and labels each one. The second pass
// is not a nicety: a record is written once and then read by every future agent,
// so an unchecked confabulation becomes permanent canon.
package distill

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/YUNGC0DE/git-cairn/internal/llm"
	"github.com/YUNGC0DE/git-cairn/internal/transcript"
)

// Confidence labels how well a record survived verification. It is written into
// the Cairn-Confidence trailer.
type Confidence string

const (
	// Verified means every claim is supported by the diff.
	Verified Confidence = "verified"
	// Partial means no claim is contradicted, but some cannot be settled by the
	// diff alone (statements about production, load, or intent).
	Partial Confidence = "partial"
	// Unverified means the verification pass did not run — out of time budget, or
	// no engine available. The record is prose only, believe it accordingly.
	Unverified Confidence = "unverified"
	// Disputed means the diff contradicts at least one claim. It is its own level
	// rather than a shade of "partial": silently downgrading a contradiction would
	// hide exactly the failure the verify pass exists to catch.
	Disputed Confidence = "disputed"
	// MetadataOnly means distillation produced no prose at all; the record is
	// trailers only.
	MetadataOnly Confidence = "metadata-only"
)

// Rule is one file-level rule a session established: an option turned down, or a
// property the code has to keep. Three parts, and each has exactly one job.
//
// Rule is the sentence a later agent is shown the moment it opens the file, so it
// is short and carries no justification. That is not taste: both harnesses drop a
// hook's injection past 10 000 characters, and fifty commits of a file's history
// have to fit inside that, which leaves about two lines per commit.
//
// Why is the justification, and it stays in the commit. An agent reaches it only
// after the rule has told it there is something worth reaching for — one `git
// show` away, at no cost to anyone who does not need it.
//
// Files is what the rule binds. The record is file-level throughout: recall is
// `git log -- <path>`, so a rule naming a file this commit does not touch would
// never be served to anybody, and is dropped rather than written.
type Rule struct {
	Rule  string   `json:"rule"`
	Why   string   `json:"why"`
	Files []string `json:"files"`
}

// Extraction is the schema the first pass must produce.
//
// It used to open with a "why" paragraph about the commit as a whole. That field
// was the bulk of every record and none of its usefulness: it explained a change
// the reader already has the diff for, while the rules — the things a later agent
// cannot re-derive — were what the reactive channel actually needed to deliver.
// Justification did not disappear, it moved inside each rule, where it explains
// the one thing a reader might otherwise re-litigate.
type Extraction struct {
	Rejected   []Rule   `json:"rejected"`
	Invariants []Rule   `json:"invariants"`
	Claims     []string `json:"claims"`
}

// ClaimStatus is a verification verdict for one claim.
type ClaimStatus string

const (
	// Supported: the diff shows the claim to be true.
	Supported ClaimStatus = "supported"
	// Contradicted: the diff shows the opposite.
	Contradicted ClaimStatus = "contradicted"
	// Unverifiable: the diff neither shows nor denies it. Kept and marked rather
	// than dropped — an unverifiable claim is often the most
	// interesting part of the record.
	Unverifiable ClaimStatus = "unverifiable"
)

// ClaimVerdict is one verified claim.
type ClaimVerdict struct {
	Index  int         `json:"index"`
	Status ClaimStatus `json:"status"`
	Note   string      `json:"note"`
	Claim  string      `json:"-"`
}

// Verification is the schema the second pass must produce.
type Verification struct {
	Claims []ClaimVerdict `json:"claims"`
}

// Input is everything distillation reads.
type Input struct {
	Sessions []*transcript.Session
	Diff     string
	// DiffTruncated marks that Diff is a prefix; claims about untouched parts of
	// the change would be unfair to verify.
	DiffTruncated bool
	Files         []string
	// Subject is the author's existing subject line, if any.
	Subject string
}

// Result is the distilled record plus how it was produced.
type Result struct {
	Extraction   *Extraction
	Verification *Verification
	Confidence   Confidence
	Engine       string
	Model        string
	Elapsed      time.Duration
	// Notes record every degradation, so the caller can tell the user why a
	// record is thinner than expected instead of leaving them guessing.
	Notes []string
}

// Options tune a distillation run.
type Options struct {
	// Budget is the wall-clock allowance for one session's extraction (and the
	// commit's verification reserve). Like PromptBudget, the unit is one session:
	// a commit with N relevant sessions may wait up to N×Budget, so committing
	// ten sessions at once does not starve each of the time a solo commit would
	// have had. When the budget runs out we degrade, we do not wait.
	Budget time.Duration
	// Model overrides the engine default for extraction.
	Model string
	// VerifyModel overrides the model used for verification only, for anyone who
	// wants the checking pass on something other than the extraction model.
	VerifyModel string
	// Effort is the reasoning effort both passes run at, where the engine has a
	// knob for it. Empty lets the engine choose; `claude` defaults to low, which
	// is the single biggest lever on how long a commit waits.
	Effort string
	// PromptBudget caps the rendered session text in bytes, per session.
	PromptBudget int
	// SkipVerify disables the second pass (used by `cairn audit --no-verify`).
	SkipVerify bool
	// Verbose streams progress to the writer, if set.
	Trace func(format string, args ...any)
}

// DefaultBudget is the wall-clock allowance for one session.
//
// Measured on real hardware, extraction costs ~11 s and verification ~11 s, so the
// 12 s the original design assumed would mean the verification pass — the only thing
// standing between a confabulation and permanent canon — never runs, and every
// record ships "unverified". 60 s fits both with comfortable headroom on a cold
// prompt cache. A record is written once and read for years, so correctness outranks
// the extra seconds; `cairn.timeout 12` is there for anyone who prefers the faster
// commit. Commits with several sessions scale this by the session count.
const DefaultBudget = 60 * time.Second

// minVerifyBudget is the least time worth starting a verification pass with, and
// the only slice held back from extraction.
//
// The split is deliberately lopsided rather than proportional. Extraction alone
// still yields a useful record; verification alone yields nothing, so a timeout in
// extraction is the worst outcome available and must be made unlikely. A fixed
// 65/35 split caused exactly that: on a cold prompt cache a real extraction ran
// past 22 s and the whole record collapsed to metadata-only. Now extraction may use
// everything but this reserve, and verification is opportunistic — it runs with
// whatever extraction did not need, and failing it costs only the confidence label.
const minVerifyBudget = 4 * time.Second

// Run distills one commit's worth of context.
func Run(ctx context.Context, engine llm.Engine, in Input, opts Options) (*Result, error) {
	if opts.Budget <= 0 {
		opts.Budget = DefaultBudget
	}
	if opts.PromptBudget <= 0 {
		opts.PromptBudget = transcript.DefaultBudget
	}
	trace := opts.Trace
	if trace == nil {
		trace = func(string, ...any) {}
	}

	res := &Result{Confidence: MetadataOnly, Engine: engine.Name(), Model: opts.Model}
	start := time.Now()

	// Which sessions actually produced this commit is decided against the staged
	// file list, not guessed from the transcript: cairn is holding the diff.
	sessions, skipped := transcript.Relevant(in.Sessions, in.Files)
	if skipped > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"%d of %d sessions wrote none of the staged files and were left out",
			skipped, len(in.Sessions)))
	}

	payloads := transcript.CompactEach(sessions, opts.PromptBudget)
	empty := 0
	for _, p := range payloads {
		res.Notes = append(res.Notes, p.Notes...)
		if strings.TrimSpace(p.Body) == "" && strings.TrimSpace(p.Requests) == "" {
			empty++
		}
	}
	if empty == len(payloads) {
		return res, fmt.Errorf("distill: nothing to distill from %d session(s)", len(sessions))
	}

	// Time budget is per session, matching PromptBudget: N sessions get N× the
	// configured allowance so a batch commit is not thinner than N solo ones.
	n := len(payloads)
	if n < 1 {
		n = 1
	}
	totalBudget := opts.Budget * time.Duration(n)
	deadline := start.Add(totalBudget)

	for _, s := range in.Sessions {
		if s.Degraded {
			res.Notes = append(res.Notes, fmt.Sprintf("%s session read degraded: %s", s.Agent, s.DegradedReason))
		}
	}
	if in.DiffTruncated {
		// Say this out loud: a truncated diff makes the verification pass mark
		// claims about the omitted part unverifiable, and a reader who does not know
		// why will read the lower confidence as a model failure.
		res.Notes = append(res.Notes,
			"diff was truncated, so claims about the omitted part cannot be verified "+
				"(raise cairn.diffBudget)")
	}

	// Each session gets the same extract slice a solo commit would: the verify
	// reserve is held once for the commit, not once per session.
	perSession := opts.Budget - minVerifyBudget
	if perSession <= 0 {
		perSession = opts.Budget
	}
	extractWall := totalBudget - minVerifyBudget
	if extractWall <= 0 {
		extractWall = totalBudget
	}
	trace("extract: %d session(s) on %s, %s each (%s wall)",
		len(payloads), engine.Name(), perSession.Round(time.Millisecond), extractWall.Round(time.Millisecond))
	exs, xres := extractAll(ctx, engine, sessions, payloads, in, opts, perSession, extractWall, trace)
	res.Elapsed = time.Since(start)
	res.Notes = append(res.Notes, xres.notes...)
	if xres.model != "" {
		res.Model = xres.model
	}
	// Which engine actually answered, not which one was asked first: with a
	// fallback chain those differ, and the record should name the one that ran.
	if xres.engine != "" {
		res.Engine = xres.engine
	}
	if len(exs) == 0 {
		return res, xres.err
	}

	// Naive concatenation, deliberately. Merging several sessions into one
	// narrative is a second model call and a second chance to invent; stacking
	// what each session said keeps every intent attributable to the work that
	// produced it, exactly as separate commits would have.
	ex, mergeNotes := merge(exs)
	res.Extraction = ex
	res.Notes = append(res.Notes, mergeNotes...)
	res.Confidence = Unverified

	if opts.SkipVerify {
		res.Notes = append(res.Notes, "verification skipped by request")
		return res, nil
	}
	if len(ex.Claims) == 0 {
		res.Notes = append(res.Notes, "no checkable claims to verify")
		return res, nil
	}
	remaining := time.Until(deadline)
	if remaining < minVerifyBudget {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"verification skipped: %s left of a %s budget (%d×%s)",
			remaining.Round(time.Millisecond), totalBudget, n, opts.Budget))
		return res, nil
	}

	// An empty verify model is not a missing setting: each engine has its own
	// idea of "the small fast one", and only the engine knows the ids it accepts.
	verifyModel := opts.VerifyModel
	trace("verify: %d claims, model %s, budget %s",
		len(ex.Claims), orDefault(verifyModel, "engine default"), remaining.Round(time.Millisecond))
	vresp, err := engine.Complete(ctx, llm.Request{
		System: verifySystem,
		Prompt: verifyPrompt(ex.Claims, in),
		Model:  verifyModel,
		Effort: opts.Effort,
		Budget: remaining,
	})
	res.Elapsed = time.Since(start)
	if err != nil {
		res.Notes = append(res.Notes, "verification failed: "+err.Error())
		return res, nil // extraction still stands; the record is just unverified
	}
	res.Notes = append(res.Notes, vresp.Notes...)
	var v Verification
	if err := llm.ExtractJSON(vresp.Text, &v); err != nil {
		res.Notes = append(res.Notes, "verification returned unusable JSON: "+err.Error())
		return res, nil
	}
	attachClaims(&v, ex.Claims)
	res.Verification = &v
	res.Confidence = score(&v)
	trace("verify: done in %s on %s/%s → %s", vresp.Elapsed.Round(time.Millisecond),
		vresp.Engine, orDefault(vresp.Model, "engine default"), res.Confidence)
	return res, nil
}

// score maps verdicts to a confidence label.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func score(v *Verification) Confidence {
	if len(v.Claims) == 0 {
		return Unverified
	}
	supported, contradicted := 0, 0
	for _, c := range v.Claims {
		switch c.Status {
		case Contradicted:
			contradicted++
		case Supported:
			supported++
		}
	}
	switch {
	case contradicted > 0:
		return Disputed
	case supported == len(v.Claims):
		return Verified
	default:
		return Partial
	}
}

// attachClaims pairs verdicts back to their claim text, dropping verdicts whose
// index the model invented.
func attachClaims(v *Verification, claims []string) {
	out := v.Claims[:0]
	for _, c := range v.Claims {
		if c.Index < 0 || c.Index >= len(claims) {
			continue
		}
		c.Claim = claims[c.Index]
		switch c.Status {
		case Supported, Contradicted, Unverifiable:
		default:
			c.Status = Unverifiable
		}
		out = append(out, c)
	}
	v.Claims = out
}

// Disputed returns the claims the diff contradicts.
func (r *Result) DisputedClaims() []ClaimVerdict {
	if r.Verification == nil {
		return nil
	}
	var out []ClaimVerdict
	for _, c := range r.Verification.Claims {
		if c.Status == Contradicted {
			out = append(out, c)
		}
	}
	return out
}

// sanitize enforces what the prompt asks for but a model may not deliver.
//
// The split of labour is deliberate: the prompt decides what is worth saying,
// this decides what is well-formed. Everything here is a check a reader could
// run without knowing the session — length, emptiness, a rejection missing its
// reason, a rule phrased as a report of this very commit. Semantic judgement
// stays in the prompt, because a regex that guesses at meaning silently deletes
// the one rejection that mattered.
func sanitize(ex *Extraction, in Input) {
	ex.Rejected = cleanRules(ex.Rejected, in.Files, maxRejectedPerSession)
	ex.Invariants = cleanRules(ex.Invariants, in.Files, maxInvariantsPerSession)

	claims := ex.Claims[:0]
	for _, c := range ex.Claims {
		if c = clean(c, maxClaim); c != "" {
			claims = append(claims, c)
		}
	}
	if len(claims) > maxClaims {
		claims = claims[:maxClaims]
	}
	ex.Claims = dedupStrings(claims)
}

// cleanRules enforces what the prompt asks of one list of rules.
func cleanRules(in []Rule, staged []string, max int) []Rule {
	out := in[:0]
	for _, r := range in {
		r.Rule = clean(r.Rule, maxRule)
		r.Why = clean(r.Why, maxWhy)
		switch {
		case r.Rule == "" || r.Why == "":
			// Half a rule is worse than none: with no reason it reads as a bare
			// prohibition, and the next agent either obeys it blindly or re-opens it.
			continue
		case emptyReason(r.Why):
			// "Not chosen" is not a reason. The prompt says so; models still do it.
			continue
		case sameThing(r.Rule, r.Why):
			continue // the reason merely repeats the rule
		case describesThisChange(r.Rule):
			continue // a report of this commit, not a rule for the next one
		}
		r.Files = bindFiles(r.Files, staged)
		if len(r.Files) == 0 {
			continue
		}
		out = append(out, r)
	}
	out = dedupRules(out)
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// bindFiles resolves the paths a rule names against the files the commit stages.
//
// A model names a file the way it saw it — absolute, or relative to somewhere
// else — while git records repo-relative paths, and recall runs `git log --
// <path>`. So a rule that cannot be tied to a staged path is not "unscoped", it
// is undeliverable: no reader will ever open a file it matches. It is dropped,
// which costs one rule, rather than written, which costs every future reader the
// bytes and teaches them nothing.
//
// The single-file commit is the exception worth handling: there the model had one
// possible answer and leaving the field empty is a formatting slip, not an
// ambiguity. That only applies to an empty list — a rule that names a file and
// names the wrong one has said something, and overriding it with a guess would
// bind it to code it was never about.
func bindFiles(named, staged []string) []string {
	if len(named) == 0 && len(staged) == 1 {
		return []string{staged[0]}
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range named {
		m := matchStaged(n, staged)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) > maxFiles {
		out = out[:maxFiles]
	}
	return out
}

// matchStaged returns the staged path a named one refers to, or "".
//
// Matching is on separator boundaries in both directions — the name may be an
// absolute path holding the staged one, or a repo-relative path the model wrote
// from a subdirectory — and falls back to the base name, which is unambiguous in
// every commit that does not stage two files with the same name.
func matchStaged(named string, staged []string) string {
	n := strings.TrimSpace(named)
	if n == "" {
		return ""
	}
	n = filepath.ToSlash(filepath.Clean(n))
	base := path.Base(n)
	var byBase string
	for _, s := range staged {
		s = filepath.ToSlash(filepath.Clean(s))
		switch {
		case n == s, strings.HasSuffix(n, "/"+s), strings.HasSuffix(s, "/"+n):
			return s
		case path.Base(s) == base:
			if byBase != "" {
				return "" // two staged files share the name: refuse to guess
			}
			byBase = s
		}
	}
	return byBase
}

// emptyReason recognises a "why" that says only that the option lost.
//
// These are the reasons that make a record unusable: a later agent reading "not
// chosen" learns that someone once said no, and nothing about whether the no
// still applies. The list is short and literal on purpose — it matches phrases
// that carry no cause at all, not phrases that merely mention a person.
func emptyReason(s string) bool {
	t := strings.ToLower(strings.TrimRight(s, ". "))
	for _, dead := range []string{
		"not chosen", "was not chosen", "not selected", "was not selected",
		"rejected", "was rejected", "declined", "was declined",
		"deferred", "was deferred", "postponed", "out of scope",
		"the author preferred otherwise", "author preference", "user preference",
		"preference", "not needed", "no reason given", "unclear", "n/a",
	} {
		if t == dead {
			return true
		}
	}
	return false
}

// describesThisChange rejects an "invariant" that is really a report of the
// commit it sits in. A rule has to constrain work that has not happened yet; one
// that talks about "this change" constrains nothing and will be read by every
// future agent as though it did.
func describesThisChange(s string) bool {
	t := strings.ToLower(s)
	for _, marker := range []string{
		"this change", "this commit", "this session", "this pr", "this patch",
		"was added", "were added", "was fixed", "were fixed",
		"was renamed", "were renamed", "was raised", "were raised",
		"has been added", "have been added",
	} {
		if strings.Contains(t, marker) {
			return true
		}
	}
	return false
}

// Per-session caps. They are the same numbers the prompt states, enforced here
// because a model that ignores "at most three" ignores it by a wide margin.
// Merging several sessions may exceed them; that is the merge's business, since a
// commit spanning four sessions legitimately carries more than one did.
//
// maxRule is the one number with an external constraint behind it. The reactive
// injection is capped at 10 000 characters by the harnesses, and the design goal
// is fifty commits of a file's history inside that — roughly two rules per commit
// at 110 characters plus their commit line. A rule that will not fit in 110
// characters is carrying its own justification, which belongs in Why.
const (
	maxRule                 = 110
	maxWhy                  = 300
	maxFiles                = 3
	maxClaim                = 300
	maxClaims               = 6
	maxRejectedPerSession   = 3
	maxInvariantsPerSession = 2
)

func clean(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ")
	for _, junk := range []string{"N/A", "n/a", "None", "none", "null", "-", "unknown"} {
		if s == junk {
			return ""
		}
	}
	return transcript.Truncate(s, max)
}
