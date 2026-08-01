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
