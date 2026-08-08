// Package record is Cairn's wire format: the rules and trailers written into a
// commit, and the reader that gets them back out.
//
// Trailers are parsed and written by git's own `interpret-trailers`, never by a
// hand-rolled parser, so `git log --grep`, `-S` and `--follow` keep working and
// a Cairn record coexists with any other tool's trailers.
package record

import (
	"sort"
	"strings"
	"time"

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
// and nothing else does. A closing tag on its own also ends the message with a
// paragraph that is not trailer-shaped, so `interpret-trailers` appends a clean
// trailer block after it instead of merging into whatever the record's last line
// happened to look like.
const (
	OpenTag  = "<git-cairn>"
	CloseTag = "</git-cairn>"
)

// Field keys inside the block. A key sits at column zero and its continuation
// lines are indented, which is the whole grammar: no key can be confused with a
// wrapped line, and no wrapped line can be confused with a git trailer.
const (
	RejectKey    = "reject:"
	InvariantKey = "invariant:"
	whyKey       = "why:"
	fileKey      = "file:"
)

// Body renders the record as one <git-cairn> block. Returns "" when there is
// nothing worth saying — an empty block is worse than no block, because a reader
// who finds one stops looking for the reasoning elsewhere.
//
// Every rule is one paragraph: the rule itself at column zero, then the
// justification and the files it binds, indented under it. The rule line is the
// only part the reactive channel ever serves; the rest is here for whoever
// follows the commit back.
func Body(res *distill.Result) string {
	if res == nil || res.Extraction == nil {
		return ""
	}
	ex := res.Extraction
	var paras []string
	for _, r := range ex.Rejected {
		paras = append(paras, rule(RejectKey, r))
	}
	for _, r := range ex.Invariants {
		paras = append(paras, rule(InvariantKey, r))
	}
	if len(paras) == 0 {
		return ""
	}
	return OpenTag + "\n" + strings.Join(paras, "\n\n") + "\n" + CloseTag
}

// rule renders one entry: the instruction, why it holds, and what it binds.
func rule(key string, r distill.Rule) string {
	out := field(key, r.Rule)
	if w := strings.TrimSpace(r.Why); w != "" {
		out += "\n" + subfield(whyKey, w)
	}
	if len(r.Files) > 0 {
		// Never wrapped, however long the list gets. A wrapped path list cannot be
		// read back unambiguously — a continuation line is indistinguishable from
		// the tail of a wrapped sentence — so the one line that must survive a
		// round trip intact is the one line that does not wrap.
		out += "\n  " + fileKey + " " + strings.Join(r.Files, ", ")
	}
	return out
}

// field renders "key: value", wrapping continuation lines under a two-space
// indent so the grammar stays unambiguous: column zero starts a field, anything
// indented continues one.
func field(key, value string) string {
	return wrapIndent(key+" "+strings.TrimSpace(value), "", "  ")
}

// subfield renders a key that belongs to the entry above it, one level in.
//
// The first-line indent has to be passed in rather than glued onto the text:
// wrapping splits on whitespace, so a leading indent inside the string is simply
// dropped — which is how "  why:" would ship at column zero and break the one
// rule the grammar has.
func subfield(key, value string) string {
	return wrapIndent(key+" "+strings.TrimSpace(value), "  ", "    ")
}

// Meta is the machine-readable half of a record.
type Meta struct {
	Agent       string // "claude-code/opus-5"
	Sessions    []string
	Files       []string
	Transcripts []string
	Confidence  distill.Confidence
	Disputed    []string
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
// record, then trailers. The author's subject and body are never rewritten —
// Cairn appends, so a human's words always survive.
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

// Entry is one parsed rule.
type Entry struct {
	Rule  string   `json:"rule"`
	Why   string   `json:"why,omitempty"`
	Files []string `json:"files,omitempty"`
}

// Binds reports whether this rule is about the given repo-relative path.
//
// A rule with no file binds nothing. That is the whole point of a file-level
// record: an entry that reaches every reader of every commit that happened to
// touch this file is the noise the format exists to remove.
//
// The base-name fallback is for a file that moved. Callers only ask this of
// commits `git log --follow -- <path>` already attributed to that one file, so
// within that set a matching base name is the same file under a new directory,
// not a coincidence.
func (e Entry) Binds(path string) bool {
	path = strings.TrimPrefix(path, "./")
	base := path[strings.LastIndexByte(path, '/')+1:]
	for _, f := range e.Files {
		f = strings.TrimPrefix(f, "./")
		if f == path || f[strings.LastIndexByte(f, '/')+1:] == base {
			return true
		}
	}
	return false
}

// Record is a parsed record read back out of a commit.
type Record struct {
	SHA         string
	Short       string
	Subject     string
	When        time.Time
	Author      string
	Rejected    []Entry
	Invariants  []Entry
	Disputed    []string
	Agent       string
	Sessions    []string
	Files       []string
	Transcripts []string
	Confidence  string
}

// Has reports whether a commit carries a Cairn record at all.
func (r *Record) Has() bool { return r != nil && r.Agent != "" }

// Rules returns every entry the record carries that binds path, rejections
// first. Callers serving an agent want exactly this and nothing else.
func (r *Record) Rules(path string) (rejected, invariants []Entry) {
	if r == nil {
		return nil, nil
	}
	for _, e := range r.Rejected {
		if e.Binds(path) {
			rejected = append(rejected, e)
		}
	}
	for _, e := range r.Invariants {
		if e.Binds(path) {
			invariants = append(invariants, e)
		}
	}
	return rejected, invariants
}

// Parse reads a record out of a commit message. Trailers come from git's own
// parser; the rules come from the <git-cairn> block.
func Parse(repoDir string, c gitx.Commit) (*Record, error) {
	rec := &Record{SHA: c.SHA, Short: c.Short, Subject: c.Subject, When: c.When, Author: c.Author}
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
	}
	rec.Disputed = Dedup(rec.Disputed)
	return rec, nil
}

// blockIn returns the contents of the <git-cairn> block, if the message has one.
//
// The tag only counts at the start of a line. Cairn writes it that way, and the
// tag can legitimately appear mid-text elsewhere in a message: a contradicted
// claim about this very format lands in a Cairn-Disputed trailer quoting both
// tags, and matching that would parse a trailer as the record.
func blockIn(msg string) (string, bool) {
	i := lineIndex(msg, OpenTag)
	if i < 0 {
		return "", false
	}
	rest := msg[i+len(OpenTag):]
	if j := lineIndex(rest, CloseTag); j >= 0 {
		return rest[:j], true
	}
	// An unterminated block is still a block: a truncated message should give up
	// what rules it has rather than none at all.
	return rest, true
}

// parseBlock reads the block grammar: a key at column zero opens an entry, an
// indented line either opens a subfield or continues the line above it, and a
// blank line closes the entry.
func parseBlock(rec *Record, body string) {
	var cur *Entry   // the entry being built
	var cont *string // the string an indented continuation line extends
	open := func(list *[]Entry, text string) {
		*list = append(*list, Entry{Rule: text})
		cur = &(*list)[len(*list)-1]
		cont = &cur.Rule
	}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		indented := line != "" && (line[0] == ' ' || line[0] == '\t')
		switch {
		case trimmed == "":
			cur, cont = nil, nil
		case !indented && hasKey(trimmed, RejectKey):
			open(&rec.Rejected, value(trimmed, RejectKey))
		case !indented && hasKey(trimmed, InvariantKey):
			open(&rec.Invariants, value(trimmed, InvariantKey))
		case cur == nil:
			// A line outside any entry: the author's own words, or a stray.
		case hasKey(trimmed, whyKey):
			cur.Why = value(trimmed, whyKey)
			cont = &cur.Why
		case hasKey(trimmed, fileKey):
			cur.Files = append(cur.Files, splitList(value(trimmed, fileKey))...)
			cont = nil // a wrapped path list would be ambiguous; do not guess
		case cont != nil:
			*cont += " " + trimmed
		}
	}
}

// lineIndex finds tag where it begins a line, or -1.
func lineIndex(s, tag string) int {
	for i := 0; ; {
		j := strings.Index(s[i:], tag)
		if j < 0 {
			return -1
		}
		at := i + j
		if at == 0 || s[at-1] == '\n' {
			return at
		}
		i = at + len(tag)
	}
}

func hasKey(line, key string) bool {
	return strings.HasPrefix(strings.ToLower(line), key)
}

func value(line, key string) string {
	return strings.TrimSpace(line[len(key):])
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
func wrapIndent(s, first, cont string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	lineLen := 0
	for i, w := range words {
		switch {
		case i == 0:
			b.WriteString(first)
			b.WriteString(w)
			lineLen = len(first) + len(w)
		case lineLen+1+len(w) > wrapAt:
			b.WriteByte('\n')
			b.WriteString(cont)
			b.WriteString(w)
			lineLen = len(cont) + len(w)
		default:
			b.WriteByte(' ')
			b.WriteString(w)
			lineLen += 1 + len(w)
		}
	}
	return b.String()
}
