// Package cursoride parses Cursor IDE (the desktop app) transcripts.
//
// Layout, macOS spelling:
//
//	~/Library/Application Support/Cursor/User/
//	  globalStorage/state.vscdb              every conversation, all workspaces
//	  workspaceStorage/<hash>/workspace.json  hash → folder URI: the repo match
//
// state.vscdb is a key-value store. `composerData:<composerId>` holds one
// conversation — its ordered bubble list in `fullConversationHeadersOnly` —
// and `bubbleId:<composerId>:<bubbleId>` holds each message. composerHeaders
// indexes conversations by workspace, which is what makes discovery a lookup
// rather than a scan.
//
// That file reaches several gigabytes on a machine that has been used for a
// while, so two rules hold everywhere in this package: every read is a targeted
// query by key, and the database is opened read-only in place, never copied.
package cursoride

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/YUNGC0DE/Cairn/internal/sqlitex"
	"github.com/YUNGC0DE/Cairn/internal/transcript"
)

// Name is the agent identifier written into Cairn-Agent trailers.
const Name = "cursor-ide"

// Parser reads the Cursor IDE state store.
type Parser struct {
	// Root is the Cursor "User" directory holding globalStorage and
	// workspaceStorage (tests, CAIRN_CURSOR_IDE_ROOT).
	Root string
}

// New returns a parser rooted at the user's Cursor profile.
func New() *Parser { return &Parser{Root: DefaultRoot()} }

// DefaultRoot locates Cursor's User directory, overridable with
// CAIRN_CURSOR_IDE_ROOT.
func DefaultRoot() string {
	if d := os.Getenv("CAIRN_CURSOR_IDE_ROOT"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User")
	case "windows":
		if d := os.Getenv("APPDATA"); d != "" {
			return filepath.Join(d, "Cursor", "User")
		}
		return filepath.Join(home, "AppData", "Roaming", "Cursor", "User")
	default:
		return filepath.Join(home, ".config", "Cursor", "User")
	}
}

func (p *Parser) Name() string { return Name }

// GlobalDB is the state store holding every conversation.
func (p *Parser) GlobalDB() string {
	if p.Root == "" {
		return ""
	}
	return filepath.Join(p.Root, "globalStorage", "state.vscdb")
}

// Discover lists the conversations belonging to workspaces open on repoRoot.
//
// Two lookups, both targeted: the workspace hashes come from small JSON files,
// and the conversations from one indexed query per commit.
func (p *Parser) Discover(repoRoot string, since time.Time) ([]transcript.Ref, error) {
	db := p.GlobalDB()
	if db == "" {
		return nil, nil
	}
	if _, err := os.Stat(db); err != nil {
		return nil, nil // Cursor is not installed here: not an error
	}
	spaces := p.workspacesIn(repoRoot)
	if len(spaces) == 0 {
		return nil, nil
	}
	if !sqlitex.Available() {
		return nil, sqlitex.ErrUnavailable
	}

	heads, err := composersFor(db, spaces)
	if err != nil {
		return nil, err
	}
	var refs []transcript.Ref
	for _, h := range heads {
		// A conversation with no recorded time cannot be shown to postdate the
		// last commit, and replaying an old one into a fresh record is worse than
		// missing it. Unfiltered listings (`cairn sessions --all`) still see it.
		if !since.IsZero() && (h.updated.IsZero() || h.updated.Before(since)) {
			continue
		}
		refs = append(refs, transcript.Ref{
			Agent:   Name,
			ID:      h.id,
			Key:     Name + ":" + h.id,
			Path:    db,
			CWD:     spaces[h.workspace],
			Title:   h.title,
			Updated: h.updated,
		})
	}
	transcript.SortRefs(refs)
	return refs, nil
}

// workspacesIn maps workspace hash → folder for every Cursor window opened on
// repoRoot. workspace.json is a two-line file, so reading all of them costs
// less than one SQLite query.
func (p *Parser) workspacesIn(repoRoot string) map[string]string {
	out := map[string]string{}
	dir := filepath.Join(p.Root, "workspaceStorage")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var ws struct {
			Folder string `json:"folder"`
		}
		if err := readJSON(filepath.Join(dir, e.Name(), "workspace.json"), &ws); err != nil {
			continue
		}
		folder := localPath(ws.Folder)
		if folder == "" || !transcript.PathsIn(folder, repoRoot) {
			continue
		}
		out[e.Name()] = folder
	}
	return out
}

