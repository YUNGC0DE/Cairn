package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YUNGC0DE/Cairn/internal/capture"
	"github.com/YUNGC0DE/Cairn/internal/config"
	"github.com/YUNGC0DE/Cairn/internal/distill"
	"github.com/YUNGC0DE/Cairn/internal/gitx"
	"github.com/YUNGC0DE/Cairn/internal/record"
	"github.com/YUNGC0DE/Cairn/internal/transcript"
)

func cmdHook(env *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("hook: which hook?")
	}
	switch args[0] {
	case "prepare-commit-msg":
		return hookPrepareCommitMsg(env, args[1:])
	case "post-commit":
		return hookPostCommit(env, args[1:])
	default:
		return fmt.Errorf("hook: cairn does not handle %q", args[0])
	}
}

// pending is the state prepare-commit-msg hands to post-commit.
type pending struct {
	Cursors map[string]transcript.Cursor `json:"cursors"`
	Note    string                       `json:"note,omitempty"`
	Mode    config.Mode                  `json:"mode"`
	Written time.Time                    `json:"written"`

	// MessageDigest ties this pending record to the commit it was prepared for.
	// Without it a leftover pending — from a commit the author abandoned in the
	// editor — would be applied to whatever commit came next, attaching a note to
	// the wrong commit and consuming a transcript that was never recorded.
	MessageDigest string `json:"messageDigest"`
}

// messageDigest fingerprints a commit message the way git will store it:
// comments stripped and whitespace normalised, so the value computed in
// prepare-commit-msg matches the one computed from the finished commit.
func messageDigest(repoDir, message string) string {
	clean, err := gitx.RunInput(repoDir, message, "stripspace", "--strip-comments")
	if err != nil {
		clean = message
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(clean)))
	return hex.EncodeToString(sum[:])
}

func pendingPath(repo *gitx.Repo) string {
	return filepath.Join(repo.CairnDir(), "pending.json")
}

// hookPrepareCommitMsg is CAPTURE + DISTILL (spec §6.1). It must never fail a
// commit and never hang one, so every error path returns nil.
func hookPrepareCommitMsg(env *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("prepare-commit-msg: no message file given")
	}
	msgFile, source := args[0], ""
	if len(args) > 1 {
		source = args[1]
	}

	repo, err := gitx.Open(env.Dir)
	if err != nil {
		return nil // not a repo: nothing to do
	}
	cfg := config.Load(repo, env.Getenv)
	log := debugf(env, cfg.Debug)

	if !cfg.Enabled {
		log("disabled by config")
		return nil
	}
	// A merge or squash message is machine-generated and belongs to git, not to
	// one agent session.
	if source == "merge" || source == "squash" {
		log("skipping: source=%s", source)
		return nil
	}

	raw, err := os.ReadFile(msgFile)
	if err != nil {
		log("cannot read %s: %v", msgFile, err)
		return nil
	}
	body, tail := splitMessage(repo, string(raw))
	if strings.Contains(body, record.TrailerAgent+":") {
		log("skipping: message already carries a record (amend or re-run)")
		return nil
	}
	if !repo.HasStaged() {
		log("skipping: nothing staged")
		return nil
	}

	// CAPTURE. Sessions that have not changed since the last commit cannot have
	// produced this one.
	since, _ := repo.HeadTime()
	refs, discErr := capture.Discover(repo.Root, since)
	for _, e := range discErr {
		log("discovery: %v", e)
	}
	offsets, err := capture.LoadOffsets(repo.CairnDir())
	if err != nil {
		log("offsets: %v", err)
	}
	sessions, loadErrs := capture.LoadNew(refs, offsets)
	for _, e := range loadErrs {
		log("load: %v", e)
	}
	if len(sessions) == 0 {
		log("no new agent transcripts for %s — human commit, leaving it alone", repo.Root)
		return nil
	}

	diff, truncated, err := repo.StagedDiff(cfg.DiffBudget)
	if err != nil {
		log("diff: %v", err)
		return nil
	}
	files, _ := repo.StagedFiles()

	in := distill.Input{
		Sessions:      sessions,
		Diff:          diff,
		DiffTruncated: truncated,
		Files:         files,
		Subject:       firstNonEmptyLine(body),
	}
	meta := metaFor(sessions, files)

	// DISTILL. Any failure degrades to trailers only; the pointer to the
	// transcript survives even when the prose does not.
	var res *distill.Result
	engine, err := env.engine(cfg.Engine)
	if err != nil {
		log("no engine: %v", err)
		meta.Confidence = distill.MetadataOnly
	} else {
		res, err = distill.Run(context.Background(), engine, in, distill.Options{
			Budget:       cfg.Budget,
			Model:        cfg.Model,
			VerifyModel:  cfg.VerifyModel,
			PromptBudget: cfg.PromptBudget,
			Trace:        log,
		})
		if err != nil {
			log("distill: %v", err)
		}
		if res != nil {
			meta.Confidence = res.Confidence
			for _, d := range res.DisputedClaims() {
				meta.Disputed = append(meta.Disputed, d.Claim)
			}
			if res.Model != "" {
				meta.Agent = agentLabel(sessions, res.Model)
			}
		}
	}

	prose := record.Body(res)
	switch cfg.Mode {
	case config.ModeNotes:
		// Keep the commit message untouched; the note is attached post-commit.
		note, err := record.Compose(repo.Root, "", prose, meta)
		if err != nil {
			log("compose note: %v", err)
			return nil
		}
		if err := savePending(repo, offsets, sessions, note, cfg.Mode,
			messageDigest(repo.Root, body)); err != nil {
			log("pending: %v", err)
		}
		summarize(env, res, meta, sessions, "note pending")
	default:
		msg, err := record.Compose(repo.Root, body, prose, meta)
		if err != nil {
			log("compose message: %v", err)
			return nil
		}
		if err := os.WriteFile(msgFile, []byte(joinMessage(msg, tail)), 0o644); err != nil {
			log("write %s: %v", msgFile, err)
			return nil
		}
		if err := savePending(repo, offsets, sessions, "", cfg.Mode,
			messageDigest(repo.Root, msg)); err != nil {
			log("pending: %v", err)
		}
		summarize(env, res, meta, sessions, "")
	}
	return nil
}

