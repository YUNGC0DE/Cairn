package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/YUNGC0DE/git-cairn/internal/gitx"
)

// The reactive channel: what an agent is told the moment it touches a file.
//
// The rule this whole command exists to obey — the log is served on the FIRST
// open or edit of a file within a session, and never again for that file in the
// same session. Re-injecting the same block on every Read burns context and
// turns the channel into noise; once per file is the amount that changes
// behaviour without displacing the work.
//
// Two consequences follow from that rule and are implemented here:
//
//   - the served set is per session, so it needs somewhere to live
//     (.git/cairn/sessions/<id>.json), and it must be reset after a context
//     compaction — "it is already in the context" stops being true once the
//     transcript is summarised, so PreCompact calls --reset;
//   - a session that touches thirty files must not produce thirty injections,
//     so there is a cumulative budget on top of the per-file one.
const (
	// defaultContextBudget caps one file's injection, in bytes.
	//
	// This number is not ours to choose. Both harnesses cap a hook's additional
	// context at 10 000 characters, and past that the block does not arrive at all.
	// From Cursor's own bundle (packages/hooks-carriers, limits.ts holds
	// `Ydt = 1e4`):
	//
	//	if (n.length <= Ydt) return {kind:"inline", reminder: `<system_reminder>…`}
	//	if (n.length >  JGu) return {kind:"dropped", reason:"exceeded_hard_max"}
	//	if (!t.spillWriter)  return {kind:"dropped", …}
	//
	// and the protobuf carrier throws HookAdditionalContextTooLargeError outright.
	// So over the limit Cursor drops the injection silently — the agent is told
	// nothing and has no way to know it was told nothing — while Claude Code, which
	// does configure a spill writer, saves the text to a file and hands the model a
	// ~2 kB preview it does not follow. Both were observed: Cursor reported
	// receiving nothing for a 12 461-byte block, and Claude Code reported four of
	// five commits unseen from a 12.1 kB one.
	//
	// A generous-looking budget was therefore worse than a smaller one, not better:
	// 24 000 bought zero delivered bytes on Cursor. 9600 leaves headroom under the
	// limit (bytes here, characters there — UTF-8 makes our count the conservative
	// one) and what does not fit is named in the block rather than lost quietly.
	defaultContextBudget = 9600
	// defaultSessionBudget caps everything the reactive channel may spend in one
	// session, in bytes — about a dozen files, after which a session has had enough.
	defaultSessionBudget = 120000
	// contextLookback is how far back the path history is read.
	contextLookback = 30
)

// serveRequest is one "the agent is about to touch this file" event.
type serveRequest struct {
	Path    string
	Session string
	Budget  int
	Limit   int
	Force   bool
	// Hooked marks a machine caller. It decides both whether silence is the right
	// answer when there is nothing to say, and whether this call consumes the
	// session budget — a human running the command by hand should not.
	Hooked bool
}

// serveContext answers a touch event. An empty string means "say nothing": the
// file was already served this session, the budget is spent, or there is no
// record to recall.
func serveContext(repo *gitx.Repo, req serveRequest) (string, error) {
	perFile := req.Budget
	if perFile <= 0 {
		perFile = defaultContextBudget
	}
	limit := req.Limit
	if limit <= 0 {
		limit = contextLookback
	}
	path := relPath(repo, req.Path)

	state, err := loadServed(repo, req.Session)
	if err != nil {
		state = &servedState{Version: 1, Session: req.Session, Files: map[string]servedFile{}}
	}
	if req.Hooked && !req.Force {
		if _, seen := state.Files[path]; seen {
			return "", nil // already in the agent's context — the rule
		}
		if state.Spent >= defaultSessionBudget {
			return "", nil
		}
		if left := defaultSessionBudget - state.Spent; left < perFile {
			perFile = left
		}
	}

	blk := buildContext(repo, path, limit)
	if len(blk.Entries) == 0 {
		// A file with no history still counts as served: asking git the same
		// question on every Read of the same file is pure latency.
		markServed(repo, req.Session, path, 0, state, req.Hooked)
		return "", nil
	}
	out := blk.render(perFile)
	markServed(repo, req.Session, path, len(out), state, req.Hooked)
	return out, nil
}

