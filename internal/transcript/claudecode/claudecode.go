// Package claudecode parses Claude Code transcripts.
//
// Layout: ~/.claude/projects/<mangled-cwd>/<session-uuid>.jsonl — one JSON
// object per line, append-only. Every code-bearing entry carries `cwd`, so we
// never have to trust the directory name; the mangled name is only a prefilter.
package claudecode

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
const Name = "claude-code"

// Parser reads Claude Code JSONL transcripts.
type Parser struct {
	// Root overrides ~/.claude/projects (tests, CLAUDE_CONFIG_DIR).
	Root string
}

// New returns a parser rooted at the user's Claude Code project directory.
func New() *Parser { return &Parser{Root: DefaultRoot()} }

// DefaultRoot resolves the transcript directory.
//
// CAIRN_CLAUDE_ROOT points straight at a projects directory. It exists because
// CLAUDE_CONFIG_DIR also relocates credentials: exporting that to redirect
// cairn's *reads* would log out the headless agent cairn *runs*.
func DefaultRoot() string {
	if d := os.Getenv("CAIRN_CLAUDE_ROOT"); d != "" {
		return d
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

func (p *Parser) Name() string { return Name }

// Discover lists sessions whose cwd is inside repoRoot.
func (p *Parser) Discover(repoRoot string, since time.Time) ([]transcript.Ref, error) {
	if p.Root == "" {
		return nil, nil
	}
	dirs, err := os.ReadDir(p.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Prefilter by the mangled directory name, which is the repo path with every
	// non-alphanumeric rune replaced by '-'. A session started in a subdirectory
	// gets a longer name with the same prefix.
	//
	// Both spellings of the repo path are tried: git reports the physical path
	// while the agent named the directory the developer typed, and those differ
	// wherever a symlink is involved.
	wants := []string{Slug(repoRoot)}
	if resolved := transcript.Resolve(repoRoot); resolved != repoRoot {
		wants = append(wants, Slug(resolved))
	}
	var candidates []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		n := d.Name()
		for _, want := range wants {
			if n == want || strings.HasPrefix(n, want+"-") {
				candidates = append(candidates, n)
				break
			}
		}
	}
	// Distinct paths can mangle to the same name and mangling may change between
	// Claude Code versions; if the prefilter finds nothing, fall back to
	// inspecting every directory. Only a few lines per file are read either way.
	if len(candidates) == 0 {
		for _, d := range dirs {
			if d.IsDir() {
				candidates = append(candidates, d.Name())
			}
		}
	}

	var refs []transcript.Ref
	for _, dir := range candidates {
		entries, err := os.ReadDir(filepath.Join(p.Root, dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if !since.IsZero() && info.ModTime().Before(since) {
				continue
			}
			path := filepath.Join(p.Root, dir, e.Name())
			cwd, title := peek(path)
			switch {
			case cwd != "":
				if !transcript.PathsIn(cwd, repoRoot) {
					continue
				}
			case matchesAny(dir, wants):
				// A transcript can carry no cwd at all — a session that was opened
				// and abandoned, or one whose head is all bookkeeping. The directory
				// name is derived from the cwd, so it is the fallback signal. Such a
				// session usually has no content either and gets dropped later.
				cwd = repoRoot
			default:
				continue
			}
			refs = append(refs, transcript.Ref{
				Agent:   Name,
				ID:      strings.TrimSuffix(e.Name(), ".jsonl"),
				Key:     Name + ":" + path,
				Path:    path,
				CWD:     cwd,
				Title:   title,
				Updated: info.ModTime(),
			})
		}
	}
	transcript.SortRefs(refs)
	return refs, nil
}

func matchesAny(dir string, wants []string) bool {
	for _, w := range wants {
		if dir == w {
			return true
		}
	}
	return false
}

// Slug mangles a path the way Claude Code names its project directories.
func Slug(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// peekLines bounds how far into a transcript the cwd probe reads.
const peekLines = 200

// peek reads the head of a transcript for its cwd and title without parsing the
// whole file.
func peek(path string) (cwd, title string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	// Enough lines to get past a long run of bookkeeping entries at the head of a
	// transcript, while still reading only a fraction of a large file.
	for i := 0; sc.Scan() && i < peekLines; i++ {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if cwd == "" && e.CWD != "" {
			cwd = e.CWD
		}
		if title == "" && e.AITitle != "" {
			title = e.AITitle
		}
		if cwd != "" && title != "" {
			break
		}
	}
	return cwd, title
}

// entry is the subset of a transcript line cairn reads.
type entry struct {
	Type        string          `json:"type"`
	CWD         string          `json:"cwd"`
	GitBranch   string          `json:"gitBranch"`
	SessionID   string          `json:"sessionId"`
	Timestamp   string          `json:"timestamp"`
	IsSidechain bool            `json:"isSidechain"`
	PromptID    string          `json:"promptId"`
	AITitle     string          `json:"aiTitle"`
	Message     json.RawMessage `json:"message"`
	ToolResult  json.RawMessage `json:"toolUseResult"`
}

type apiMessage struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type contentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	IsError  bool            `json:"is_error"`
	Content  json.RawMessage `json:"content"`
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
		line, err := r.ReadString('\n')
		if !strings.HasSuffix(line, "\n") {
			// An agent writing this very moment leaves a partial last line. Stop
			// short of it so the next run re-reads it once it is complete; a
			// half-parsed turn is how you lose a whole session's worth of context.
			break
		}
		consumed += int64(len(line))
		var e entry
		if jsonErr := json.Unmarshal([]byte(line), &e); jsonErr != nil {
			continue // unknown or corrupt line: skip it, keep the offset moving
		}
		if e.IsSidechain {
			continue // subagent chatter: noise for distillation
		}
		m, model := convert(e)
		if m != nil {
			s.Messages = append(s.Messages, *m)
		}
		if model != "" {
			s.Model = model
		}
		if e.GitBranch != "" {
			s.Branch = e.GitBranch
		}
		if e.SessionID != "" {
			s.ID = e.SessionID
		}
		if err != nil {
			break
		}
	}
	s.Cursor = transcript.Cursor{Bytes: consumed}
	return s, nil
}

// convert normalises one transcript line, also reporting the model that
// produced it when the line names one.
func convert(e entry) (*transcript.Message, string) {
	if e.Type != "user" && e.Type != "assistant" {
		return nil, ""
	}
	var am apiMessage
	if len(e.Message) == 0 || json.Unmarshal(e.Message, &am) != nil {
		return nil, ""
	}
	msg := transcript.Message{}
	switch am.Role {
	case "assistant":
		msg.Role = transcript.RoleAssistant
	case "user":
		msg.Role = transcript.RoleUser
	default:
		msg.Role = transcript.RoleSystem
	}
	if t, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
		msg.Time = t
	}

	// content is either a bare string (human prompt) or a list of parts.
	var str string
	if json.Unmarshal(am.Content, &str) == nil {
		msg.Text = str
		return &msg, am.Model
	}
	var parts []contentPart
	if json.Unmarshal(am.Content, &parts) != nil {
		return nil, am.Model
	}
	var text, think []string
	for _, part := range parts {
		switch part.Type {
		case "text":
			text = append(text, part.Text)
		case "thinking":
			think = append(think, part.Thinking)
		case "tool_use":
			msg.Tools = append(msg.Tools, toolCall(part))
		case "tool_result":
			// Results are noise unless they failed; a failure is what makes an
			// agent change course, which is exactly what we want to capture.
			msg.Meta = true
			if part.IsError {
				msg.Tools = append(msg.Tools, transcript.ToolCall{
					Name:  "result",
					Error: truncate(rawText(part.Content), 400),
				})
			}
		}
	}
	msg.Text = strings.TrimSpace(strings.Join(text, "\n"))
	msg.Thinking = strings.TrimSpace(strings.Join(think, "\n"))
	if msg.Role == transcript.RoleUser && e.PromptID == "" && len(parts) > 0 {
		// Not a typed prompt: tool results, IDE context, hook output.
		msg.Meta = true
	}
	if msg.Text == "" && msg.Thinking == "" && len(msg.Tools) == 0 {
		return nil, am.Model
	}
	return &msg, am.Model
}

func toolCall(part contentPart) transcript.ToolCall {
	tc := transcript.ToolCall{Name: part.Name}
	var in map[string]any
	if json.Unmarshal(part.Input, &in) != nil {
		return tc
	}
	tc.Files = FilesFromInput(in)
	tc.Summary = SummarizeInput(part.Name, in)
	return tc
}

// FilesFromInput pulls file paths out of a tool input by well-known key names.
func FilesFromInput(in map[string]any) []string {
	var out []string
	for _, k := range []string{"file_path", "filePath", "path", "target_file", "notebook_path"} {
		if v, ok := in[k].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	// MultiEdit-style batches.
	if edits, ok := in["edits"].([]any); ok {
		for _, e := range edits {
			if m, ok := e.(map[string]any); ok {
				out = append(out, FilesFromInput(m)...)
			}
		}
	}
	return out
}

// SummarizeInput renders the salient part of a tool input on one line.
func SummarizeInput(name string, in map[string]any) string {
	for _, k := range []string{"command", "pattern", "query", "prompt", "description", "url"} {
		if v, ok := in[k].(string); ok && v != "" {
			return truncate(collapse(v), 200)
		}
	}
	_ = name
	return ""
}

func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []contentPart
	if json.Unmarshal(raw, &parts) == nil {
		var b []string
		for _, p := range parts {
			if p.Text != "" {
				b = append(b, p.Text)
			}
		}
		return strings.Join(b, "\n")
	}
	return string(raw)
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
