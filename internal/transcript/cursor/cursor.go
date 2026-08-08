// Package cursor parses Cursor transcripts.
//
// Layout: ~/.cursor/projects/<mangled-cwd>/agent-transcripts/<id>/<id>.jsonl —
// one JSON object per line, append-only:
//
//	{"role":"user","message":{"content":[{"type":"text","text":"…"}]}}
//	{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read",
//	                                          "input":{"path":"…"}}]}}
//	{"type":"turn_ended","status":"success"}
//
// One file covers both ways Cursor is used. The editor and `cursor-agent` write
// the same transcript here — measured on this machine, 106 of 107 CLI sessions
// in ~/.cursor/chats have their counterpart under agent-transcripts, and the
// editor's own conversations appear here and nowhere else.
//
// This replaced two SQLite readers: one walking the CLI's content-addressed blob
// DAG through a hand-decoded protobuf, the other running indexed queries against
// a 5.4 GB globalStorage/state.vscdb. Both were reading, by a longer route, what
// this file already holds in plain text.
package cursor

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YUNGC0DE/git-cairn/internal/transcript"
)

// Name is the agent identifier written into Cairn-Agent trailers.
const Name = "cursor"

// Parser reads Cursor JSONL transcripts.
type Parser struct {
	// Root overrides ~/.cursor/projects (tests, CAIRN_CURSOR_ROOT).
	Root string
}

// New returns a parser rooted at the user's Cursor project directory.
func New() *Parser { return &Parser{Root: DefaultRoot()} }

// DefaultRoot is ~/.cursor/projects, overridable with CAIRN_CURSOR_ROOT.
func DefaultRoot() string {
	if d := os.Getenv("CAIRN_CURSOR_ROOT"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cursor", "projects")
}

func (p *Parser) Name() string { return Name }

// transcriptsDir is the subdirectory of a project holding its sessions. A
// project directory also holds terminals, MCP state and a worker log, none of
// which are transcripts.
const transcriptsDir = "agent-transcripts"

// Discover lists the sessions belonging to repoRoot.
//
// A project directory is named for the directory Cursor was opened on, with
// every non-alphanumeric rune replaced by '-', so the repo's own name and the
// names of sessions started in a subdirectory all share one prefix. Both
// spellings of the repo path are tried: git reports the physical path while
// Cursor named the directory the developer opened, and those differ wherever a
// symlink is involved.
func (p *Parser) Discover(repoRoot string, since time.Time) ([]transcript.Ref, error) {
	if p.Root == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(p.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // Cursor is not installed here: not an error
		}
		return nil, err
	}
	wants := []string{slug(repoRoot)}
	if resolved := transcript.Resolve(repoRoot); resolved != repoRoot {
		wants = append(wants, slug(resolved))
	}

	var refs []transcript.Ref
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cwd := projectCWD(e.Name(), repoRoot, wants)
		if cwd == "" {
			continue
		}
		found, err := p.sessionsIn(filepath.Join(p.Root, e.Name(), transcriptsDir), cwd, since)
		if err != nil {
			continue
		}
		refs = append(refs, found...)
	}
	transcript.SortRefs(refs)
	return refs, nil
}

// projectCWD reports which directory a project name stands for, or "" when it is
// not this repository's.
//
// An exact match is the repository itself. A longer name with the same prefix is
// a session started somewhere below it — but only if the remainder really names
// a directory that exists there, because '-' in a project name spells both '/'
// and a literal hyphen, so `…-Cairn-bench` is either `Cairn/bench` or a sibling
// repository called `Cairn-bench`. Resolving it against the filesystem is what
// tells those apart; guessing would attribute another project's sessions to this
// one and distil them into its commits.
func projectCWD(name, repoRoot string, wants []string) string {
	for _, want := range wants {
		if name == want {
			return repoRoot
		}
		if !strings.HasPrefix(name, want+"-") {
			continue
		}
		if sub := resolveUnder(repoRoot, strings.Split(name[len(want)+1:], "-")); sub != "" {
			return sub
		}
	}
	return ""
}

// resolveUnder walks slug segments back into a real path under root, trying each
// separator the mangling could have erased. Depth is bounded by the segment
// count, and every step is a stat on a path that must already exist, so a wrong
// branch dies immediately rather than fanning out.
func resolveUnder(root string, segments []string) string {
	if len(segments) == 0 {
		return root
	}
	for i := 1; i <= len(segments); i++ {
		// The first i segments, rejoined with the hyphen the mangling may have kept.
		next := filepath.Join(root, strings.Join(segments[:i], "-"))
		if st, err := os.Stat(next); err != nil || !st.IsDir() {
			continue
		}
		if found := resolveUnder(next, segments[i:]); found != "" {
			return found
		}
	}
	return ""
}

