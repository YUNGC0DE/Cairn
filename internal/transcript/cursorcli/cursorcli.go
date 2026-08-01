// Package cursorcli parses Cursor CLI (cursor-agent) transcripts.
//
// Layout: ~/.cursor/chats/<workspace-hash>/<session-uuid>/ containing
//   - meta.json           cwd, title, createdAtMs/updatedAtMs
//   - prompt_history.json flat list of human prompts
//   - store.db            SQLite: meta(key,value) + blobs(id,data)
//
// The store is a content-addressed DAG. meta['0'] holds session JSON with
// latestRootBlobId; that blob is a protobuf whose repeated field 1 is the
// ordered list of 32-byte message-blob ids. Each message blob is an AI-SDK
// message: {role, content:[{type:"text"|"reasoning"|"tool-call"|"tool-result"}]}.
//
// Only field 1 of the root blob is decoded — the rest is Cursor's own bookkeeping
// and is skipped, so added fields cannot break us.
package cursorcli

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YUNGC0DE/Cairn/internal/sqlitex"
	"github.com/YUNGC0DE/Cairn/internal/transcript"
)

// Name is the agent identifier written into Cairn-Agent trailers.
const Name = "cursor-cli"

// Parser reads Cursor CLI chat stores.
type Parser struct {
	// Root overrides ~/.cursor/chats (tests).
	Root string
}

// New returns a parser rooted at the user's Cursor chat directory.
func New() *Parser { return &Parser{Root: DefaultRoot()} }