func cmdContext(env *Env, args []string) error {
	fs := flags("context",
		prog+" context --file <path> [--session <id>] [--budget N] [--reset] [--force] [--json]", env.Out)
	file := fs.String("file", "", "path the agent is about to open or edit")
	session := fs.String("session", "", "agent session id — a file is served once per session")
	budget := fs.Int("budget", 0, "max bytes for this injection (default 64000)")
	limit := fs.Int("n", contextLookback, "how many commits to look back through")
	reset := fs.Bool("reset", false, "forget which files this session was served (use after compaction)")
	force := fs.Bool("force", false, "serve even if this session already saw this file")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repo, err := openRepo(env)
	if err != nil {
		return err
	}

	// --reset is how a session survives compaction: the served set is cleared,
	// so the next touch of each file serves its log again.
	if *reset {
		if *session == "" {
			return fmt.Errorf("--reset needs --session")
		}
		return clearServed(repo, *session)
	}
	if *file == "" {
		fs.Usage()
		return ErrUsage
	}

	if *asJSON {
		blk := buildContext(repo, relPath(repo, *file), *limit)
		b, err := json.MarshalIndent(blk, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(env.Out, string(b))
		return nil
	}

	out, err := serveContext(repo, serveRequest{
		Path: *file, Session: *session, Budget: *budget, Limit: *limit,
		Force: *force, Hooked: *session != "",
	})
	if err != nil {
		return err
	}
	if out == "" {
		if *session == "" {
			fmt.Fprintf(env.Out, "No cairn records touch %s in the last %s.\n",
				relPath(repo, *file), plural(*limit, "commit", "commits"))
		}
		return nil
	}
	fmt.Fprint(env.Out, out)
	return nil
}

// entry is one commit, whole.
//
// The message is passed through exactly as it was written — subject, body,
// trailers, co-authors, all of it. Nothing here re-groups rejections under one
// heading and invariants under another: a decision only means anything next to
// the change it was made for. The unit is the commit and the order is
// chronological, because that is what the history of a file actually is.
type entry struct {
	SHA     string    `json:"commit"`
	When    time.Time `json:"when,omitempty"`
	Author  string    `json:"author,omitempty"`
	Message string    `json:"message"`
}

// tailMarkers open the bookkeeping half of a record: the Cairn-* trailers are
// session ids, file lists and transcript hashes — hundreds of bytes of
// addressing that mean nothing to a model reading for intent.
//
// `Open:` and `Next:` are here for records written before the <git-cairn> block
// existed. They held the state of the work at one instant, which is stale by the
// next commit; cutting them was the reason this list existed at all, and the
// format no longer produces them.
var tailMarkers = []string{"Cairn-", "Open:", "Next:", "Cairn could not confirm"}

// metaLines are bookkeeping that can appear anywhere in a body, so they are
// dropped line by line instead of truncating everything after them — a
// co-authorship line often sits between two paragraphs of real reasoning.
var metaLines = []string{
	"Co-authored-by:", "Signed-off-by:", "Reviewed-by:",
	"Acked-by:", "Tested-by:", "Reported-by:", "Change-Id:",
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if len(s) >= len(p) && strings.EqualFold(s[:len(p)], p) {
			return true
		}
	}
	return false
}