// savePending stores the cursors this commit consumed. When cairn's post-commit
// hook is installed the offsets are committed there, so an aborted commit can be
// retried against the same transcript; otherwise they are committed immediately,
// trading that safety for not re-recording the same session twice.
func savePending(repo *gitx.Repo, offsets *capture.Offsets, sessions []*transcript.Session, note string, mode config.Mode, digest string) error {
	cursors := map[string]transcript.Cursor{}
	for _, s := range sessions {
		cursors[s.Key] = s.Cursor
	}
	if !installedHook(repo, "post-commit") {
		for k, c := range cursors {
			offsets.Set(k, c, "")
		}
		return offsets.Save()
	}
	if err := os.MkdirAll(repo.CairnDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(pending{
		Cursors: cursors, Note: note, Mode: mode, Written: time.Now().UTC(),
		MessageDigest: digest,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pendingPath(repo), append(b, '\n'), 0o644)
}

// hookPostCommit finalises what prepare-commit-msg staged: it attaches the note
// in notes mode and advances the read offsets.
func hookPostCommit(env *Env, _ []string) error {
	repo, err := gitx.Open(env.Dir)
	if err != nil {
		return nil
	}
	cfg := config.Load(repo, env.Getenv)
	log := debugf(env, cfg.Debug)

	path := pendingPath(repo)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil // nothing pending: a plain human commit
	}
	defer os.Remove(path)

	var p pending
	if err := json.Unmarshal(b, &p); err != nil {
		log("pending unreadable: %v", err)
		return nil
	}
	sha := repo.HeadSHA()
	// Confirm this pending record belongs to the commit that just landed. A
	// mismatch means the prepared commit never happened, so the transcript stays
	// unconsumed and the next commit records it instead.
	if p.MessageDigest != "" {
		head, err := gitx.Run(repo.Root, "log", "-1", "--format=%B")
		if err != nil || messageDigest(repo.Root, head) != p.MessageDigest {
			log("discarding pending state: it was prepared for a different commit")
			return nil
		}
	}
	if p.Note != "" && sha != "" {
		if err := repo.AddNote(sha, p.Note); err != nil {
			log("notes: %v", err)
			// The record is lost, so do not consume the transcript: the next
			// commit will try again rather than leave a silent hole.
			return nil
		}
		fmt.Fprintf(env.Err, "cairn: record written to %s for %s\n", gitx.NotesRef, short(sha))
	}
	offsets, err := capture.LoadOffsets(repo.CairnDir())
	if err != nil {
		log("offsets: %v", err)
		return nil
	}
	for k, c := range p.Cursors {
		offsets.Set(k, c, sha)
	}
	if err := offsets.Save(); err != nil {
		log("offsets save: %v", err)
	}
	return nil
}

// metaFor assembles the trailer payload from the captured sessions.
func metaFor(sessions []*transcript.Session, files []string) record.Meta {
	m := record.Meta{Files: files}
	for _, s := range sessions {
		m.Sessions = append(m.Sessions, short(s.ID))
		if p := capture.TranscriptPointer(s.Ref); p != "" {
			m.Transcripts = append(m.Transcripts, p)
		}
	}
	m.Sessions = record.Dedup(m.Sessions)
	m.Transcripts = record.Dedup(m.Transcripts)
	m.Agent = agentLabel(sessions, "")
	return m
}

// agentLabel renders "claude-code/sonnet", naming the agent that did the work
// and the model that distilled it when they differ.
func agentLabel(sessions []*transcript.Session, distillModel string) string {
	var agents, models []string
	for _, s := range sessions {
		agents = append(agents, s.Agent)
		if s.Model != "" {
			models = append(models, s.Model)
		}
	}
	label := strings.Join(record.Dedup(agents), "+")
	if len(models) > 0 {
		label += "/" + strings.Join(record.Dedup(models), "+")
	} else if distillModel != "" {
		label += "/?"
	}
	if distillModel != "" {
		label += " (distilled by " + distillModel + ")"
	}
	return label
}

// summarize prints the one line that makes cairn trustworthy: what it recorded,
// how confident it is, and how long it cost.
func summarize(env *Env, res *distill.Result, meta record.Meta, sessions []*transcript.Session, suffix string) {
	var parts []string
	parts = append(parts, string(meta.Confidence))
	if res != nil && res.Extraction != nil {
		if n := len(res.Extraction.Rejected); n > 0 {
			parts = append(parts, plural(n, "rejected", "rejected"))
		}
		if n := len(res.Extraction.Invariants); n > 0 {
			parts = append(parts, plural(n, "invariant", "invariants"))
		}
	}
	if n := len(meta.Disputed); n > 0 {
		parts = append(parts, plural(n, "disputed claim", "disputed claims"))
	}
	if res != nil && res.Elapsed > 0 {
		parts = append(parts, res.Elapsed.Round(100*time.Millisecond).String())
	}
	if suffix != "" {
		parts = append(parts, suffix)
	}
	fmt.Fprintf(env.Err, "cairn: %s from %s [%s]\n",
		verb(meta.Confidence), meta.Agent, strings.Join(parts, ", "))
	if res != nil {
		for _, n := range res.Notes {
			fmt.Fprintf(env.Err, "cairn:   %s\n", n)
		}
	}
	_ = sessions
}

func verb(c distill.Confidence) string {
	if c == distill.MetadataOnly {
		return "trailers only"
	}
	return "recorded"
}

// splitMessage separates the message a human would edit from git's trailing
// comment block (and the verbose-mode diff after the scissors line), so cairn
// appends to the former and leaves the latter alone.
func splitMessage(repo *gitx.Repo, raw string) (body, tail string) {
	cc := repo.ConfigGet("core.commentChar")
	if cc == "" || cc == "auto" || len(cc) > 1 {
		cc = "#"
	}
	lines := strings.Split(raw, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, cc) {
			return strings.Join(lines[:i], "\n"), strings.Join(lines[i:], "\n")
		}
	}
	return raw, ""
}

func joinMessage(body, tail string) string {
	body = strings.TrimRight(body, "\n") + "\n"
	if tail == "" {
		return body
	}
	return body + tail
}

func firstNonEmptyLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// debugf returns a logger that is silent unless CAIRN_DEBUG is set.
func debugf(env *Env, on bool) func(string, ...any) {
	if !on {
		return func(string, ...any) {}
	}
	return func(format string, args ...any) {
		fmt.Fprintf(env.Err, "cairn: "+format+"\n", args...)
	}
}
