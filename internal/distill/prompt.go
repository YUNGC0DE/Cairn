package distill

import (
	"fmt"
	"strings"
)

// The prompts are the product. Every rule below exists because its absence
// produced a specific bad record: invented rejections, invariants that were
// really one-off instructions, prose that restated the diff, or claims that
// could not be checked against anything.

const noTools = ` You have no tools and no filesystem access, and you need none: everything required is in the message. Never attempt a tool call — answer directly.`

const extractSystem = `You are Cairn's extraction pass. You read an agent coding session and the staged git diff it produced, and you emit exactly one JSON object describing the change. You output raw JSON and nothing else: no prose, no markdown fences, no commentary.` + noTools

const extractInstructions = `Emit a JSON object with exactly these keys:

{
  "subject": "",                 // imperative summary, <=70 chars, or "" to keep the author's
  "intent": "",                  // 1-2 sentences: WHY this change exists
  "decision": "",                // 1-3 sentences: what was decided and what settled it, or ""
  "rejected": [{"option": "", "reason": ""}],
  "invariants": [{"text": "", "scope": []}],
  "open_items": [""],
  "next_step": "",
  "claims": [""]
}

intent — the reason the change was made, in the author's terms. Its source is
WHAT THE HUMAN ASKED, below: those are the author's own words, and the last
requests are the ones this change answers. Never restate what the diff does; a
reader can see the diff, and a summary of it is not a reason. If the requests
give no reason, say what the change accomplishes and stop. No filler.

A session often spans several topics — earlier requests may have produced earlier
commits. Weigh the requests that match the staged diff, and ignore the rest
rather than blending them into one vague purpose.

decision — only when the session actually settled something: a tradeoff weighed,
an approach chosen over another, a constraint discovered. Empty string when the
work was mechanical.

rejected — alternatives that were genuinely considered and turned DOWN in this
session, each with the reason it lost. This is the most valuable field and the
easiest to get wrong:
  - Include: an approach discussed then abandoned; a library or service
    considered then declined; an implementation tried, then reverted or replaced.
  - Exclude: anything you infer was "probably" considered. Exclude approaches
    nobody mentioned. Exclude a failed command that was simply retried.
  - An empty array is the correct and common answer. A fabricated rejection is
    worse than an empty field: it will be read as settled precedent for years.

invariants — durable rules that should constrain FUTURE work in this repository,
stated as rules, not as events. "No new external datastores without an ADR" is
an invariant. "Rate limiting was added to /login" is not. "scope" is a list of
path globs the rule applies to (e.g. ["internal/auth/**"]); omit or leave empty
only when the rule truly applies repository-wide. Most sessions yield none.

open_items — work this change knowingly leaves unfinished, each as a concrete
fact a later reader can check off: "X-RateLimit-* response headers not
implemented", "/register endpoint still unprotected". Only what the session
actually established as incomplete — not speculative improvements, not a wishlist,
not "add more tests". Empty array when the change is complete.

next_step — the single most obvious next action, if the session made one clear.
One short sentence, imperative. Empty string when there is no obvious next step;
do not invent one to fill the field.

claims — falsifiable statements about the STAGED DIFF, each checkable by someone
holding only the diff. "The token bucket is stored in memory, not Redis" is a
claim. "Peak traffic is 340 req/s" is not (nothing in a diff can settle it), and
neither is "the code is now cleaner". Emit 0-6 claims covering the load-bearing
statements in your intent and decision. Fewer, sharper claims beat more.

Style: write for a developer reading ` + "`git log`" + ` two years from now. Do not mention
Cairn, transcripts, sessions, agents, or that a tool wrote this. No first person.`

// extractPrompt assembles the extraction request.
//
// The human's requests are their own section, above the session body. They are
// what the record is supposed to explain, and buried in the transcript they lose
// to whatever the agent did most recently — which is how a record ends up
// paraphrasing its own diff.
func extractPrompt(requests, sessionText string, in Input) string {
	var b strings.Builder
	b.WriteString(extractInstructions)
	if s := strings.TrimSpace(in.Subject); s != "" {
		fmt.Fprintf(&b, "\n\nThe author already wrote this subject line: %q\n"+
			"Return \"\" for subject unless you can state the change materially better.", s)
	}
	if strings.TrimSpace(requests) != "" {
		b.WriteString("\n\n=== WHAT THE HUMAN ASKED (verbatim, oldest first) ===\n")
		b.WriteString(requests)
	}
	b.WriteString("\n\n=== AGENT SESSION")
	b.WriteString(" (may be truncated; oldest turns elided) ===\n")
	b.WriteString(sessionText)
	b.WriteString("\n\n=== STAGED DIFF ===\n")
	if strings.TrimSpace(in.Diff) == "" {
		b.WriteString("(empty)\n")
	} else {
		b.WriteString(in.Diff)
		b.WriteByte('\n')
	}
	if in.DiffTruncated {
		b.WriteString("\n(the diff above is truncated; do not claim anything about the omitted part)\n")
	}
	return b.String()
}

const verifySystem = `You are Cairn's verification pass. You are given a git diff and a list of claims about it. You have NOT seen the conversation that produced the claims and must not guess at it. For each claim, decide whether the diff settles it. You output raw JSON and nothing else: no prose, no markdown fences, no commentary.` + noTools

const verifyInstructions = `Emit a JSON object with exactly this shape:

{"claims": [{"index": 0, "status": "supported", "note": ""}]}

One entry per claim, using the claim's index as given.

status:
  "supported"    — the diff shows the claim to be true.
  "contradicted" — the diff shows the opposite, or shows the claim is false.
  "unverifiable" — the diff neither establishes nor refutes it.

note — at most 120 characters, pointing at the evidence (a file, a symbol, a
line) or naming what is missing.

Be strict, not charitable. This pass exists to catch plausible-sounding
statements that the code does not actually support, so:
  - A claim about runtime behaviour, load, production, users, or intent is
    "unverifiable" — a diff cannot settle it. Do not stretch to "supported".
  - A claim only partly shown by the diff is "unverifiable", not "supported".
  - Judge only against the diff below. Absence of evidence is "unverifiable";
    evidence of absence is "contradicted".`

// verifyPrompt assembles the verification request. It deliberately omits the
// transcript: a verifier that has read the session will agree with it.
func verifyPrompt(claims []string, in Input) string {
	var b strings.Builder
	b.WriteString(verifyInstructions)
	b.WriteString("\n\n=== DIFF ===\n")
	if strings.TrimSpace(in.Diff) == "" {
		b.WriteString("(empty)\n")
	} else {
		b.WriteString(in.Diff)
		b.WriteByte('\n')
	}
	if in.DiffTruncated {
		b.WriteString("\n(this diff is truncated; claims about the omitted part are \"unverifiable\")\n")
	}
	b.WriteString("\n=== CLAIMS ===\n")
	for i, c := range claims {
		fmt.Fprintf(&b, "%d. %s\n", i, c)
	}
	return b.String()
}
