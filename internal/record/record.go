// Package record is Cairn's wire format: the prose and trailers written into a
// commit (spec §4.1), and the reader that gets them back out.
//
// Trailers are parsed and written by git's own `interpret-trailers`, never by a
// hand-rolled parser, so `git log --grep`, `-S` and `--follow` keep working and
// a Cairn record coexists with any other tool's trailers.
package record

import (
	"fmt"
	"sort"
	"strings"

	"github.com/YUNGC0DE/Cairn/internal/distill"
	"github.com/YUNGC0DE/Cairn/internal/gitx"
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

// Prose line prefixes. They are prose, not trailers, because they are for the
// human reading `git log`; trailers are for machines.
const (
	rejectedPrefix  = "Rejected: "
	invariantPrefix = "Invariant: "
	openPrefix      = "Open: "
	nextPrefix      = "Next: "
	unverifiedLine  = "Cairn could not confirm against the diff: "
)

// Limits keep a record from swallowing the commit message (risk §9: "commit
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
	maxOpenRendered      = 8
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

// Body renders the prose half of a record: everything a human reads. Returns ""
// when there is nothing worth saying.
func Body(res *distill.Result) string {
	if res == nil || res.Extraction == nil {
		return ""
	}
	ex := res.Extraction
	var paras []string
	if ex.Intent != "" {
		paras = append(paras, wrap(ex.Intent))
	}
	if ex.Decision != "" && !strings.EqualFold(ex.Decision, ex.Intent) {
		paras = append(paras, wrap(ex.Decision))
	}

	if len(ex.Rejected) > 0 {
		var lines []string
		for i, r := range ex.Rejected {
			if i >= maxRejectedRendered {
				lines = append(lines, fmt.Sprintf("Rejected: (+%d more, see `cairn rejected`)", len(ex.Rejected)-i))
				break
			}
			lines = append(lines, wrap(rejectedPrefix+r.Option+" — "+r.Reason))
		}
		paras = append(paras, strings.Join(lines, "\n"))
	}

	if len(ex.Invariants) > 0 {
		var lines []string
		for i, c := range ex.Invariants {
			if i >= maxInvariantRendered {
				break
			}
			line := invariantPrefix + c.Text
			if len(c.Scope) > 0 {
				line += " (" + strings.Join(c.Scope, ", ") + ")"
			}
			lines = append(lines, wrap(line))
		}
		paras = append(paras, strings.Join(lines, "\n"))
	}

	// Open items and the next step feed `cairn resume` (spec §3.5). They are prose
	// so a human reading `git log` sees them too, and prefixed so the brief can be
	// assembled from them without a model.
	if len(ex.OpenItems) > 0 || ex.NextStep != "" {
		var lines []string
		for i, o := range ex.OpenItems {
			if i >= maxOpenRendered {
				break
			}
			lines = append(lines, wrap(openPrefix+o))
		}
		if ex.NextStep != "" {
			lines = append(lines, wrap(nextPrefix+ex.NextStep))
		}
		paras = append(paras, strings.Join(lines, "\n"))
	}

	// A contradicted claim is never rendered as fact. It is named, in the open,
	// so the next reader knows the record and the code disagree.
	if disputed := res.DisputedClaims(); len(disputed) > 0 {
		var lines []string
		for _, d := range disputed {
			lines = append(lines, wrap(unverifiedLine+d.Claim))
		}
		paras = append(paras, strings.Join(lines, "\n"))
	}

	return strings.Join(paras, "\n\n")
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
	Intent      string
	Rejected    []string
	Invariants  []string
	Open        []string
	Next        string
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

	// Body lines are hard-wrapped to git's conventional width, so a Rejected or
	// Invariant entry spans several lines and only the first carries the prefix.
	// Continuation lines belong to the block they follow until a blank line ends
	// it; treating them as prose is what makes a read-back look shuffled.
	var intent []string
	const (
		inProse = iota
		inRejected
		inInvariant
		inOpen
		inNext
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
	for _, line := range strings.Split(c.Body, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			block = inProse
		// Prose prefixes are matched before the trailer check on purpose. A short
		// "Invariant: …" line is shaped like a trailer, so git folds it into the
		// trailer block and its own parser reports it as one. Checking prefixes
		// first makes reading independent of how git happened to group the blocks.
		case strings.HasPrefix(trimmed, rejectedPrefix):
			block = inRejected
			rec.Rejected = append(rec.Rejected, strings.TrimPrefix(trimmed, rejectedPrefix))
		case strings.HasPrefix(trimmed, invariantPrefix):
			block = inInvariant
			rec.Invariants = append(rec.Invariants, strings.TrimPrefix(trimmed, invariantPrefix))
		case strings.HasPrefix(trimmed, openPrefix):
			block = inOpen
			rec.Open = append(rec.Open, strings.TrimPrefix(trimmed, openPrefix))
		case strings.HasPrefix(trimmed, nextPrefix):
			block = inNext
			rec.Next = strings.TrimPrefix(trimmed, nextPrefix)
		case strings.HasPrefix(trimmed, unverifiedLine):
			block = inDisputed
			rec.Disputed = append(rec.Disputed, strings.TrimPrefix(trimmed, unverifiedLine))
		case isTrailerLine(trailers, trimmed):
			block = inProse
		default:
			switch block {
			case inRejected:
				appendTo(&rec.Rejected, trimmed)
			case inInvariant:
				appendTo(&rec.Invariants, trimmed)
			case inOpen:
				appendTo(&rec.Open, trimmed)
			case inNext:
				rec.Next += " " + trimmed
			case inDisputed:
				appendTo(&rec.Disputed, trimmed)
			default:
				intent = append(intent, trimmed)
			}
		}
	}
	rec.Intent = strings.TrimSpace(strings.Join(intent, " "))
	rec.Disputed = Dedup(rec.Disputed)
	return rec, nil
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

// wrap hard-wraps a paragraph at git's conventional body width.
func wrap(s string) string {
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
			b.WriteString(w)
			lineLen = len(w)
		default:
			b.WriteByte(' ')
			b.WriteString(w)
			lineLen += 1 + len(w)
		}
	}
	return b.String()
}