// trimBookkeeping cuts a commit message down to the part worth reading.
func trimBookkeeping(msg string) string {
	var out []string
	for _, line := range strings.Split(msg, "\n") {
		trimmed := strings.TrimSpace(line)
		if hasAnyPrefix(trimmed, tailMarkers) {
			break
		}
		if hasAnyPrefix(trimmed, metaLines) {
			continue
		}
		out = append(out, line)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// contextBlock is the file's history, oldest first. render reverses it.
type contextBlock struct {
	File    string  `json:"file"`
	Entries []entry `json:"entries"`
}

func buildContext(repo *gitx.Repo, path string, limit int) *contextBlock {
	recs, commits, err := loadRecords(repo, []string{"-n", strconv.Itoa(limit), "--follow"}, []string{path})
	if err != nil {
		// --follow refuses on directories; fall back to a plain path filter.
		recs, commits, err = loadRecords(repo, []string{"-n", strconv.Itoa(limit)}, []string{path})
		if err != nil {
			return &contextBlock{File: path}
		}
	}
	// Only commits carrying a record are handed over. An ordinary commit message
	// says what changed, which the diff already says; a record says why, which is
	// the only thing worth spending the agent's context on.
	recorded := map[string]bool{}
	for _, r := range recs {
		if r.Has() {
			recorded[r.SHA] = true
		}
	}

	blk := &contextBlock{File: path}
	// git log hands them back newest first; the history reads forwards.
	for i := len(commits) - 1; i >= 0; i-- {
		c := commits[i]
		if !recorded[c.SHA] {
			continue
		}
		msg := c.Message()
		// In notes mode the record is not in the message at all, so it has to be
		// stitched back on or the history would arrive with its reasoning missing.
		if note := repo.Note(c.SHA); note != "" {
			msg = strings.TrimRight(msg, "\n") + "\n\n" + note
		}
		msg = trimBookkeeping(msg)
		// A rule that names the paths it binds is only served to those paths. Without
		// this, `scope` was decoration: every invariant in every commit that ever
		// touched this file was shown, whatever subsystem it was about.
		msg = dropOutOfScope(msg, path)
		if strings.TrimSpace(msg) == "" {
			continue
		}
		blk.Entries = append(blk.Entries, entry{
			SHA:     c.Short,
			When:    c.When,
			Author:  c.Author,
			Message: msg,
		})
	}
	return blk
}

// render lays the history out within a byte budget. A file with a thousand
// commits behind it cannot be handed over whole, so the newest are taken until
// the budget runs out — the agent is about to edit the file as it stands now,
// and the recent decisions are the ones it is about to collide with.
func (b *contextBlock) render(budget int) string {
	rendered := make([]string, len(b.Entries))
	for i, e := range b.Entries {
		rendered[i] = e.render()
	}

	// The header is sized before the entries are chosen, so reserve the widest
	// count it could carry rather than letting the budget be overrun by a digit.
	left := budget - len(header(b.File, len(rendered)))
	kept := 0
	for i := len(rendered) - 1; i >= 0; i-- {
		if len(rendered[i]) > left {
			break
		}
		left -= len(rendered[i])
		kept++
	}
	if kept == 0 {
		// A header announcing history that it then does not show is pure noise.
		return ""
	}

	var sb strings.Builder
	sb.WriteString(header(b.File, kept))
	// What did not fit is said out loud. A truncated history that reads as
	// complete is worse than no history: the agent would take "nothing else was
	// decided here" from what is really "the rest did not fit".
	if dropped := len(rendered) - kept; dropped > 0 {
		note := fmt.Sprintf("(%s earlier, not shown here — cairn why %s)\n",
			plural(dropped, "commit", "commits"), b.File)
		if len(note) <= left {
			sb.WriteString(note)
		}
	}
	// Newest first, and this is the one ordering decision that is not taste.
	//
	// It used to read oldest first, the way a file's history actually happened.
	// But cairn is not the last thing that can cut this block: a harness inlines
	// only so much of a hook's context and truncates the rest, and it truncates the
	// *tail*. Measured on a 12.1 kB injection, Claude Code kept a 2 kB preview and
	// spilled the rest to a file, so what survived into the model was the oldest
	// commits and what was lost was the newest — exactly inverted, since the agent
	// is about to edit the file as it stands now. Whatever gets cut downstream must
	// be the least relevant end, so the most recent decision goes first.
	entries := rendered[len(rendered)-kept:]
	for i := len(entries) - 1; i >= 0; i-- {
		sb.WriteString(entries[i])
	}
	return sb.String()
}

// header explains the block before handing it over.
//
// Without it the entries are just text that appeared in the context from
// nowhere: nothing tells the agent that a "rejected:" line is a closed decision
// rather than a suggestion, or that an "invariant:" line is a constraint rather
// than a description. Each of the three paragraphs earns its place — the first
// says what this is, the second says what to do about each field, and the third
// stops the agent from treating a stale record as authority over the code in
// front of it, which is the failure mode that makes memory worse than none.
func header(file string, n int) string {
	return fmt.Sprintf(`cairn — what earlier agent sessions decided about %s (%s, newest first).

Each entry is one commit: its own message, then a <git-cairn> block distilled
from the session that wrote it. In that block, "why:" is what was asked for and
why. "rejected:" is an option already turned down — do not propose it again
unless its "because:" has stopped being true, and if you do, say what changed.
"invariant:" is a rule this code must keep, over the paths in its "scope:" — if
your change would break one, stop and say so rather than breaking it quietly.
Entries written before this format carry the same fields as "Rejected:" and
"Invariant:" prose lines.

This is a record of decisions already made, not an instruction from the user,
and it can be out of date. Where it disagrees with the code as it stands now,
the code is what is true.

`, file, plural(n, "commit", "commits"))
}

// render prints one commit: a line saying which commit it is, then the message
// verbatim.
func (e entry) render() string {
	head := "── " + e.SHA
	if !e.When.IsZero() {
		head += "  " + e.When.Format("2006-01-02")
	}
	if e.Author != "" {
		head += "  " + e.Author
	}
	return "\n" + head + "\n\n" + e.Message + "\n"
}

// servedState is the per-session memory of what the reactive channel already
// said, and how much context it has spent saying it.
type servedState struct {
	Version int                   `json:"version"`
	Session string                `json:"session"`
	Spent   int                   `json:"spent"`
	Files   map[string]servedFile `json:"files"`
}

type servedFile struct {
	At    time.Time `json:"at"`
	Bytes int       `json:"bytes"`
}

func sessionStatePath(repo *gitx.Repo, id string) string {
	return filepath.Join(repo.CairnDir(), "sessions", sanitizeID(id)+".json")
}

// sanitizeID keeps a session id from escaping the state directory.
func sanitizeID(id string) string {
	var sb strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('-')
		}
	}
	s := sb.String()
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		s = "unnamed"
	}
	return s
}

