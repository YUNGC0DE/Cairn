// Package distill turns a session tail plus a staged diff into a commit record
// (spec §3.2).
//
// Two passes. The first extracts the record. The second re-reads the claims
// against the diff *without* the transcript and labels each one. The second pass
// is not a nicety: a record is written once and then read by every future agent,
// so an unchecked confabulation becomes permanent canon (spec §2, P5).
package distill

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/YUNGC0DE/Cairn/internal/llm"
	"github.com/YUNGC0DE/Cairn/internal/transcript"
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
	// Disputed means the diff contradicts at least one claim. Extends the three
	// values in spec §3.2: silently downgrading a contradiction to "partial"
	// would hide exactly the failure the verify pass exists to catch.
	Disputed Confidence = "disputed"
	// MetadataOnly means distillation produced no prose at all; the record is
	// trailers only.
	MetadataOnly Confidence = "metadata-only"
)

// Rejected is an alternative that was considered and turned down.
type Rejected struct {
	Option string `json:"option"`
	Reason string `json:"reason"`
}

// InvariantCandidate is a durable project rule proposed by a session. PRUNE
// (v0.3) decides whether it survives.
type InvariantCandidate struct {
	Text  string   `json:"text"`
	Scope []string `json:"scope"`
}

// Extraction is the schema the first pass must produce.
//
// OpenItems and NextStep exist from v0.1 on purpose (spec §8): they cost one
// prompt paragraph now, and without them `cairn resume` (§3.5) would need an LLM
// pass instead of a deterministic template — and records already written would
// never gain the fields retroactively.
type Extraction struct {
	Subject    string               `json:"subject"`
	Intent     string               `json:"intent"`
	Decision   string               `json:"decision"`
	Rejected   []Rejected           `json:"rejected"`
	Invariants []InvariantCandidate `json:"invariants"`
	OpenItems  []string             `json:"open_items"`
	NextStep   string               `json:"next_step"`
	Claims     []string             `json:"claims"`
}

// ClaimStatus is a verification verdict for one claim.
type ClaimStatus string

const (
	// Supported: the diff shows the claim to be true.
	Supported ClaimStatus = "supported"
	// Contradicted: the diff shows the opposite.
	Contradicted ClaimStatus = "contradicted"
	// Unverifiable: the diff neither shows nor denies it. Kept and marked rather
	// than dropped (spec §3.2) — an unverifiable claim is often the most
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
	// Budget is the total wall-clock allowance for both passes. A commit must
	// never hang: when the budget runs out we degrade, we do not wait.
	Budget time.Duration
	// Model overrides the engine default for extraction.
	Model string
	// VerifyModel overrides the model used for verification only. Checking claims
	// against a diff is a narrow mechanical task — a small fast model does it well
	// and, more importantly, leaves the pass affordable inside a commit's budget.
	VerifyModel string
	// PromptBudget caps the rendered session text in bytes.
	PromptBudget int
	// SkipVerify disables the second pass (used by `cairn audit --no-verify`).
	SkipVerify bool
	// Verbose streams progress to the writer, if set.
	Trace func(format string, args ...any)
}