// DefaultRoot is ~/.cursor/chats, overridable with CAIRN_CURSOR_ROOT.
func DefaultRoot() string {
	if d := os.Getenv("CAIRN_CURSOR_ROOT"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cursor", "chats")
}

func (p *Parser) Name() string { return Name }

type metaFile struct {
	SchemaVersion   int    `json:"schemaVersion"`
	Title           string `json:"title"`
	CWD             string `json:"cwd"`
	CreatedAtMs     int64  `json:"createdAtMs"`
	UpdatedAtMs     int64  `json:"updatedAtMs"`
	HasConversation bool   `json:"hasConversation"`
}

type sessionMeta struct {
	AgentID          string `json:"agentId"`
	LatestRootBlobID string `json:"latestRootBlobId"`
	Name             string `json:"name"`
	LastUsedModel    string `json:"lastUsedModel"`
}

// Discover lists Cursor sessions whose cwd is inside repoRoot.
func (p *Parser) Discover(repoRoot string, since time.Time) ([]transcript.Ref, error) {
	if p.Root == "" {
		return nil, nil
	}
	spaces, err := os.ReadDir(p.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var refs []transcript.Ref
	for _, space := range spaces {
		if !space.IsDir() {
			continue
		}
		sessions, err := os.ReadDir(filepath.Join(p.Root, space.Name()))
		if err != nil {
			continue
		}
		for _, s := range sessions {
			if !s.IsDir() {
				continue
			}
			dir := filepath.Join(p.Root, space.Name(), s.Name())
			var mf metaFile
			if err := readJSON(filepath.Join(dir, "meta.json"), &mf); err != nil {
				continue
			}
			if !transcript.PathsIn(mf.CWD, repoRoot) {
				continue
			}
			// Cursor leaves behind directories holding only a meta.json when a
			// session is opened and abandoned. There is nothing to read in them, so
			// listing them would only produce load errors on every commit.
			if !readable(dir) {
				continue
			}
			updated := msTime(mf.UpdatedAtMs)
			if updated.IsZero() {
				if st, err := os.Stat(filepath.Join(dir, "store.db")); err == nil {
					updated = st.ModTime()
				}
			}
			if !since.IsZero() && updated.Before(since) {
				continue
			}
			refs = append(refs, transcript.Ref{
				Agent:   Name,
				ID:      s.Name(),
				Key:     Name + ":" + dir,
				Path:    dir,
				CWD:     mf.CWD,
				Title:   mf.Title,
				Updated: updated,
			})
		}
	}
	transcript.SortRefs(refs)
	return refs, nil
}

// Load reads messages after from.Count. When the root blob is unchanged there is
// nothing new, and when the message list shrank (the user rewound the session)
// we restart from the beginning rather than guess.
func (p *Parser) Load(ref transcript.Ref, from transcript.Cursor) (*transcript.Session, error) {
	dbPath := filepath.Join(ref.Path, "store.db")
	if _, err := os.Stat(dbPath); err != nil {
		return p.loadPromptsOnly(ref, from, fmt.Errorf("no store.db: %w", err))
	}
	if !sqlitex.Available() {
		return p.loadPromptsOnly(ref, from, sqlitex.ErrUnavailable)
	}

	rows, err := sqlitex.Query(dbPath, `SELECT key, hex(value) FROM meta;`)
	if err != nil {
		return p.loadPromptsOnly(ref, from, err)
	}
	sm, err := parseSessionMeta(rows)
	if err != nil {
		return p.loadPromptsOnly(ref, from, err)
	}
	if sm.LatestRootBlobID == "" {
		return p.loadPromptsOnly(ref, from, errors.New("no latestRootBlobId"))
	}

	ids, err := childIDs(dbPath, sm.LatestRootBlobID)
	if err != nil {
		return p.loadPromptsOnly(ref, from, err)
	}

	s := &transcript.Session{Ref: ref, Model: sm.LastUsedModel}
	start := from.Count
	if start > len(ids) {
		start = 0 // session was rewound or branched: re-read rather than guess
	}
	s.Complete = start == 0
	s.Cursor = transcript.Cursor{Count: len(ids), Token: sm.LatestRootBlobID}
	if start == len(ids) {
		return s, nil // nothing new
	}

	want := ids[start:]
	blobs, err := fetchBlobs(dbPath, want)
	if err != nil {
		return p.loadPromptsOnly(ref, from, err)
	}
	for _, id := range want {
		raw, ok := blobs[id]
		if !ok {
			continue // pruned or encrypted blob: skip, never fail the commit
		}
		if m := convert(raw, ref.Updated); m != nil {
			s.Messages = append(s.Messages, *m)
		}
	}
	return s, nil
}

// loadPromptsOnly is the documented degradation path: without a readable store
// we still have the human prompts, which carry most of the intent signal.
func (p *Parser) loadPromptsOnly(ref transcript.Ref, from transcript.Cursor, cause error) (*transcript.Session, error) {
	var prompts []string
	if err := readJSON(filepath.Join(ref.Path, "prompt_history.json"), &prompts); err != nil {
		return nil, fmt.Errorf("cursorcli: %w (and no prompt_history.json: %v)", cause, err)
	}
	s := &transcript.Session{Ref: ref, Degraded: true, DegradedReason: cause.Error()}
	start := from.Count
	if start > len(prompts) {
		start = 0
	}
	s.Complete = start == 0
	for _, pr := range prompts[start:] {
		if strings.TrimSpace(pr) == "" {
			continue
		}
		s.Messages = append(s.Messages, transcript.Message{
			Role: transcript.RoleUser,
			Text: pr,
			Time: ref.Updated,
		})
	}
	s.Cursor = transcript.Cursor{Count: len(prompts)}
	return s, nil
}

func parseSessionMeta(rows [][]string) (sessionMeta, error) {
	var sm sessionMeta
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		raw, err := hex.DecodeString(r[1])
		if err != nil {
			continue
		}
		// Values are JSON; some Cursor builds store them hex-encoded inside TEXT.
		var candidate sessionMeta
		if json.Unmarshal(raw, &candidate) == nil && candidate.LatestRootBlobID != "" {
			return candidate, nil
		}
		if inner, err := hex.DecodeString(strings.TrimSpace(string(raw))); err == nil {
			if json.Unmarshal(inner, &candidate) == nil && candidate.LatestRootBlobID != "" {
				return candidate, nil
			}
		}
	}
	return sm, errors.New("cursorcli: session meta not found in store")
}

// childIDs decodes the ordered message-blob ids from a root blob.
func childIDs(dbPath, rootID string) ([]string, error) {
	blobs, err := fetchBlobs(dbPath, []string{rootID})
	if err != nil {
		return nil, err
	}
	root, ok := blobs[rootID]
	if !ok {
		return nil, fmt.Errorf("cursorcli: root blob %s missing", rootID[:8])
	}
	return decodeChildList(root), nil
}

// decodeChildList reads the leading repeated bytes field 1 of a protobuf
// message and stops at the first other field.
func decodeChildList(b []byte) []string {
	var ids []string
	for i := 0; i < len(b); {
		tag, n := varint(b[i:])
		if n == 0 || tag != 0x0a { // field 1, wire type 2
			break
		}
		i += n
		size, n := varint(b[i:])
		if n == 0 {
			break
		}
		i += n
		if size > uint64(len(b)-i) {
			break
		}
		chunk := b[i : i+int(size)]
		i += int(size)
		if len(chunk) == 32 { // sha256 blob id
			ids = append(ids, hex.EncodeToString(chunk))
		}
	}
	return ids
}

func varint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

// fetchBlobs loads blobs by id in one query. ids come from the store itself, so
// they are hex strings; we validate anyway before interpolating.
func fetchBlobs(dbPath string, ids []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	const batch = 200
	for start := 0; start < len(ids); start += batch {
		end := min(start+batch, len(ids))
		var quoted []string
		for _, id := range ids[start:end] {
			if !isHex(id) {
				continue
			}
			quoted = append(quoted, "'"+id+"'")
		}
		if len(quoted) == 0 {
			continue
		}
		q := fmt.Sprintf("SELECT id, hex(data) FROM blobs WHERE id IN (%s);", strings.Join(quoted, ","))
		rows, err := sqlitex.Query(dbPath, q)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if len(r) < 2 {
				continue
			}
			data, err := hex.DecodeString(r[1])
			if err != nil {
				continue
			}
			out[r[0]] = data
		}
	}
	return out, nil
}

