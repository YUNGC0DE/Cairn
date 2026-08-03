// Package gitx wraps the git CLI.
//
// A git library (go-git) would do the reads. We shell out for both reads and
// writes instead. Reasons: `git log
// --follow`, `git interpret-trailers`, notes, signing, hooks and the user's own
// config all come for free and behave identically to the user's git. Reads are
// scoped and rare (a hook does one `diff --cached`), so process overhead is not
// on the hot path.
package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Repo is a handle on a git working tree.
type Repo struct {
	Root   string // absolute path to the working tree root
	GitDir string // absolute path to the .git dir (resolved for worktrees)
}

// Run executes git in dir and returns trimmed stdout.
func Run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// RunInput executes git in dir feeding stdin, and returns raw stdout.
func RunInput(dir, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out.String(), nil
}

// Open resolves the repository containing dir.
func Open(dir string) (*Repo, error) {
	root, err := Run(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	gd, err := Run(dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	root, _ = filepath.Abs(root)
	return &Repo{Root: root, GitDir: gd}, nil
}

// CairnDir is the local, non-versioned state directory inside .git.
func (r *Repo) CairnDir() string { return filepath.Join(r.GitDir, "cairn") }

// StagedDiff returns the staged diff, truncated to maxBytes on a line
// boundary. Binary files are excluded from content but kept in the stat.
func (r *Repo) StagedDiff(maxBytes int) (diff string, truncated bool, err error) {
	out, err := Run(r.Root, "diff", "--cached", "--no-color", "--no-ext-diff", "-M", "--unified=3")
	if err != nil {
		return "", false, err
	}
	if maxBytes > 0 && len(out) > maxBytes {
		cut := out[:maxBytes]
		if i := strings.LastIndexByte(cut, '\n'); i > 0 {
			cut = cut[:i]
		}
		stat, _ := Run(r.Root, "diff", "--cached", "--stat")
		return cut + "\n\n[diff truncated by cairn; full stat follows]\n" + stat, true, nil
	}
	return out, false, nil
}

// StagedFiles lists paths in the index relative to the repo root.
func (r *Repo) StagedFiles() ([]string, error) {
	out, err := Run(r.Root, "diff", "--cached", "--name-only")
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// HasStaged reports whether anything is staged.
func (r *Repo) HasStaged() bool {
	out, err := Run(r.Root, "diff", "--cached", "--name-only")
	return err == nil && strings.TrimSpace(out) != ""
}

// HeadTime returns the commit time of HEAD. Zero time on an unborn branch.
func (r *Repo) HeadTime() (time.Time, error) {
	out, err := Run(r.Root, "log", "-1", "--format=%ct")
	if err != nil || out == "" {
		return time.Time{}, nil //nolint:nilerr // unborn branch is not an error
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, 0), nil
}

// HeadSHA returns the current HEAD sha, or "" on an unborn branch.
func (r *Repo) HeadSHA() string {
	out, _ := Run(r.Root, "rev-parse", "HEAD")
	return out
}

// Branch returns the current branch name, or "" when detached.
func (r *Repo) Branch() string {
	out, _ := Run(r.Root, "symbolic-ref", "--short", "-q", "HEAD")
	return out
}

// Commit describes one commit with its full message.
type Commit struct {
	SHA     string
	Short   string
	Author  string
	When    time.Time
	Subject string
	Body    string
	Files   []string
}

// Message reassembles the full commit message.
func (c Commit) Message() string {
	if c.Body == "" {
		return c.Subject
	}
	return c.Subject + "\n\n" + c.Body
}

const logSep = "\x1e" // record separator
const fieldSep = "\x1f"

// Log returns commits matching the given revision range / path filters.
// extraArgs are passed to git log verbatim (e.g. "--follow", "-20").
func (r *Repo) Log(extraArgs []string, paths []string) ([]Commit, error) {
	args := []string{"log", "--format=" + logSep + "%H" + fieldSep + "%h" + fieldSep + "%an" + fieldSep + "%ct" + fieldSep + "%s" + fieldSep + "%b"}
	args = append(args, extraArgs...)
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	out, err := Run(r.Root, args...)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, rec := range strings.Split(out, logSep) {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, fieldSep, 6)
		if len(f) < 6 {
			continue
		}
		sec, _ := strconv.ParseInt(strings.TrimSpace(f[3]), 10, 64)
		commits = append(commits, Commit{
			SHA:     f[0],
			Short:   f[1],
			Author:  f[2],
			When:    time.Unix(sec, 0),
			Subject: f[4],
			Body:    strings.TrimRight(f[5], "\n"),
		})
	}
	return commits, nil
}

// FilesOf lists paths touched by a commit.
func (r *Repo) FilesOf(sha string) ([]string, error) {
	out, err := Run(r.Root, "show", "--name-only", "--format=", "-M", sha)
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// DiffOf returns the diff a commit introduced, truncated to maxBytes.
func (r *Repo) DiffOf(sha string, maxBytes int) (string, bool, error) {
	out, err := Run(r.Root, "show", "--no-color", "--no-ext-diff", "-M", "--unified=3", "--format=", sha)
	if err != nil {
		return "", false, err
	}
	if maxBytes > 0 && len(out) > maxBytes {
		cut := out[:maxBytes]
		if i := strings.LastIndexByte(cut, '\n'); i > 0 {
			cut = cut[:i]
		}
		stat, _ := Run(r.Root, "show", "--stat", "--format=", sha)
		return cut + "\n\n[diff truncated by cairn; full stat follows]\n" + stat, true, nil
	}
	return out, false, nil
}

// ParseTrailers runs the stock `git interpret-trailers --parse` and returns
// trailers in order. We never write our own trailer parser.
func ParseTrailers(dir, message string) ([][2]string, error) {
	out, err := RunInput(dir, message, "interpret-trailers", "--parse")
	if err != nil {
		return nil, err
	}
	var res [][2]string
	for _, line := range splitLines(out) {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		res = append(res, [2]string{strings.TrimSpace(k), strings.TrimSpace(v)})
	}
	return res, nil
}

// Trailer returns the first value of the named trailer (case-insensitive).
func Trailer(trailers [][2]string, name string) string {
	for _, t := range trailers {
		if strings.EqualFold(t[0], name) {
			return t[1]
		}
	}
	return ""
}

// AddTrailers appends trailers to a message via git interpret-trailers,
// letting git handle blank-line and separator rules.
func AddTrailers(dir, message string, trailers [][2]string) (string, error) {
	args := []string{"interpret-trailers"}
	for _, t := range trailers {
		if t[1] == "" {
			continue
		}
		args = append(args, "--trailer", t[0]+": "+t[1])
	}
	if len(args) == 1 {
		return message, nil
	}
	return RunInput(dir, message, args...)
}

// NotesRef is where cairn records live in notes mode.
const NotesRef = "refs/notes/cairn"

// AddNote attaches (or replaces) a cairn note on a commit.
func (r *Repo) AddNote(sha, body string) error {
	_, err := RunInput(r.Root, body, "notes", "--ref="+NotesRef, "add", "-f", "-F", "-", sha)
	return err
}

// Note reads the cairn note of a commit, "" when absent.
func (r *Repo) Note(sha string) string {
	out, err := Run(r.Root, "notes", "--ref="+NotesRef, "show", sha)
	if err != nil {
		return ""
	}
	return out
}

// ConfigGet reads a git config value, "" when unset.
func (r *Repo) ConfigGet(key string) string {
	out, _ := Run(r.Root, "config", "--get", key)
	return out
}

var ErrNotRepo = errors.New("not a git repository")

func splitLines(s string) []string {
	var res []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			res = append(res, l)
		}
	}
	return res
}