// DefaultBudget is the wall-clock allowance for both passes.
//
// Spec §3.2 proposes 12 s. Measured on real hardware, extraction costs ~11 s and
// verification ~11 s, so 12 s would mean the verification pass — the whole answer
// to P5 — never runs, and every record would ship "unverified". 35 s fits both with
// room for a slow response. A record is written once and read for years, so
// correctness outranks the extra seconds; `cairn.timeout 12` restores the spec's
// budget for anyone who prefers the faster commit.
const DefaultBudget = 35 * time.Second

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
	deadline := start.Add(opts.Budget)

	body := transcript.Compact(in.Sessions, opts.PromptBudget)
	if strings.TrimSpace(body) == "" {
		return res, fmt.Errorf("distill: nothing to distill from %d session(s)", len(in.Sessions))
	}
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

	extractBudget := opts.Budget - minVerifyBudget
	if extractBudget <= 0 {
		extractBudget = opts.Budget
	}
	trace("extract: %s budget %s", engine.Name(), extractBudget.Round(time.Millisecond))
	resp, err := engine.Complete(ctx, llm.Request{
		System: extractSystem,
		Prompt: extractPrompt(body, in),
		Model:  opts.Model,
		Budget: extractBudget,
	})
	res.Elapsed = time.Since(start)
	if err != nil {
		res.Notes = append(res.Notes, "extraction failed: "+err.Error())
		return res, err
	}
	res.Model = resp.Model
	trace("extract: done in %s", resp.Elapsed.Round(time.Millisecond))

	var ex Extraction
	if err := llm.ExtractJSON(resp.Text, &ex); err != nil {
		res.Notes = append(res.Notes, "extraction returned unusable JSON: "+err.Error())
		return res, err
	}
	sanitize(&ex, in)
	res.Extraction = &ex
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
			"verification skipped: %s left of a %s budget", remaining.Round(time.Millisecond), opts.Budget))
		return res, nil
	}

	verifyModel := opts.VerifyModel
	if verifyModel == "" {
		verifyModel = llm.DefaultVerifyModel
	}
	trace("verify: %d claims, model %s, budget %s",
		len(ex.Claims), verifyModel, remaining.Round(time.Millisecond))
	vresp, err := engine.Complete(ctx, llm.Request{
		System: verifySystem,
		Prompt: verifyPrompt(ex.Claims, in),
		Model:  verifyModel,
		Budget: remaining,
	})
	res.Elapsed = time.Since(start)
	if err != nil {
		res.Notes = append(res.Notes, "verification failed: "+err.Error())
		return res, nil // extraction still stands; the record is just unverified
	}
	var v Verification
	if err := llm.ExtractJSON(vresp.Text, &v); err != nil {
		res.Notes = append(res.Notes, "verification returned unusable JSON: "+err.Error())
		return res, nil
	}
	attachClaims(&v, ex.Claims)
	res.Verification = &v
	res.Confidence = score(&v)
	trace("verify: done in %s → %s", vresp.Elapsed.Round(time.Millisecond), res.Confidence)
	return res, nil
}

// score maps verdicts to a confidence label.
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

// sanitize enforces the limits the prompt asks for but a model may exceed, and
// strips the failure modes seen most often: a restated subject, an empty
// rejected entry, an invariant that is really a one-off instruction.
func sanitize(ex *Extraction, in Input) {
	ex.Subject = firstLine(strings.TrimSpace(ex.Subject))
	if len(ex.Subject) > maxSubject {
		ex.Subject = ""
	}
	if strings.EqualFold(ex.Subject, strings.TrimSpace(in.Subject)) {
		ex.Subject = ""
	}
	ex.Intent = clean(ex.Intent, maxIntent)
	ex.Decision = clean(ex.Decision, maxDecision)

	rej := ex.Rejected[:0]
	for _, r := range ex.Rejected {
		r.Option = clean(r.Option, 160)
		r.Reason = clean(r.Reason, 400)
		if r.Option == "" || r.Reason == "" {
			continue // half a rejection is worse than none: it invites re-litigation
		}
		rej = append(rej, r)
	}
	ex.Rejected = rej

	inv := ex.Invariants[:0]
	for _, c := range ex.Invariants {
		c.Text = clean(c.Text, 240)
		if c.Text == "" {
			continue
		}
		c.Scope = normalizeScope(c.Scope)
		inv = append(inv, c)
	}
	ex.Invariants = inv

	open := ex.OpenItems[:0]
	for _, o := range ex.OpenItems {
		if o = clean(o, 200); o != "" {
			open = append(open, o)
		}
	}
	if len(open) > maxOpenItems {
		open = open[:maxOpenItems]
	}
	ex.OpenItems = open
	ex.NextStep = clean(ex.NextStep, 200)

	claims := ex.Claims[:0]
	for _, c := range ex.Claims {
		if c = clean(c, 300); c != "" {
			claims = append(claims, c)
		}
	}
	if len(claims) > maxClaims {
		claims = claims[:maxClaims]
	}
	ex.Claims = claims
}

func normalizeScope(scope []string) []string {
	out := scope[:0]
	for _, s := range scope {
		s = strings.TrimSpace(s)
		if s == "" || s == "*" || s == "**" {
			continue // a scope covering everything carries no information
		}
		out = append(out, s)
	}
	return out
}

const (
	maxSubject   = 72
	maxIntent    = 500
	maxDecision  = 700
	maxClaims    = 8
	maxOpenItems = 5
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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