// aiMessage is the AI-SDK message shape Cursor persists.
type aiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type aiPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ToolName string          `json:"toolName"`
	Args     json.RawMessage `json:"args"`
	Input    json.RawMessage `json:"input"`
	IsError  bool            `json:"isError"`
	Result   json.RawMessage `json:"result"`
	Output   json.RawMessage `json:"output"`
}

func convert(raw []byte, when time.Time) *transcript.Message {
	raw = trimSpaceBytes(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil // binary or encrypted blob
	}
	var am aiMessage
	if json.Unmarshal(raw, &am) != nil {
		return nil
	}
	msg := transcript.Message{Time: when}
	switch am.Role {
	case "assistant":
		msg.Role = transcript.RoleAssistant
	case "user":
		msg.Role = transcript.RoleUser
	case "system":
		return nil // the harness system prompt: never useful, always huge
	default:
		msg.Role = transcript.RoleSystem
		msg.Meta = true
	}

	var str string
	if json.Unmarshal(am.Content, &str) == nil {
		// The first user turn is Cursor's injected <user_info> environment block.
		msg.Text = str
		msg.Meta = strings.Contains(str, "<user_info>")
		return &msg
	}
	var parts []aiPart
	if json.Unmarshal(am.Content, &parts) != nil {
		return nil
	}
	var text, think []string
	for _, part := range parts {
		switch part.Type {
		case "text":
			text = append(text, part.Text)
		case "reasoning":
			think = append(think, part.Text)
		case "tool-call":
			msg.Tools = append(msg.Tools, toolCall(part))
		case "tool-result":
			msg.Meta = true
			if part.IsError {
				msg.Tools = append(msg.Tools, transcript.ToolCall{
					Name:  orDefault(part.ToolName, "result"),
					Error: transcript.Truncate(string(part.Output)+string(part.Result), 400),
				})
			}
		}
	}
	msg.Text = strings.TrimSpace(strings.Join(text, "\n"))
	msg.Thinking = strings.TrimSpace(strings.Join(think, "\n"))
	if msg.Text == "" && msg.Thinking == "" && len(msg.Tools) == 0 {
		return nil
	}
	return &msg
}

func toolCall(part aiPart) transcript.ToolCall {
	tc := transcript.ToolCall{Name: part.ToolName}
	args := part.Args
	if len(args) == 0 {
		args = part.Input
	}
	var in map[string]any
	if json.Unmarshal(args, &in) != nil {
		// Some providers hand back args as a JSON string.
		var s string
		if json.Unmarshal(args, &s) != nil || json.Unmarshal([]byte(s), &in) != nil {
			return tc
		}
	}
	tc.Files = filesFromInput(in)
	tc.Summary = summarize(in)
	return tc
}

func filesFromInput(in map[string]any) []string {
	var out []string
	for _, k := range []string{"path", "file_path", "target_file", "filePath", "notebook_path"} {
		if v, ok := in[k].(string); ok && v != "" {
			out = append(out, v)
		}
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
	for _, k := range []string{"command", "pattern", "query", "instructions", "explanation"} {
		if v, ok := in[k].(string); ok && v != "" {
			return transcript.Truncate(strings.Join(strings.Fields(v), " "), 200)
		}
	}
	return ""
}

// readable reports whether a session directory holds anything cairn can parse.
func readable(dir string) bool {
	for _, name := range []string{"store.db", "prompt_history.json"} {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && st.Size() > 0 {
			return true
		}
	}
	return false
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func trimSpaceBytes(b []byte) []byte { return []byte(strings.TrimSpace(string(b))) }

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
