package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cmdLogs prints what cairn did on recent commits.
//
// It reads the same file the hook writes, and exists for the case the file was
// written for: the commit was made from an editor's UI, the hook's stderr went
// nowhere, and the record came out thinner than expected.
func cmdLogs(env *Env, args []string) error {
	fs := flags("logs", "cairn logs [-n N] [--path]", env.Out)
	n := fs.Int("n", 20, "how many entries to show, newest last")
	showPath := fs.Bool("path", false, "print the log file's path and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repo, err := openRepo(env)
	if err != nil {
		return err
	}
	path := filepath.Join(repo.CairnDir(), runLogName)
	if *showPath {
		fmt.Fprintln(env.Out, path)
		return nil
	}

	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		fmt.Fprintf(env.Out, "No log yet: %s\n", rel(repo.Root, path))
		fmt.Fprintln(env.Out, "It is written from the first commit cairn records in this repository.")
		return nil
	}
	if err != nil {
		return err
	}

	// An entry is a timestamped line plus the indented notes beneath it, so
	// splitting on lines would cut a reason away from its outcome.
	var entries []string
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.HasPrefix(line, " ") && len(entries) > 0 {
			entries[len(entries)-1] += "\n" + line
			continue
		}
		entries = append(entries, line)
	}
	if *n > 0 && len(entries) > *n {
		entries = entries[len(entries)-*n:]
	}
	for _, e := range entries {
		fmt.Fprintln(env.Out, e)
	}
	return nil
}
