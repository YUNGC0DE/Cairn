package distill

import (
	"fmt"
	"strings"
)

// The prompts are the product. Every rule below exists because its absence
// produced a specific bad record in this repository's own history: invented
// rejections, invariants that were really one-off instructions, prose that
// restated the diff, and the same intention written out four times because four
// sessions each said it slightly differently.
//
// Two things shape the whole file. The record is written once and then read by
// every future agent that opens the file, so a plausible-sounding sentence is
// not a cheap mistake — it becomes canon. And the reader pays for every byte in
// context it could have spent on the work, so a field that cannot change what
// the next agent does has negative value and must not be asked for at all.

const noTools = ` You have no tools and no filesystem access, and you need none: everything required is in the message. Never attempt a tool call — answer directly.`

const extractSystem = `You read one agent coding session and the staged git diff it produced, and you write the record that is committed alongside that diff. Later agents are shown this record when they open the files it touched, so a sentence that is vague, invented, or merely restates the diff costs them context and teaches them something false. You emit exactly one raw JSON object and nothing else: no prose, no markdown fences, no commentary.` + noTools

// The section order is deliberate: the requests come first because they are what
// the record has to explain, and the diff comes last because it is the largest
// and the least ambiguous — nothing after it can be misread as instructions.
const extractInstructions = `Emit one JSON object with exactly these four keys:

{
  "why": "",
  "rejected":   [{"option": "", "because": ""}],
  "invariants": [{"rule": "", "scope": []}],
  "claims":     [""]
}

You are given, in this order: <human-requests> (everything the human typed,
verbatim, oldest first), <agent-session> (what the agent thought, said and ran;
may be truncated), and <staged-diff> (the change being committed).

── why ────────────────────────────────────────────────────────────────────────

Two to four sentences answering one question: what did the human want, and why
did they want it?

Its source is <human-requests> — those are the author's own words. A reason is
usually stated once, in passing, in an early request; carry it over even when
the later requests are pure mechanics. When several topics were discussed, answer
for the work that is in <staged-diff> and ignore the rest: blending unrelated
requests into one purpose produces a sentence that is true of nothing.

When the session walked through several bugs or iterations on the way to one
ask, write that ask once. Do not give each bug its own paragraph — "images were
gone", then "centering broke", then "images still left-aligned" is a changelog
of the session, not the human's intention.

The last sentence may say what was actually built, and only if the diff would
not make it obvious. Never write a changelog — the reader has the diff open.

If the requests give no reason at all, say in one sentence what the change
accomplishes and stop. A short "why" is a good "why"; padding is not neutral,
it displaces the reader's own work.

  Bad — restates the diff: "Adds a token-bucket rate limiter to the auth
  handlers, wires it into the middleware, and adds tests."
  Bad — the same point three times: "The budget must be per session. Each
  session should get its own allowance. Sessions must not share one budget."
  Good: "Credential stuffing hit /login with 40k attempts overnight, and the
  author wanted repeated attempts from one client stopped without adding
  infrastructure to the deployment."

── rejected ───────────────────────────────────────────────────────────────────

Alternatives that were on the table in this session and were turned down, each
with the reason it lost.

Include one only if BOTH of these hold:
  1. Someone actually raised it — the human or the agent — and then dropped it.
  2. A later agent could plausibly propose it again, and the reason it lost is
     still the answer to why not.

Exclude, and this covers most of what a session will offer you:
  - Anything you infer was "probably considered". If nobody said it, it is not
    here. Silence is not deliberation.
  - Ordering and scheduling: "do X first", "leave Y for later". That is a plan,
    not a rejected design, and it is stale within a week.
  - A reason that is only a preference — "the author chose otherwise", "not
    selected", "deferred". If you cannot name what makes the option wrong,
    worse, or unavailable, drop the entry entirely.
  - A command that failed and was then retried or corrected. A typo is not a
    design alternative.
  - Not making the change at all. The status quo is never a rejected option.

At most three. Zero is the normal answer and the correct one for most commits.
Each "because" must name what disqualified the option: a constraint, a rule, a
measurement, an assumption that turned out false. An invented rejection is far
worse than an empty list — every agent that later opens this file will treat it
as a decision already made.

── invariants ─────────────────────────────────────────────────────────────────

Rules that must hold for FUTURE work in this repository and that this session
established or confirmed.

Include one only if BOTH of these hold:
  1. It reads as a property, not an event. "A hook failure must never block a
     commit" is a rule; "the hook failure was fixed" is a report.
  2. Breaking it would do real damage — a bug, a security hole, data loss, a
     rejected review — and someone could break it without noticing. That is
     what makes it worth carrying; a rule nobody can violate teaches nothing.

Exclude:
  - A restatement of what this commit did. The diff already says it.
  - House style: formatting, naming, "add tests", "keep functions small". Every
    repository has those and no commit needs to announce them.
  - Anything true only for the moment ("we are on v2 for now").
  - A one-off instruction for this task ("use tabs here", "call it limitByKey").
  - A second phrasing of a rule you already wrote. "Resolve the binary name from
    argv[0]" and "use the prog variable derived from argv[0]" are one rule —
    keep the clearer one, drop the echo.

At most two, and zero is the normal answer. "scope" is the path globs the rule
constrains, e.g. ["internal/auth/**"] — leave it empty only when the rule truly
binds the whole repository, because an unscoped rule is served to every agent
that opens any file.

── claims ─────────────────────────────────────────────────────────────────────

Zero to four statements about <staged-diff> that someone holding only the diff
could check and could prove false. They exist so a second pass can catch a
record that reads well and is not true, so state the load-bearing parts of
"why" and "because" — not decoration.

Claim a concrete edit to a named file or symbol. Do not claim what a document
"shows", "conveys", or "replaces with an image" — those are readings of prose,
and the verifier holding only the diff will either invent agreement or correctly
contradict you.

  Claim: "The bucket is held in memory; no Redis client is added."
  Claim: "README.md drops the Spec-Driven comparison table and adds a Complements section."
  Not a claim: "Peak traffic is 340 req/s" — no diff can settle it.
  Not a claim: "The code is cleaner now" — nothing could falsify it.
  Not a claim: "The comparison table was replaced by a pyramid image" — unless
  the diff itself adds that image reference.

── style ──────────────────────────────────────────────────────────────────────

Write for a developer reading ` + "`git log`" + ` two years from now. No first person. Never
mention Cairn, this record, transcripts, sessions, agents, or that a tool wrote
any of it. Return "" or [] for anything the session does not support — an empty
field is a finding, not a failure.`