// localPath turns a workspace URI into a path, and only when it names one.
// Cursor stores remote windows as vscode-remote:// URIs whose path is a
// directory on another machine — "/home/ubuntu/api" must never be matched
// against a local repository that happens to sit at the same path.
func localPath(uri string) string {
	if uri == "" {
		return ""
	}
	if !strings.HasPrefix(uri, "file://") {
		return ""
	}
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	if u.Host != "" && u.Host != "localhost" {
		return ""
	}
	return u.Path
}

// head is one conversation as discovery knows it, before anything is loaded.
type head struct {
	id        string
	workspace string
	title     string
	updated   time.Time
}

// composersFor lists the conversations of the given workspaces.
//
// The composerHeaders index is the fast path. Older Cursor builds do not have
// it; there the per-workspace state store names the conversations that were
// open, which is the set a commit could have come from anyway.
func composersFor(db string, spaces map[string]string) ([]head, error) {
	ids := keys(spaces)
	rows, err := sqlitex.QueryReadOnly(db, fmt.Sprintf(
		`SELECT composerId, workspaceId,
		        MAX(COALESCE(lastUpdatedAt, 0), COALESCE(createdAt, 0)), isSubagent,
		        json_extract(CAST(value AS TEXT), '$.name')
		   FROM composerHeaders WHERE workspaceId IN (%s);`, quote(ids)))
	if err == nil {
		var out []head
		for _, r := range rows {
			if len(r) < 5 || r[3] == "1" { // subagent chatter: noise for distillation
				continue
			}
			out = append(out, head{id: r[0], workspace: r[1], title: r[4], updated: msTime(atoi(r[2]))})
		}
		return out, nil
	}
	legacy, legacyErr := legacyComposers(db, spaces)
	if legacyErr != nil {
		return nil, fmt.Errorf("cursoride: %w (and no legacy index: %v)", err, legacyErr)
	}
	return legacy, nil
}

// legacyComposers reads the conversation ids out of each workspace's own state
// store, then asks the global store when they were last touched.
func legacyComposers(db string, spaces map[string]string) ([]head, error) {
	byID := map[string]string{}
	for hash := range spaces {
		wsDB := filepath.Join(filepath.Dir(filepath.Dir(db)), "workspaceStorage", hash, "state.vscdb")
		rows, err := sqlitex.Query(wsDB, `SELECT CAST(value AS TEXT) FROM ItemTable WHERE key = 'composer.composerData';`)
		if err != nil || len(rows) == 0 || len(rows[0]) == 0 {
			continue
		}
		var data struct {
			Selected    []string `json:"selectedComposerIds"`
			LastFocused []string `json:"lastFocusedComposerIds"`
			All         []struct {
				ComposerID string `json:"composerId"`
			} `json:"allComposers"`
		}
		if json.Unmarshal([]byte(rows[0][0]), &data) != nil {
			continue
		}
		for _, id := range data.Selected {
			byID[id] = hash
		}
		for _, id := range data.LastFocused {
			byID[id] = hash
		}
		for _, c := range data.All {
			byID[c.ComposerID] = hash
		}
	}
	if len(byID) == 0 {
		return nil, errors.New("no conversations recorded for this workspace")
	}
	var composerKeys []string
	for id := range byID {
		if isID(id) {
			composerKeys = append(composerKeys, "composerData:"+id)
		}
	}
	rows, err := sqlitex.QueryReadOnly(db, fmt.Sprintf(
		`SELECT key, COALESCE(json_extract(CAST(value AS TEXT), '$.lastUpdatedAt'),
		               json_extract(CAST(value AS TEXT), '$.createdAt'), 0),
		        json_extract(CAST(value AS TEXT), '$.name')
		   FROM cursorDiskKV WHERE key IN (%s);`, quote(composerKeys)))
	if err != nil {
		return nil, err
	}
	var out []head
	for _, r := range rows {
		if len(r) < 3 {
			continue
		}
		id := strings.TrimPrefix(r[0], "composerData:")
		out = append(out, head{id: id, workspace: byID[id], title: r[2], updated: msTime(atoi(r[1]))})
	}
	return out, nil
}

