// Package transcript defines the agent-agnostic session model that every
// per-agent parser normalises into. Adding an agent means adding one parser in
// a subpackage; nothing else in cairn knows which agent produced a session.
package transcript

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Role of a message in a session.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// ToolCall is a normalised tool invocation. We keep only what distillation can
// use: the tool name, the files it touched and a short summary of the input.
type ToolCall struct {
	Name    string
	Files   []string
	Summary string // one-line rendering of the salient input (command, pattern…)
	Error   string // non-empty when the call failed
}

// Message is one normalised turn.
type Message struct {
	Role     Role
	Time     time.Time
	Text     string // visible text, parts concatenated
	Thinking string // extended thinking / reasoning, when the agent stores it
	Tools    []ToolCall
	Meta     bool // synthetic/system-injected content, not authored by the human
}

// Cursor marks how much of a session has already been consumed. Whichever
// field a parser uses, an empty Cursor means "from the beginning".
//
// Byte offsets work for append-only JSONL (Claude Code). Count works for
// content-addressed stores where the message list is rebuilt per turn (Cursor).
// Token carries any parser-specific opaque marker (e.g. a root blob id).
type Cursor struct {
	Bytes int64  `json:"bytes,omitempty"`
	Count int    `json:"count,omitempty"`
	Token string `json:"token,omitempty"`
}

// String renders a cursor for humans.
func (c Cursor) String() string {
	switch {
	case c.Bytes > 0:
		return fmt.Sprintf("byte %d", c.Bytes)
	case c.Count > 0:
		return fmt.Sprintf("message %d", c.Count)
	default:
		return "start"
	}
}

// Ref identifies a session on disk without loading it.
type Ref struct {
	Agent   string    // parser name, e.g. "claude-code"
	ID      string    // session id as the agent knows it
	Key     string    // stable key for offset bookkeeping (usually the path)
	Path    string    // file or directory holding the transcript
	CWD     string    // working directory the session ran in
	Title   string    // human title, when the agent stores one
	Updated time.Time // last modification
}

// Session is a loaded slice of a transcript: everything after the cursor it was
// loaded from.
type Session struct {
	Ref
	Model    string
	Branch   string
	Messages []Message
	Cursor   Cursor // cursor to persist after consuming this slice
	Complete bool   // true when loaded from the very beginning

	// Degraded marks a partial read — a parser fell back to a lesser source
	// because the primary one was unreadable. Callers surface the reason rather
	// than pretending the record is complete.
	Degraded       bool
	DegradedReason string

	// Approximate marks a slice assembled without per-message timestamps, so its
	// boundaries are inferred rather than known.
	Approximate bool
}

// Parser reads one agent's transcripts.
type Parser interface {
	// Name is the stable agent identifier written into trailers.
	Name() string
	// Discover finds sessions whose cwd lies inside repoRoot and that changed
	// at or after since. A zero since means no time filter.
	Discover(repoRoot string, since time.Time) ([]Ref, error)
	// Load reads a session slice starting at from.
	Load(ref Ref, from Cursor) (*Session, error)
}

// Pointerer is implemented by parsers whose sessions are not one file each.
//
// The default transcript pointer is the sha256 of Ref.Path, which assumes a
// session *is* a file. Cursor's IDE keeps every conversation in one shared
// multi-gigabyte database, where that hash would be both unreadable inside a
// commit hook and wrong — it would change whenever any other conversation did.
type Pointerer interface {
	// Pointer returns a "sha256:…" pointer to this session's content, or "".
	Pointer(ref Ref) string
}

// LastHumanPrompt returns the most recent genuine user message text, which is
// the best single proxy for intent.
func (s *Session) LastHumanPrompt() string {
	for i := len(s.Messages) - 1; i >= 0; i-- {
		m := s.Messages[i]
		if m.Role == RoleUser && !m.Meta && strings.TrimSpace(m.Text) != "" {
			return m.Text
		}
	}
	return ""
}

