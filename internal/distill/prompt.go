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

const extractSystem = `You read one agent coding session and the staged git diff it produced, and you write the rules that are committed alongside that diff. Later agents are shown those rules the moment they open one of the files they bind, so a sentence that is vague, invented, or merely restates the diff costs them context and teaches them something false. You emit exactly one raw JSON object and nothing else: no prose, no markdown fences, no commentary.` + noTools

// The section order is deliberate: the requests come first because they are what
// the rules have to be grounded in, and the diff comes last because it is the
// largest and the least ambiguous — nothing after it can be misread as
// instructions.
const extractInstructions = `Emit one JSON object with exactly these three keys:

{
  "rejected":   [{"rule": "", "why": "", "files": [""]}],
  "invariants": [{"rule": "", "why": "", "files": [""]}],
  "claims":     [""]
}

You are given, in this order: <human-requests> (everything the human typed,
verbatim, oldest first), <staged-files> (the paths this commit changes),
<agent-session> (what the agent thought, said and ran; may be truncated), and
<staged-diff> (the change being committed).

This record is not a summary of the commit. Nobody needs one: the reader has the
diff and the commit message. It is a set of rules about specific files, written
so the next agent that opens one of those files does not undo a decision already
made. Everything that is not such a rule is left out.

One test decides every entry: would a competent agent, six months from now, work
differently for having read it? If not, leave it out. Empty lists are the normal
result and cost nothing; an entry that changes nobody's behaviour costs every
future reader the context it occupies.

── the shape of one entry ─────────────────────────────────────────────────────

"rule"  — the instruction itself, at most 110 characters, imperative, and with
          NO justification in it. This is the only part a later agent is shown
          on opening the file, and the space is hard-limited: fifty commits of a
          file's history have to fit in 10 000 characters. Write the sentence
          you would want shouted at someone about to make the mistake.
"why"   — one or two sentences, at most 300 characters, saying what makes the
          rule true: the constraint, the measurement, the rule it would break.
          This stays in the commit and is read only by someone who went looking.
"files" — the repo-relative paths from <staged-files> the rule binds, at most
          three. Copy them exactly as they appear there.

  Bad rule — carries its own reasoning: "Do not add a Redis rate limiter,
  because ADR-412 forbids new datastores and one instance at 340 req/s does not
  need cross-instance precision."
  Good: rule "No Redis-backed rate limiter here — keep the bucket in process",
        why  "ADR-412 disallows new external datastores, and a single instance
              at 340 req/s does not need cross-instance precision."

"files" is what decides who is ever shown the rule: a later agent is served an
entry only when the file it opened is one of these. A rule bound to no file, or
to a file outside <staged-files>, reaches nobody and is discarded — so name the
file the rule is actually about, and if it binds two or three of them, name
those. Never write a glob, a directory, or a path that is not in <staged-files>.

── rejected ───────────────────────────────────────────────────────────────────

Alternatives that were on the table in this session and were turned down.

Include one only if ALL of these hold:
  1. Someone actually raised it — the human or the agent — and then dropped it.
  2. A later agent could plausibly propose it again, and the reason it lost is
     still the answer to why not.
  3. It is a choice someone could face again: a design, a library, a mechanism,
     a place to put something. Not a detail so local that the choice cannot
     recur — one line's wording, one selector, one test's fixture. Those cost a
     future reader more than they save.

Exclude, and this covers most of what a session will offer you:
  - Anything you infer was "probably considered". If nobody said it, it is not
    here. Silence is not deliberation.
  - Ordering and scheduling: "do X first", "leave Y for later". That is a plan,
    not a rejected design, and it is stale within a week.
  - A "why" that is only a preference — "the author chose otherwise", "not
    selected", "deferred". If you cannot name what makes the option wrong,
    worse, or unavailable, drop the entry entirely.
  - A command that failed and was then retried or corrected. A typo is not a
    design alternative.
  - Not making the change at all. The status quo is never a rejected option.

At most three. Zero is the normal answer and the correct one for most commits.
An invented rejection is far worse than an empty list — every agent that later
opens that file will treat it as a decision already made.

── invariants ─────────────────────────────────────────────────────────────────

Properties the named files must keep, that this session established or confirmed.

Include one only if ALL of these hold:
  1. It reads as a property, not an event. "A hook failure must never block a
     commit" is a rule; "the hook failure was fixed" is a report.
  2. Breaking it would do real damage — a bug, a security hole, data loss, a
     rejected review — and someone could break it without noticing. That is
     what makes it worth carrying; a rule nobody can violate teaches nothing.
  3. It binds code in <staged-files> — neither one line nobody will touch again,
     nor something true of every repository in the world. A rule about a
     function's contract, a file format, a boundary between components is the
     right size; "this variable stays lowercase" and "write clean code" are the
     two ways of being useless.

Exclude:
  - A restatement of what this commit did. The diff already says it.
  - House style: formatting, naming, "add tests", "keep functions small". Every
    repository has those and no commit needs to announce them.
  - Anything true only for the moment ("we are on v2 for now").
  - A one-off instruction for this task ("use tabs here", "call it limitByKey").
  - A second phrasing of a rule you already wrote. "Resolve the binary name from
    argv[0]" and "use the prog variable derived from argv[0]" are one rule —
    keep the clearer one, drop the echo.

At most two, and zero is the normal answer.

── claims ─────────────────────────────────────────────────────────────────────

Zero to four statements about <staged-diff> that someone holding only the diff
could check and could prove false. They exist so a second pass can catch a
record that reads well and is not true, so state the load-bearing parts of the
rules and their "why" — not decoration.

One fact per claim. A claim joined by "and" is checked as a whole, so a reader
who can confirm the first half and not the second must call the whole thing
contradicted — which is how a true record ends up labelled disputed. Split it.

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
any of it. Return [] for anything the session does not support — an empty list
is a finding, not a failure.`

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
	// The staged paths get their own section rather than being left for the model
	// to read off the diff headers. Every rule has to name one of them exactly, and
	// a path recovered from a hunk header is a path the model may retype wrong —
	// which costs the rule, since one it cannot be bound to is discarded.
	if len(in.Files) > 0 {
		b.WriteString("\n\n<staged-files>\n")
		for _, f := range in.Files {
			b.WriteString(fence(f, "staged-files") + "\n")
		}
		b.WriteString("</staged-files>")
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
