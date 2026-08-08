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
	"github.com/YUNGC0DE/git-cairn/internal/record"
)

// The reactive channel: what an agent is told the moment it touches a file.
//
// The rule this whole command exists to obey — the rules recorded for a file are
// served on the FIRST open or edit of it within a session, and never again for
// that file in the same session. Re-injecting the same block on every Read burns
// context and turns the channel into noise; once per file is the amount that
// changes behaviour without displacing the work.
//
// That rule needs somewhere to remember what it already said
// (.git/cairn/sessions/<id>.json), and it has to be reset after a context
// compaction — "it is already in the context" stops being true once the
// transcript is summarised, so PreCompact calls --reset.
const (
	// maxInjection is the hard ceiling on one injection, in bytes, and it is not
	// ours to choose. Both harnesses cap a hook's additional context at 10 000
	// characters; past that the block does not arrive at all. From Cursor's own
	// bundle (packages/hooks-carriers, limits.ts holds `Ydt = 1e4`):
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
	// There is no configurable budget any more, and no separate per-session
	// allowance. A number the user can raise is a number that silently deletes
	// their injection, and a second budget on top of this one only ever subtracts
	// from what fits. Bytes here against characters there makes this the
	// conservative count of the two.
	maxInjection = 10000
	// contextLookback is how far back the file's history is read. The record is
	// two short lines per commit, so a hundred commits of a file is a sane thing to
	// ask git for and the injection ceiling decides how many of them are served.
	contextLookback = 100
)

// serveRequest is one "the agent is about to touch this file" event.
type serveRequest struct {
	Path    string
	Session string
	Limit   int
	Force   bool
	// Hooked marks a machine caller. It decides whether silence is the right
	// answer when there is nothing to say, and whether this call is remembered as
	// having been served — a human running the command by hand should not be.
	Hooked bool
}

// serveContext answers a touch event. An empty string means "say nothing": the
// file was already served this session, or nothing recorded a rule about it.
func serveContext(repo *gitx.Repo, req serveRequest) (string, error) {
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
	}

	blk := buildContext(repo, path, limit)
	if len(blk.Commits) == 0 {
		// A file with no rules still counts as served: asking git the same
		// question on every Read of the same file is pure latency.
		markServed(repo, req.Session, path, state, req.Hooked)
		return "", nil
	}
	out := blk.render()
	markServed(repo, req.Session, path, state, req.Hooked)
	return out, nil
}

func cmdContext(env *Env, args []string) error {
	fs := flags("context",
		prog+" context --file <path> [--session <id>] [--reset] [--force] [--json]", env.Out)
	file := fs.String("file", "", "path the agent is about to open or edit")
	session := fs.String("session", "", "agent session id — a file is served once per session")
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
	// so the next touch of each file serves its rules again.
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
		Path: *file, Session: *session, Limit: *limit,
		Force: *force, Hooked: *session != "",
	})
	if err != nil {
		return err
	}
	if out == "" {
		if *session == "" {
			fmt.Fprintf(env.Out, "No cairn rules bind %s in the last %s.\n",
				relPath(repo, *file), plural(*limit, "commit", "commits"))
		}
		return nil
	}
	fmt.Fprint(env.Out, out)
	return nil
}

// commitRules is what one commit recorded about the file being opened.
//
// Only the rules travel. The commit's own message, its trailers and the "why"
// behind each rule stay in git, one `git show` away: the injection is capped at
// 10 000 characters by the harness, and spending it on prose the agent did not
// ask for is how a file's fiftieth commit stops arriving at all.
type commitRules struct {
	SHA        string         `json:"commit"`
	When       time.Time      `json:"when,omitempty"`
	Rejected   []record.Entry `json:"rejected,omitempty"`
	Invariants []record.Entry `json:"invariants,omitempty"`
}

// contextBlock is every rule bound to one file, newest commit first.
type contextBlock struct {
	File    string        `json:"file"`
	Commits []commitRules `json:"commits"`
}

func buildContext(repo *gitx.Repo, path string, limit int) *contextBlock {
	recs, _, err := loadRecords(repo, []string{"-n", strconv.Itoa(limit), "--follow"}, []string{path})
	if err != nil {
		// --follow refuses on directories; fall back to a plain path filter.
		recs, _, err = loadRecords(repo, []string{"-n", strconv.Itoa(limit)}, []string{path})
		if err != nil {
			return &contextBlock{File: path}
		}
	}
	blk := &contextBlock{File: path}
	// git log hands them back newest first, which is the order they are served in.
	for _, r := range recs {
		rejected, invariants := r.Rules(path)
		if len(rejected)+len(invariants) == 0 {
			continue
		}
		blk.Commits = append(blk.Commits, commitRules{
			SHA: r.Short, When: r.When, Rejected: rejected, Invariants: invariants,
		})
	}
	return blk
}