// Relevant keeps the sessions that wrote a file this commit is staging, and
// reports how many were left out.
//
// The staged file list is ground truth — cairn is holding the diff — so there is
// no need to guess from tool names whether a session "did work". A session that
// wrote none of these files did not produce this commit: it discussed the
// project, or it worked on something else that is not being committed. With a
// hundred conversations open on a repository, each of those would otherwise take
// an equal share of the prompt budget away from the sessions that did the work.
//
// Running a command is not writing a file. A build, a test run or a `git status`
// changes nothing that lands in the commit, and treating a terminal as an editor
// is what made an earlier version of this test keep every conversation.
//
// Two escape hatches, because a wrongly dropped session is work cairn can never
// explain while an extra one only costs budget: with nothing staged there is
// nothing to compare against, and when *no* session wrote a staged file — the
// human edited by hand, or edited through a shell — everything is kept.
func Relevant(sessions []*Session, staged []string) (kept []*Session, skipped int) {
	if len(staged) == 0 {
		return sessions, 0
	}
	for _, s := range sessions {
		if s.wroteAnyOf(staged) {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return sessions, 0
	}
	return kept, len(sessions) - len(kept)
}

// wroteAnyOf reports whether a non-read-only tool call in this session named one
// of the staged paths. Reading a file names it too, which is why the tool still
// has to be one that could change it.
func (s *Session) wroteAnyOf(staged []string) bool {
	for _, m := range s.Messages {
		for _, t := range m.Tools {
			if readOnlyTool(t.Name) {
				continue
			}
			for _, f := range t.Files {
				if isStaged(f, staged) {
					return true
				}
			}
		}
	}
	return false
}

// isStaged matches a path from a tool call against a repo-relative staged path.
// Agents record absolute paths, git records relative ones, so the comparison is
// by path suffix on a separator boundary — never a bare substring, which would
// match `auth/limit.go` against `internal/auth/limit.go.bak`.
func isStaged(touched string, staged []string) bool {
	t := filepath.ToSlash(filepath.Clean(touched))
	for _, s := range staged {
		s = filepath.ToSlash(filepath.Clean(s))
		if t == s || strings.HasSuffix(t, "/"+s) {
			return true
		}
	}
	return false
}

// readOnlyTool recognises the tools that cannot change a file, across the agents
// cairn reads: Read/Grep/Glob/WebFetch, read_file_v2, ripgrep_raw_search,
// glob_file_search, web_search. It exists because naming a file proves nothing —
// reading one names it too.
func readOnlyTool(tool string) bool {
	t := strings.ToLower(tool)
	if t == "" {
		return false
	}
	for _, verb := range []string{
		"read", "view", "grep", "search", "glob", "find", "list", "ls",
		"fetch", "lookup", "todo", "think", "plan", "diagnostic",
	} {
		if strings.Contains(t, verb) {
			return true
		}
	}
	return false
}

// TouchedFiles lists files the session's tool calls touched, most recent first.
func (s *Session) TouchedFiles() []string {
	seen := map[string]bool{}
	var out []string
	for i := len(s.Messages) - 1; i >= 0; i-- {
		for _, t := range s.Messages[i].Tools {
			for _, f := range t.Files {
				if f != "" && !seen[f] {
					seen[f] = true
					out = append(out, f)
				}
			}
		}
	}
	return out
}

// SortRefs orders refs oldest-first so distillation sees sessions in the order
// they happened.
func SortRefs(refs []Ref) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].Updated.Before(refs[j].Updated) })
}

// PathsIn reports whether cwd is repoRoot or below it.
//
// The two sides come from different places and do not always spell the same
// directory the same way: an agent records the path the developer actually
// worked in, while git reports the physical path. On macOS /tmp is a symlink to
// /private/tmp, and symlinked work trees are common everywhere — so a plain
// string comparison silently finds no sessions at all, and cairn records
// nothing while looking perfectly healthy. Compare literally first, then again
// with symlinks resolved.
func PathsIn(cwd, repoRoot string) bool {
	if cwd == "" || repoRoot == "" {
		return false
	}
	if within(cwd, repoRoot) {
		return true
	}
	return within(Resolve(cwd), Resolve(repoRoot))
}

func within(cwd, repoRoot string) bool {
	cwd = strings.TrimSuffix(filepath.Clean(cwd), "/")
	repoRoot = strings.TrimSuffix(filepath.Clean(repoRoot), "/")
	return cwd == repoRoot || strings.HasPrefix(cwd, repoRoot+"/")
}

// Resolve returns the physical path, falling back to a cleaned path when it
// cannot be resolved — a transcript can name a directory that no longer exists.
func Resolve(path string) string {
	if path == "" {
		return ""
	}
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return filepath.Clean(path)
}
