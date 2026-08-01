package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/YUNGC0DE/Cairn/internal/gitx"
)

// The run log answers one question: "the commit went through, the record came
// out thin — what happened?"
//
// Everything cairn says during a commit goes to the hook's stderr, which git
// forwards to the terminal. Commit from an editor's UI — the Cursor app, an IDE
// git panel — and that stream is usually swallowed, so a degraded record has no
// explanation anywhere. This file is that explanation: one line per run, in the
// repository's own state directory, never committed and safe to delete.
//
// It holds what cairn already prints, and nothing from the transcript: a record
// that must not leak into git history must not leak into a log either.
const (
	runLogName    = "log"
	runLogMaxSize = 256 << 10
)

// logRun appends one entry, with any notes indented beneath it. Failure to log
// is never allowed to matter: a commit does not fail over bookkeeping.
func logRun(repo *gitx.Repo, headline string, notes []string) {
	if repo == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", time.Now().UTC().Format(time.RFC3339), headline)
	for _, n := range notes {
		fmt.Fprintf(&b, "%s    %s\n", strings.Repeat(" ", len(time.RFC3339)-5), n)
	}

	path := repo.CairnDir() + "/" + runLogName
	if err := os.MkdirAll(repo.CairnDir(), 0o755); err != nil {
		return
	}
	trimRunLog(path)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(b.String())
}

// trimRunLog keeps the log bounded by dropping the older half. A log that grows
// forever in .git is a bug report waiting to happen; a log that rotates into
// numbered files is a directory nobody asked for.
func trimRunLog(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < runLogMaxSize {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	half := b[len(b)/2:]
	if i := strings.IndexByte(string(half), '\n'); i >= 0 {
		half = half[i+1:]
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, half, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}
