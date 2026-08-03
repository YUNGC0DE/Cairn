// Package record is Cairn's wire format: the prose and trailers written into a
// commit, and the reader that gets them back out.
//
// Trailers are parsed and written by git's own `interpret-trailers`, never by a
// hand-rolled parser, so `git log --grep`, `-S` and `--follow` keep working and
// a Cairn record coexists with any other tool's trailers.
package record

import (
	"fmt"
	"sort"
	"strings"

	"github.com/YUNGC0DE/git-cairn/internal/distill"
	"github.com/YUNGC0DE/git-cairn/internal/gitx"
)

// Trailer keys. Cairn-Agent doubles as the marker that a commit carries a
// record, so it is always written, even in the metadata-only degradation.
const (
	TrailerAgent      = "Cairn-Agent"
	TrailerSession    = "Cairn-Session"
	TrailerConfidence = "Cairn-Confidence"
	TrailerFiles      = "Cairn-Files"
	TrailerTranscript = "Cairn-Transcript"
	TrailerDisputed   = "Cairn-Disputed"
)

// The record's own delimiters.
//
// Everything Cairn writes into a commit message lives between these two lines,
// and nothing else does. Before them the record was loose prose recognised by
// capitalised prefixes, which cost more than it looks:
//
//   - git's trailer parser folded a short "Invariant: …" line into the trailer
//     block, so reading a record back depended on how git had grouped paragraphs;
//   - a wrapped entry's continuation lines had to be re-attached by guessing,
//     which is how a read-back could come out shuffled;
//   - the reactive channel had no way to tell the author's own words from
//     Cairn's, so it cut the message at a list of known bookkeeping prefixes and
//     hoped.
//
// A closing tag on its own also ends the message with a paragraph that is not
// trailer-shaped, so `interpret-trailers` appends a clean trailer block after it
// instead of merging into whatever the record's last line happened to look like.
const (
	OpenTag  = "<git-cairn>"
	CloseTag = "</git-cairn>"
)

// Field keys inside the block. A key sits at column zero and its continuation
// lines are indented, which is the whole grammar: no key can be confused with a
// wrapped line, and no wrapped line can be confused with a git trailer.
const (
	whyKey         = "why:"
	rejectedKey    = "rejected:"
	becauseKey     = "because:"
	invariantKey   = "invariant:"
	scopeKey       = "scope:"
	unconfirmedKey = "unconfirmed:"
)

// Legacy prefixes, from before the block existed. They are still read, because
// the records already written with them are the whole point of the tool — a
// format change that silently blanks a repository's history would be worse than
// the format it replaced. They are never written.
//
// Open: and Next: are recognised only so they can be discarded: they held the
// state of the work at one instant, which is stale by the next commit, and the
// reactive channel was already cutting them before serving a record.
const (
	legacyRejected    = "Rejected: "
	legacyInvariant   = "Invariant: "
	legacyOpen        = "Open: "
	legacyNext        = "Next: "
	legacyUnconfirmed = "Cairn could not confirm against the diff: "
)

// Limits keep a record from swallowing the commit message (the risk being "commit
// messages bloat, the team revolts").
//
// They were originally much tighter — four rejections, three invariants — and
// that was wrong once the record started being read back into agents. The
// overflow is only reachable through `cairn rejected`, which is a command a
// human runs; an agent handed the record on a file touch never sees it. So a
// rejection dropped here is not "available elsewhere", it is lost from the one
// channel that would have used it, and lost permanently, because the record is
// written once and read many times.
//
// The bloat risk is real but was never measured, and it is the cheaper of the
// two: a long commit message is annoying, a forgotten rejection gets rebuilt.
const (
	maxRejectedRendered  = 12
	maxInvariantRendered = 10
)

// Meta is the machine-readable half of a record.
type Meta struct {
	Agent       string // "claude-code/opus-5"
	Sessions    []string
	Files       []string
	Transcripts []string
	Confidence  distill.Confidence
	Disputed    []string
}

// Body renders the record as one <git-cairn> block. Returns "" when there is
// nothing worth saying — an empty block is worse than no block, because a reader
// who finds one stops looking for the reasoning elsewhere.
//
// Every entry is one paragraph, so the block reads as a short list rather than a
// wall: the complaint that produced this layout was not that records were wrong
// but that they were unreadable.
func Body(res *distill.Result) string {
	if res == nil || res.Extraction == nil {
		return ""
	}
	ex := res.Extraction
	var paras []string
	for _, w := range ex.Why {
		if w = strings.TrimSpace(w); w != "" {
			paras = append(paras, field(whyKey, w))
		}
	}

	for i, r := range ex.Rejected {
		if i >= maxRejectedRendered {
			paras = append(paras, fmt.Sprintf("%s (+%d more not recorded)",
				rejectedKey, len(ex.Rejected)-i))
			break
		}
		paras = append(paras, field(rejectedKey, r.Option)+"\n"+subfield(becauseKey, r.Because))
	}

	for i, c := range ex.Invariants {
		if i >= maxInvariantRendered {
			break
		}
		entry := field(invariantKey, c.Rule)
		if len(c.Scope) > 0 {
			entry += "\n" + subfield(scopeKey, strings.Join(c.Scope, ", "))
		}
		paras = append(paras, entry)
	}

	// A contradicted claim is never rendered as fact. It is named, in the open, so
	// the next reader knows the record and the code disagree — and named here
	// rather than only in a trailer, because the trailers are cut before an agent
	// is served the record.
	for _, d := range res.DisputedClaims() {
		paras = append(paras, field(unconfirmedKey, d.Claim))
	}

	if len(paras) == 0 {
		return ""
	}
	return OpenTag + "\n" + strings.Join(paras, "\n\n") + "\n" + CloseTag
}

