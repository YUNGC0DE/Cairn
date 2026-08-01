// Package capture is the CAPTURE stage of cairn (spec §3.1): find the agent
// sessions that produced the work being committed, read only what is new, and
// remember how far we got.
//
// The whole design rests on one observation: at commit time the transcript is
// already complete on disk. No agent-side hooks, no lifecycle events, no daemon
// — one git hook and a file read.
package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/YUNGC0DE/Cairn/internal/transcript"
	"github.com/YUNGC0DE/Cairn/internal/transcript/claudecode"
	"github.com/YUNGC0DE/Cairn/internal/transcript/cursorcli"
)

// Parsers returns every registered transcript parser.
func Parsers() []transcript.Parser {
	return []transcript.Parser{claudecode.New(), cursorcli.New()}
}

// ParserByName returns a single parser, or nil.
func ParserByName(name string) transcript.Parser {
	for _, p := range Parsers() {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// Discover finds sessions from all agents whose cwd is inside repoRoot and that
// changed at or after since. Per-parser failures are collected, not fatal: a
// broken Cursor store must never stop a Claude Code record from being written.
func Discover(repoRoot string, since time.Time) ([]transcript.Ref, []error) {
	var refs []transcript.Ref
	var errs []error
	for _, p := range Parsers() {
		found, err := p.Discover(repoRoot, since)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name(), err))
			continue
		}
		refs = append(refs, found...)
	}
	transcript.SortRefs(refs)
	return refs, errs
}

// Offsets records how much of each transcript has been consumed. It lives in
// .git/cairn/offsets.json — local, never committed, safe to delete.
type Offsets struct {
	Version int                            `json:"version"`
	Cursors map[string]transcript.Cursor   `json:"cursors"`
	Seen    map[string]offsetsSessionStamp `json:"seen,omitempty"`

	path string
}

type offsetsSessionStamp struct {
	Commit string    `json:"commit"`
	At     time.Time `json:"at"`
}

const offsetsVersion = 1

// LoadOffsets reads offsets from a cairn state directory, returning an empty set
// when absent.
func LoadOffsets(cairnDir string) (*Offsets, error) {
	o := &Offsets{
		Version: offsetsVersion,
		Cursors: map[string]transcript.Cursor{},
		Seen:    map[string]offsetsSessionStamp{},
		path:    filepath.Join(cairnDir, "offsets.json"),
	}
	b, err := os.ReadFile(o.path)
	if err != nil {
		if os.IsNotExist(err) {
			return o, nil
		}
		return o, err
	}
	if err := json.Unmarshal(b, o); err != nil {
		// A corrupt offsets file must not block commits; start over. Worst case we
		// re-read a transcript we already distilled.
		o.Cursors = map[string]transcript.Cursor{}
		o.Seen = map[string]offsetsSessionStamp{}
		return o, nil
	}
	if o.Cursors == nil {
		o.Cursors = map[string]transcript.Cursor{}
	}
	if o.Seen == nil {
		o.Seen = map[string]offsetsSessionStamp{}
	}
	return o, nil
}

// Get returns the stored cursor for a session key.
func (o *Offsets) Get(key string) transcript.Cursor { return o.Cursors[key] }

// Set records a new cursor and the commit that consumed it.
func (o *Offsets) Set(key string, c transcript.Cursor, commit string) {
	o.Cursors[key] = c
	o.Seen[key] = offsetsSessionStamp{Commit: commit, At: time.Now().UTC()}
}

// Save writes offsets atomically.
func (o *Offsets) Save() error {
	if o.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(o.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	tmp := o.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, o.path)
}

// LoadNew reads everything not yet consumed from the given sessions. Sessions
// with no new messages are dropped. Errors are per-session and non-fatal.
func LoadNew(refs []transcript.Ref, off *Offsets) ([]*transcript.Session, []error) {
	var out []*transcript.Session
	var errs []error
	for _, ref := range refs {
		p := ParserByName(ref.Agent)
		if p == nil {
			continue
		}
		s, err := p.Load(ref, off.Get(ref.Key))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", ref.Agent, shortID(ref.ID), err))
			continue
		}
		if s == nil || !hasSignal(s) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.Before(out[j].Updated) })
	return out, errs
}

// hasSignal drops slices that contain nothing a distiller could use — for
// example a session that only received tool results since the last commit.
func hasSignal(s *transcript.Session) bool {
	for _, m := range s.Messages {
		if m.Role == transcript.RoleUser && !m.Meta && m.Text != "" {
			return true
		}
		if m.Role == transcript.RoleAssistant && (m.Text != "" || m.Thinking != "" || len(m.Tools) > 0) {
			return true
		}
	}
	return false
}

// TranscriptPointer is the sha256 of a transcript's current bytes. Cairn stores
// this pointer and never the transcript itself (spec §4.3): the content stays on
// the user's disk, secrets never enter git history.
func TranscriptPointer(ref transcript.Ref) string {
	h := sha256.New()
	info, err := os.Stat(ref.Path)
	if err != nil {
		return ""
	}
	target := ref.Path
	if info.IsDir() {
		// Cursor: hash the store, which is content-addressed anyway.
		target = filepath.Join(ref.Path, "store.db")
	}
	f, err := os.Open(target)
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