// sessionsIn lists the transcripts of one project directory.
func (p *Parser) sessionsIn(dir, cwd string, since time.Time) ([]transcript.Ref, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var refs []transcript.Ref
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), e.Name()+".jsonl")
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			continue
		}
		if !since.IsZero() && info.ModTime().Before(since) {
			continue
		}
		refs = append(refs, transcript.Ref{
			Agent:   Name,
			ID:      e.Name(),
			Key:     Name + ":" + path,
			Path:    path,
			CWD:     cwd,
			Updated: info.ModTime(),
		})
	}
	return refs, nil
}

// slug mangles a path the way Cursor names its project directories.
func slug(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.TrimPrefix(b.String(), "-")
}

// line is the subset of a transcript line cairn reads. A line is either a
// message or a turn marker; the markers carry nothing a record could use.
type line struct {
	Role    string `json:"role"`
	Message struct {
		Content []part `json:"content"`
	} `json:"message"`
}

type part struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Load reads everything after from.Bytes. A file shorter than the stored offset
// means the transcript was rotated or rewritten, so we restart from zero.
func (p *Parser) Load(ref transcript.Ref, from transcript.Cursor) (*transcript.Session, error) {
	f, err := os.Open(ref.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	start := from.Bytes
	if start > info.Size() {
		start = 0
	}
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return nil, err
		}
	}

	s := &transcript.Session{Ref: ref, Complete: start == 0}
	r := bufio.NewReaderSize(f, 64*1024)
	consumed := start
	for {
		text, err := r.ReadString('\n')
		if !strings.HasSuffix(text, "\n") {
			// An agent writing this very moment leaves a partial last line. Stop
			// short of it so the next run re-reads it once it is complete; a
			// half-parsed turn is how you lose a whole session's worth of context.
			break
		}
		consumed += int64(len(text))
		var l line
		if json.Unmarshal([]byte(text), &l) != nil {
			continue // a turn marker, or a corrupt line: keep the offset moving
		}
		if m := convert(l, ref.Updated); m != nil {
			s.Messages = append(s.Messages, *m)
		}
		if err != nil {
			break
		}
	}
	s.Cursor = transcript.Cursor{Bytes: consumed}
	// The transcript records neither timestamps nor the model that answered: the
	// message boundaries are the file's, but their times are the file's mtime.
	s.Approximate = true
	return s, nil
}

// convert normalises one line.
func convert(l line, when time.Time) *transcript.Message {
	msg := transcript.Message{Time: when}
	switch l.Role {
	case "assistant":
		msg.Role = transcript.RoleAssistant
	case "user":
		msg.Role = transcript.RoleUser
	default:
		return nil
	}
	var text []string
	for _, p := range l.Message.Content {
		switch p.Type {
		case "text":
			text = append(text, p.Text)
		case "tool_use":
			msg.Tools = append(msg.Tools, toolCall(p))
		}
	}
	msg.Text = strings.TrimSpace(strings.Join(text, "\n"))
	if msg.Role == transcript.RoleUser {
		msg.Text = unwrapQuery(msg.Text)
	}
	if msg.Text == "" && len(msg.Tools) == 0 {
		return nil
	}
	return &msg
}

// unwrapQuery strips the envelope Cursor wraps a typed prompt in.
//
// The human's words arrive as "<timestamp>…</timestamp>\n<user_query>…\n</user_query>",
// and a turn with no <user_query> at all is Cursor's own injected context rather
// than something the human typed. Distillation weighs the human's requests above
// everything else in a session, so mislabelling one costs the whole record its
// subject.
func unwrapQuery(s string) string {
	const open, close = "<user_query>", "</user_query>"
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	if j := strings.Index(rest, close); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

func toolCall(p part) transcript.ToolCall {
	tc := transcript.ToolCall{Name: p.Name}
	var in map[string]any
	if json.Unmarshal(p.Input, &in) != nil {
		return tc
	}
	tc.Files = filesFromInput(in)
	tc.Summary = summarize(in)
	return tc
}

func filesFromInput(in map[string]any) []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range []string{
		"path", "file_path", "filePath", "target_file", "targetFile", "notebook_path",
	} {
		v, ok := in[k].(string)
		if !ok || v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if edits, ok := in["edits"].([]any); ok {
		for _, e := range edits {
			if m, ok := e.(map[string]any); ok {
				out = append(out, filesFromInput(m)...)
			}
		}
	}
	return out
}

func summarize(in map[string]any) string {
	for _, k := range []string{"command", "pattern", "query", "search_term", "glob_pattern", "description", "explanation"} {
		if v, ok := in[k].(string); ok && v != "" {
			return transcript.Truncate(strings.Join(strings.Fields(v), " "), 200)
		}
	}
	return ""
}