// field renders "key: value", wrapping continuation lines under a two-space
// indent so the grammar stays unambiguous: column zero starts a field, anything
// indented continues one.
func field(key, value string) string {
	return wrapIndent(key+" "+strings.TrimSpace(value), "  ")
}

// subfield renders a key that belongs to the entry above it, one level in.
func subfield(key, value string) string {
	return wrapIndent("  "+key+" "+strings.TrimSpace(value), "    ")
}

// Trailers renders the machine-readable half, in the order they should appear.
func (m Meta) Trailers() [][2]string {
	out := [][2]string{
		{TrailerAgent, m.Agent},
		{TrailerSession, strings.Join(m.Sessions, ",")},
		{TrailerConfidence, string(m.Confidence)},
		{TrailerFiles, strings.Join(m.Files, ",")},
		{TrailerTranscript, strings.Join(m.Transcripts, ",")},
	}
	for _, d := range m.Disputed {
		out = append(out, [2]string{TrailerDisputed, d})
	}
	return out
}

// Compose builds a full commit message: the author's existing message, then the
// record's prose, then trailers. The author's subject and body are never
// rewritten — Cairn appends, so a human's words always survive.
func Compose(repoDir, existing, body string, meta Meta) (string, error) {
	existing = strings.TrimRight(existing, "\n")
	msg := existing
	if body != "" {
		if strings.TrimSpace(msg) == "" {
			msg = body
		} else {
			msg = msg + "\n\n" + body
		}
	}
	return gitx.AddTrailers(repoDir, msg+"\n", meta.Trailers())
}

// Record is a parsed record read back out of a commit.
type Record struct {
	SHA         string
	Short       string
	Subject     string
	Why         []string
	Rejected    []string
	Invariants  []string
	Disputed    []string
	Agent       string
	Sessions    []string
	Files       []string
	Transcripts []string
	Confidence  string
}

// Has reports whether a commit carries a Cairn record at all.
func (r *Record) Has() bool { return r != nil && r.Agent != "" }

// Parse reads a record out of a commit message. Trailers come from git's own
// parser; the prose lines are recognised by their prefixes.
func Parse(repoDir string, c gitx.Commit) (*Record, error) {
	rec := &Record{SHA: c.SHA, Short: c.Short, Subject: c.Subject}
	trailers, err := gitx.ParseTrailers(repoDir, c.Message())
	if err != nil {
		return nil, err
	}
	rec.Agent = gitx.Trailer(trailers, TrailerAgent)
	rec.Confidence = gitx.Trailer(trailers, TrailerConfidence)
	rec.Sessions = splitList(gitx.Trailer(trailers, TrailerSession))
	rec.Files = splitList(gitx.Trailer(trailers, TrailerFiles))
	rec.Transcripts = splitList(gitx.Trailer(trailers, TrailerTranscript))
	for _, t := range trailers {
		if strings.EqualFold(t[0], TrailerDisputed) {
			rec.Disputed = append(rec.Disputed, t[1])
		}
	}

	if body, ok := blockIn(c.Body); ok {
		parseBlock(rec, body)
	} else {
		parseLegacy(rec, c.Body, trailers)
	}
	rec.Disputed = Dedup(rec.Disputed)
	return rec, nil
}

// blockIn returns the contents of the <git-cairn> block, if the message has one.
func blockIn(msg string) (string, bool) {
	i := strings.Index(msg, OpenTag)
	if i < 0 {
		return "", false
	}
	rest := msg[i+len(OpenTag):]
	if j := strings.Index(rest, CloseTag); j >= 0 {
		return rest[:j], true
	}
	// An unterminated block is still a block: a truncated message should give up
	// its reasoning, not be silently reclassified as an old-format record and
	// re-read by a parser that would mangle it.
	return rest, true
}