// composerData is the conversation record: an ordered list of bubble ids plus
// the settings the conversation ran under.
type composerData struct {
	Name        string          `json:"name"`
	LastUpdated int64           `json:"lastUpdatedAt"`
	Headers     []bubbleHeader  `json:"fullConversationHeadersOnly"`
	ModelConfig json.RawMessage `json:"modelConfig"`
}

type bubbleHeader struct {
	BubbleID string `json:"bubbleId"`
	Type     int    `json:"type"`
}

// bubble is one message. Cursor writes far more per bubble than this; the rest
// is rendering state, and reading only these fields is what keeps added fields
// from breaking the parser.
type bubble struct {
	Type     int    `json:"type"` // 1 human, 2 agent
	Text     string `json:"text"`
	Thinking struct {
		Text string `json:"text"`
	} `json:"thinking"`
	Tool      *toolFormer `json:"toolFormerData"`
	CreatedAt string      `json:"createdAt"`
	ModelInfo struct {
		ModelName string `json:"modelName"`
	} `json:"modelInfo"`
}

// toolFormer is one tool call. params is what Cursor resolved and rawArgs what
// the model asked for; both are JSON encoded as a string inside the JSON.
type toolFormer struct {
	Name    string          `json:"name"`
	Status  string          `json:"status"`
	Params  string          `json:"params"`
	RawArgs string          `json:"rawArgs"`
	Error   string          `json:"error"`
	Result  json.RawMessage `json:"result"`
}

// Load reads the bubbles after from.Count. A conversation only grows, so the
// message count is an exact cursor; a count past the end means the user rewound
// or branched the conversation, and we re-read rather than guess.
func (p *Parser) Load(ref transcript.Ref, from transcript.Cursor) (*transcript.Session, error) {
	db := ref.Path
	if db == "" {
		db = p.GlobalDB()
	}
	if !isID(ref.ID) {
		return nil, fmt.Errorf("cursoride: refusing to query for id %q", ref.ID)
	}
	raw, err := composerBlob(db, ref.ID)
	if err != nil {
		return nil, err
	}
	var cd composerData
	if err := json.Unmarshal(raw, &cd); err != nil {
		return nil, fmt.Errorf("cursoride: composerData %s: %w", short(ref.ID), err)
	}

	s := &transcript.Session{Ref: ref, Model: modelName(cd.ModelConfig)}
	start := from.Count
	if start > len(cd.Headers) {
		start = 0
	}
	s.Complete = start == 0
	s.Cursor = transcript.Cursor{Count: len(cd.Headers)}
	if n := len(cd.Headers); n > 0 {
		s.Cursor.Token = cd.Headers[n-1].BubbleID
	}
	if start == len(cd.Headers) {
		return s, nil // nothing new
	}

	want := cd.Headers[start:]
	blobs, err := fetchBubbles(db, ref.ID, want)
	if err != nil {
		return nil, err
	}
	for _, h := range want {
		raw, ok := blobs[h.BubbleID]
		if !ok {
			continue // pruned or encrypted bubble: skip, never fail the commit
		}
		m, model := convert(raw)
		if m != nil {
			s.Messages = append(s.Messages, *m)
		}
		if model != "" {
			s.Model = model
		}
	}
	return s, nil
}

// Pointer hashes the conversation rather than the store that holds it.
//
// The generic pointer is the sha256 of the transcript file, which here would be
// the whole multi-gigabyte database — unreadable in a commit hook and useless
// as an identifier, since it changes whenever any other conversation does.
func (p *Parser) Pointer(ref transcript.Ref) string {
	db := ref.Path
	if db == "" {
		db = p.GlobalDB()
	}
	if !isID(ref.ID) {
		return ""
	}
	raw, err := composerBlob(db, ref.ID)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func composerBlob(db, composerID string) ([]byte, error) {
	rows, err := sqlitex.QueryReadOnly(db, fmt.Sprintf(
		`SELECT hex(value) FROM cursorDiskKV WHERE key = 'composerData:%s';`, composerID))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return nil, fmt.Errorf("cursoride: conversation %s not in the store", short(composerID))
	}
	return hex.DecodeString(rows[0][0])
}