// render lays the rules out under the injection ceiling.
//
// Commits go in whole, newest first, until the next one would not fit — a
// half-rendered commit would show a rejection with no sign that an invariant
// from the same decision was cut. What did not fit is named, because a truncated
// history that reads as complete is worse than no history: the agent would take
// "nothing else was decided here" from what is really "the rest did not fit".
//
// Newest first is also what survives being cut a second time. A harness inlines
// only so much and truncates the tail, so whatever is lost downstream has to be
// the oldest end — the agent is about to edit the file as it stands now.
func (b *contextBlock) render() string {
	rendered := make([]string, len(b.Commits))
	for i, c := range b.Commits {
		rendered[i] = c.render()
	}
	// The header is sized before the commits are chosen, and the overflow note is
	// reserved for, so neither can push the block over the ceiling after the fact.
	left := maxInjection - len(header(b.File, len(rendered))) - len(overflow(len(rendered), b.File))
	kept := 0
	for _, s := range rendered {
		if len(s) > left {
			break
		}
		left -= len(s)
		kept++
	}
	if kept == 0 {
		// A header announcing rules that it then does not show is pure noise.
		return ""
	}

	var sb strings.Builder
	sb.WriteString(header(b.File, kept))
	for _, s := range rendered[:kept] {
		sb.WriteString(s)
	}
	if dropped := len(rendered) - kept; dropped > 0 {
		sb.WriteString(overflow(dropped, b.File))
	}
	return sb.String()
}

func overflow(dropped int, file string) string {
	if dropped <= 0 {
		return ""
	}
	return fmt.Sprintf("\n(%s with rules for this file did not fit — git log --follow -- %s)\n",
		plural(dropped, "older commit", "older commits"), file)
}

// header explains the block before handing it over.
//
// Without it the lines are text that appeared in the context from nowhere:
// nothing tells the agent that a "reject:" line is a closed decision rather than
// a suggestion, or that an "invariant:" line is a constraint rather than a
// description. It also has to say where the reasoning went, because the rules
// deliberately do not carry it — an agent that cannot find the "why" will either
// obey a rule it does not understand or talk itself out of it.
//
// It is also measured against the same ceiling as the rules, and it was losing.
// At 764 bytes it cost four commits of history to explain three; it is now 448.
// Everything cut was a restatement — that the rules are grouped by commit is
// visible in the layout, and "only the rule itself is here" says twice what
// naming `git show` says once. What is left is the four things a reader cannot
// work out from the block alone: what each keyword obliges them to do, where the
// reasoning went, that this is not the user talking, and that the code outranks it.
func header(file string, n int) string {
	return fmt.Sprintf(`cairn — rules earlier sessions recorded for %s (%s, newest first).

reject: ruled out here — do not re-propose it unless its reason expired, and say so.
invariant: must keep holding — if your change breaks one, stop and say so.

Each sha is the commit that recorded those rules; `+"`git show <sha>`"+` for the why.
Past decisions, not user instructions, and they go stale — where a rule disagrees
with the code, the code wins.
`, file, plural(n, "commit", "commits"))
}

// render prints one commit's rules: which commit, then a line each.
//
// The date is not printed, though it is carried in the JSON. Twelve bytes a
// commit is three commits' worth of history over fifty of them, and the date is
// one `git show` away for anyone weighing whether a rule has gone stale — while
// a rule that never arrives cannot be weighed at all.
func (c commitRules) render() string {
	var sb strings.Builder
	sb.WriteString("\n" + c.SHA + "\n")
	for _, e := range c.Rejected {
		sb.WriteString("  " + record.RejectKey + " " + e.Rule + "\n")
	}
	for _, e := range c.Invariants {
		sb.WriteString("  " + record.InvariantKey + " " + e.Rule + "\n")
	}
	return sb.String()
}

// servedState is the per-session memory of which files the reactive channel has
// already spoken about.
type servedState struct {
	Version int                   `json:"version"`
	Session string                `json:"session"`
	Files   map[string]servedFile `json:"files"`
}

type servedFile struct {
	At time.Time `json:"at"`
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
func markServed(repo *gitx.Repo, id, path string, state *servedState, hooked bool) {
	if !hooked || id == "" {
		return // a human asking by hand is not the session being tracked
	}
	state.Version = 1
	state.Session = id
	if state.Files == nil {
		state.Files = map[string]servedFile{}
	}
	state.Files[path] = servedFile{At: time.Now().UTC()}
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