// parseBlock reads the block grammar: a key at column zero opens an entry, an
// indented line either opens a subfield or continues the line above it, and a
// blank line closes the entry.
func parseBlock(rec *Record, body string) {
	var cur *string     // the line an indented continuation extends
	var pending *string // the entry a because:/scope: subfield belongs to

	appendTo := func(list *[]string, s string) *string {
		*list = append(*list, s)
		return &(*list)[len(*list)-1]
	}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		indented := line != "" && (line[0] == ' ' || line[0] == '\t')
		switch {
		case trimmed == "":
			cur, pending = nil, nil
		case !indented && hasKey(trimmed, whyKey):
			cur = appendTo(&rec.Why, value(trimmed, whyKey))
			pending = cur
		case !indented && hasKey(trimmed, rejectedKey):
			cur = appendTo(&rec.Rejected, value(trimmed, rejectedKey))
			pending = cur
		case !indented && hasKey(trimmed, invariantKey):
			cur = appendTo(&rec.Invariants, value(trimmed, invariantKey))
			pending = cur
		case !indented && hasKey(trimmed, unconfirmedKey):
			cur = appendTo(&rec.Disputed, value(trimmed, unconfirmedKey))
			pending = cur
		// because: and scope: belong to the entry above them. They are folded into
		// that entry's text rather than kept apart, because every reader of a
		// record — human or agent — wants the reason next to the option, and
		// nothing downstream has ever wanted them separately.
		case hasKey(trimmed, becauseKey) && pending != nil:
			*pending += " — " + value(trimmed, becauseKey)
			cur = pending
		case hasKey(trimmed, scopeKey) && pending != nil:
			*pending += " (" + value(trimmed, scopeKey) + ")"
			cur = pending
		case cur != nil:
			*cur += " " + trimmed
		}
	}
}

func hasKey(line, key string) bool {
	return strings.HasPrefix(strings.ToLower(line), key)
}

func value(line, key string) string {
	return strings.TrimSpace(line[len(key):])
}

// parseLegacy reads the pre-block format: capitalised prefixes in free prose,
// with wrapped entries continuing until a blank line. It exists only for records
// already in a repository's history.
func parseLegacy(rec *Record, body string, trailers [][2]string) {
	var prose []string
	const (
		inProse = iota
		inRejected
		inInvariant
		inDropped
		inDisputed
	)
	block := inProse
	appendTo := func(target *[]string, s string) {
		if n := len(*target); n > 0 {
			(*target)[n-1] += " " + s
		} else {
			*target = append(*target, s)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			block = inProse
		// Prefixes are matched before the trailer check on purpose. A short
		// "Invariant: …" line is shaped like a trailer, so git folds it into the
		// trailer block and its own parser reports it as one — which is exactly the
		// ambiguity the <git-cairn> block was introduced to remove.
		case strings.HasPrefix(trimmed, legacyRejected):
			block = inRejected
			rec.Rejected = append(rec.Rejected, strings.TrimPrefix(trimmed, legacyRejected))
		case strings.HasPrefix(trimmed, legacyInvariant):
			block = inInvariant
			rec.Invariants = append(rec.Invariants, strings.TrimPrefix(trimmed, legacyInvariant))
		case strings.HasPrefix(trimmed, legacyOpen), strings.HasPrefix(trimmed, legacyNext):
			block = inDropped // recognised so it is not mistaken for prose, then dropped
		case strings.HasPrefix(trimmed, legacyUnconfirmed):
			block = inDisputed
			rec.Disputed = append(rec.Disputed, strings.TrimPrefix(trimmed, legacyUnconfirmed))
		case isTrailerLine(trailers, trimmed):
			block = inProse
		default:
			switch block {
			case inRejected:
				appendTo(&rec.Rejected, trimmed)
			case inInvariant:
				appendTo(&rec.Invariants, trimmed)
			case inDisputed:
				appendTo(&rec.Disputed, trimmed)
			case inDropped:
			default:
				prose = append(prose, trimmed)
			}
		}
	}
	if s := strings.TrimSpace(strings.Join(prose, " ")); s != "" {
		rec.Why = []string{s}
	}
}

// isTrailerLine reports whether a body line is one of the parsed trailers, so
// prose extraction does not swallow them.
func isTrailerLine(trailers [][2]string, line string) bool {
	k, v, ok := strings.Cut(line, ":")
	if !ok {
		return false
	}
	k, v = strings.TrimSpace(k), strings.TrimSpace(v)
	for _, t := range trailers {
		if strings.EqualFold(t[0], k) && t[1] == v {
			return true
		}
	}
	return false
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Dedup returns a sorted, de-duplicated copy.
func Dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

const wrapAt = 76

// wrapIndent hard-wraps a paragraph at git's conventional body width, prefixing
// every line after the first with indent. The indent is what makes the block
// parseable: a wrapped line can never be mistaken for a new field, and a field
// can never be mistaken for a git trailer.
func wrapIndent(s, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	lineLen := 0
	for i, w := range words {
		switch {
		case i == 0:
			b.WriteString(w)
			lineLen = len(w)
		case lineLen+1+len(w) > wrapAt:
			b.WriteByte('\n')
			b.WriteString(indent)
			b.WriteString(w)
			lineLen = len(indent) + len(w)
		default:
			b.WriteByte(' ')
			b.WriteString(w)
			lineLen += 1 + len(w)
		}
	}
	return b.String()
}