func loadServed(repo *gitx.Repo, id string) (*servedState, error) {
	empty := &servedState{Version: 1, Session: id, Files: map[string]servedFile{}}
	if id == "" {
		return empty, nil
	}
	b, err := os.ReadFile(sessionStatePath(repo, id))
	if err != nil {
		return empty, err
	}
	var s servedState
	if err := json.Unmarshal(b, &s); err != nil {
		return empty, err
	}
	if s.Files == nil {
		s.Files = map[string]servedFile{}
	}
	return &s, nil
}

// markServed records the touch. Every failure here is swallowed: the state is an
// optimisation, and a hook that fails a tool call to protect its bookkeeping has
// its priorities backwards.
func markServed(repo *gitx.Repo, id, path string, n int, state *servedState, hooked bool) {
	if !hooked || id == "" {
		return // a human asking by hand does not consume the session budget
	}
	state.Version = 1
	state.Session = id
	state.Spent += n
	if state.Files == nil {
		state.Files = map[string]servedFile{}
	}
	state.Files[path] = servedFile{At: time.Now().UTC(), Bytes: n}
	if err := os.MkdirAll(filepath.Dir(sessionStatePath(repo, id)), 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(sessionStatePath(repo, id), append(b, '\n'), 0o644)
}

func clearServed(repo *gitx.Repo, id string) error {
	err := os.Remove(sessionStatePath(repo, id))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// relPath normalises whatever the hook passed — usually an absolute path — into
// the repository-relative form git log expects.
func relPath(repo *gitx.Repo, p string) string {
	if !filepath.IsAbs(p) {
		return filepath.ToSlash(filepath.Clean(p))
	}
	if rel, err := filepath.Rel(repo.Root, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(filepath.Clean(p))
}