// extractPrompt assembles the extraction request.
//
// The human's requests get their own section above the session body because they
// are what the record is supposed to explain, and buried in the transcript they
// lose to whatever the agent did most recently — which is how a record ends up
// paraphrasing its own diff. The sections are XML-delimited rather than
// ===-delimited: transcripts contain both `===` banners and diff hunks, so a
// heading made of punctuation is not reliably a boundary, while a closing tag
// can be neutralised in the payload (see fence).
func extractPrompt(requests, sessionText string, in Input) string {
	var b strings.Builder
	b.WriteString(extractInstructions)
	if s := strings.TrimSpace(in.Subject); s != "" {
		// Context, not a task: the subject says which of a session's topics is
		// being committed, which is exactly what disambiguates a multi-topic
		// session. Cairn never rewrites it, so nothing is asked about it.
		fmt.Fprintf(&b, "\n\n<commit-subject>\n%s\n</commit-subject>", fence(s, "commit-subject"))
	}
	if r := strings.TrimSpace(requests); r != "" {
		b.WriteString("\n\n<human-requests>\n")
		b.WriteString(fence(r, "human-requests"))
		b.WriteString("\n</human-requests>")
	}
	b.WriteString("\n\n<agent-session>\n")
	if s := strings.TrimSpace(sessionText); s != "" {
		b.WriteString(fence(s, "agent-session"))
	} else {
		b.WriteString("(the session body did not fit the prompt budget)")
	}
	b.WriteString("\n</agent-session>")
	b.WriteString("\n\n<staged-diff>\n")
	if d := strings.TrimSpace(in.Diff); d != "" {
		b.WriteString(fence(d, "staged-diff"))
	} else {
		b.WriteString("(empty)")
	}
	if in.DiffTruncated {
		b.WriteString("\n(truncated — claim nothing about the omitted part)")
	}
	b.WriteString("\n</staged-diff>\n")
	return b.String()
}

// fence keeps a payload from closing the section that holds it. A transcript
// that quotes this very prompt — which happens the moment cairn is developed
// with an agent — would otherwise end <agent-session> early and turn the rest of
// the session into instructions.
func fence(s, tag string) string {
	return strings.ReplaceAll(s, "</"+tag+">", `<\/`+tag+`>`)
}

const verifySystem = `You are a verification pass, and adversarial by design: you are given a git diff and a list of claims about it. You have NOT seen the conversation that produced the claims and must not guess at it. For each claim, decide whether the diff settles it. You emit exactly one raw JSON object and nothing else: no prose, no markdown fences, no commentary.` + noTools

const verifyInstructions = `Emit one JSON object with exactly this shape:

{"claims": [{"index": 0, "status": "supported", "note": ""}]}

One entry per claim, using the index it was given.

status:
  "supported"    — the diff shows the claim to be true.
  "contradicted" — the diff shows the opposite, or shows the claim is false.
  "unverifiable" — the diff neither establishes nor refutes it.

note — at most 120 characters, naming the evidence (a file, a symbol, a line) or
naming what is missing.

Be strict, not charitable. This pass exists to catch plausible-sounding
statements the code does not actually support, so:
  - A claim about runtime behaviour, load, production, users, or intent is
    "unverifiable" — a diff cannot settle it. Do not stretch to "supported".
  - A claim the diff only partly shows is "unverifiable", not "supported".
  - Judge against the diff alone. Absence of evidence is "unverifiable";
    evidence of absence is "contradicted".`

// verifyPrompt assembles the verification request. It deliberately omits the
// transcript: a verifier that has read the session will agree with it.
func verifyPrompt(claims []string, in Input) string {
	var b strings.Builder
	b.WriteString(verifyInstructions)
	b.WriteString("\n\n<diff>\n")
	if d := strings.TrimSpace(in.Diff); d != "" {
		b.WriteString(fence(d, "diff"))
	} else {
		b.WriteString("(empty)")
	}
	if in.DiffTruncated {
		b.WriteString("\n(truncated — claims about the omitted part are \"unverifiable\")")
	}
	b.WriteString("\n</diff>\n\n<claims>\n")
	for i, c := range claims {
		fmt.Fprintf(&b, "%d. %s\n", i, fence(c, "claims"))
	}
	b.WriteString("</claims>\n")
	return b.String()
}