// fetchBubbles loads message blobs by key, in batches, so one conversation
// costs a couple of queries regardless of its length.
func fetchBubbles(db, composerID string, headers []bubbleHeader) (map[string][]byte, error) {
	out := map[string][]byte{}
	const batch = 200
	for start := 0; start < len(headers); start += batch {
		end := min(start+batch, len(headers))
		var want []string
		for _, h := range headers[start:end] {
			if isID(h.BubbleID) {
				want = append(want, "bubbleId:"+composerID+":"+h.BubbleID)
			}
		}
		if len(want) == 0 {
			continue
		}
		rows, err := sqlitex.QueryReadOnly(db, fmt.Sprintf(
			`SELECT key, hex(value) FROM cursorDiskKV WHERE key IN (%s);`, quote(want)))
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
			out[r[0][strings.LastIndex(r[0], ":")+1:]] = data
		}
	}
	return out, nil
}

// convert normalises one bubble, also reporting the model that produced it.
func convert(raw []byte) (*transcript.Message, string) {
	var b bubble
	if json.Unmarshal(raw, &b) != nil {
		return nil, "" // encrypted or unknown blob
	}
	msg := transcript.Message{}
	switch b.Type {
	case 1:
		msg.Role = transcript.RoleUser
	case 2:
		msg.Role = transcript.RoleAssistant
	default:
		msg.Role = transcript.RoleSystem
		msg.Meta = true
	}
	if t, err := time.Parse(time.RFC3339, b.CreatedAt); err == nil {
		msg.Time = t
	}
	msg.Text = strings.TrimSpace(b.Text)
	msg.Thinking = strings.TrimSpace(b.Thinking.Text)
	if b.Tool != nil {
		msg.Tools = append(msg.Tools, toolCall(*b.Tool))
	}
	if msg.Text == "" && msg.Thinking == "" && len(msg.Tools) == 0 {
		return nil, b.ModelInfo.ModelName
	}
	return &msg, b.ModelInfo.ModelName
}

func toolCall(tf toolFormer) transcript.ToolCall {
	tc := transcript.ToolCall{Name: tf.Name}
	// params holds the resolved call, rawArgs what the model wrote; either can be
	// missing, and the resolved one carries absolute paths.
	in := decodeArgs(tf.Params)
	if raw := decodeArgs(tf.RawArgs); len(raw) > 0 {
		for k, v := range raw {
			if _, ok := in[k]; !ok {
				in[k] = v
			}
		}
	}
	tc.Files = filesFromInput(in)
	tc.Summary = summarize(in)
	switch tf.Status {
	case "", "completed", "success":
	default:
		// A failed or interrupted call is what makes an agent change course, which
		// is exactly the reasoning a record should carry.
		detail := tf.Error
		if detail == "" {
			detail = resultText(tf.Result)
		}
		tc.Error = strings.TrimSpace(tf.Status + ": " + transcript.Truncate(collapse(detail), 300))
	}
	return tc
}

// decodeArgs unwraps the JSON-inside-JSON Cursor stores tool arguments as.
func decodeArgs(s string) map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(s) == "" {
		return out
	}
	if json.Unmarshal([]byte(s), &out) == nil {
		return out
	}
	var inner string
	if json.Unmarshal([]byte(s), &inner) == nil {
		_ = json.Unmarshal([]byte(inner), &out)
	}
	return out
}

func filesFromInput(in map[string]any) []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range []string{
		"targetFile", "target_file", "path", "file_path", "filePath",
		"relativeWorkspacePath", "effectiveUri", "uri", "notebook_path",
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
	for _, k := range []string{"command", "pattern", "query", "searchTerm", "globPattern", "instructions", "explanation"} {
		if v, ok := in[k].(string); ok && v != "" {
			return transcript.Truncate(collapse(v), 200)
		}
	}
	return ""
}

func resultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func modelName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var mc struct {
		ModelName string `json:"modelName"`
	}
	if json.Unmarshal(raw, &mc) != nil {
		return ""
	}
	return mc.ModelName
}

// isID guards every value interpolated into SQL. Ids come from the store
// itself, so they are UUIDs; we check anyway rather than trust them.
func isID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// quote renders a SQL string list, dropping anything unsafe rather than
// escaping it: every value here is an id or a key built from one.
func quote(vals []string) string {
	var safe []string
	for _, v := range vals {
		if strings.ContainsAny(v, "'\";") {
			continue
		}
		safe = append(safe, "'"+v+"'")
	}
	if len(safe) == 0 {
		return "''"
	}
	return strings.Join(safe, ",")
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
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

func atoi(s string) int64 {
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
