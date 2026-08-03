package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/YUNGC0DE/git-cairn/internal/gitx"
	"github.com/YUNGC0DE/git-cairn/internal/record"
)

// loadRecords reads commits and parses whatever record each carries. Notes are
// merged into the message first, so a reader does not need to know which mode
// the repository writes in — two receivers, one format.
func loadRecords(repo *gitx.Repo, logArgs, paths []string) ([]*record.Record, []gitx.Commit, error) {
	commits, err := repo.Log(logArgs, paths)
	if err != nil {
		return nil, nil, err
	}
	var recs []*record.Record
	for _, c := range commits {
		if note := repo.Note(c.SHA); note != "" {
			c.Body = strings.TrimRight(c.Body+"\n\n"+note, "\n")
		}
		r, err := record.Parse(repo.Root, c)
		if err != nil {
			continue
		}
		recs = append(recs, r)
	}
	return recs, commits, nil
}

func cmdWhy(env *Env, args []string) error {
	fs := flags("why", prog+" why <path>[:line] [-n N]", env.Out)
	limit := fs.Int("n", 10, "how many commits to look back through")
	all := fs.Bool("all", false, "include commits with no cairn record")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return ErrUsage
	}
	repo, err := openRepo(env)
	if err != nil {
		return err
	}
	path, line := splitPathLine(fs.Arg(0))

	// --follow tracks a file across renames, which is exactly the question "why
	// is this code like this" asks. It only works for a single path.
	logArgs := []string{"-n", strconv.Itoa(*limit), "--follow"}
	recs, _, err := loadRecords(repo, logArgs, []string{path})
	if err != nil {
		// --follow refuses on directories; fall back to a plain path filter.
		recs, _, err = loadRecords(repo, []string{"-n", strconv.Itoa(*limit)}, []string{path})
		if err != nil {
			return err
		}
	}

	header := path
	if line > 0 {
		header = fmt.Sprintf("%s:%d", path, line)
	}
	fmt.Fprintf(env.Out, "why %s\n\n", header)

	shown := 0
	for _, r := range recs {
		if !r.Has() && !*all {
			continue
		}
		printRecord(env, r)
		shown++
	}
	if shown == 0 {
		fmt.Fprintf(env.Out, "No cairn records touch this path in the last %s.\n", plural(*limit, "commit", "commits"))
		fmt.Fprintf(env.Out, "Records start accumulating from the first agent commit after `%s init`.\n", prog)
		fmt.Fprintln(env.Out, "Re-run with --all to see the plain commit subjects.")
	}
	return nil
}

func cmdRejected(env *Env, args []string) error {
	fs := flags("rejected", prog+" rejected <query> [-n N]", env.Out)
	limit := fs.Int("n", 2000, "how many commits to search")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repo, err := openRepo(env)
	if err != nil {
		return err
	}
	query := strings.ToLower(strings.Join(fs.Args(), " "))

	// git does the first cut; notes-mode records are not greppable this way, so
	// the query is re-applied in full below.
	recs, _, err := loadRecords(repo, []string{"-n", strconv.Itoa(*limit)}, nil)
	if err != nil {
		return err
	}
	hits := 0
	for _, r := range recs {
		for _, rej := range r.Rejected {
			if query != "" && !strings.Contains(strings.ToLower(rej), query) {
				continue
			}
			fmt.Fprintf(env.Out, "%s  %s\n", r.Short, r.Subject)
			fmt.Fprintf(env.Out, "  rejected: %s\n", rej)
			if r.Confidence != "" {
				fmt.Fprintf(env.Out, "  confidence: %s\n", r.Confidence)
			}
			fmt.Fprintln(env.Out)
			hits++
		}
	}
	if hits == 0 {
		if query == "" {
			fmt.Fprintln(env.Out, "No rejected alternatives recorded yet.")
		} else {
			fmt.Fprintf(env.Out, "Nothing rejected matching %q in the last %s.\n", query, plural(*limit, "commit", "commits"))
		}
	}
	return nil
}

func cmdShow(env *Env, args []string) error {
	fs := flags("show", prog+" show [<commit>]", env.Out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	repo, err := openRepo(env)
	if err != nil {
		return err
	}
	rev := "HEAD"
	if fs.NArg() > 0 {
		rev = fs.Arg(0)
	}
	recs, _, err := loadRecords(repo, []string{"-n", "1", rev}, nil)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return fmt.Errorf("no commit found for %q", rev)
	}
	r := recs[0]
	if !r.Has() {
		fmt.Fprintf(env.Out, "%s  %s\n\nNo cairn record on this commit.\n", r.Short, r.Subject)
		return nil
	}
	printRecord(env, r)
	if len(r.Transcripts) > 0 {
		fmt.Fprintf(env.Out, "  transcript: %s\n", strings.Join(r.Transcripts, " "))
		fmt.Fprintln(env.Out, "  (pointer only — cairn never stores transcript contents)")
	}
	return nil
}

func printRecord(env *Env, r *record.Record) {
	fmt.Fprintf(env.Out, "%s  %s\n", r.Short, r.Subject)
	for _, w := range r.Why {
		fmt.Fprintln(env.Out, indent(wrapText("why: "+w, 72), "  "))
	}
	for _, rej := range r.Rejected {
		fmt.Fprintln(env.Out, indent(wrapText("rejected: "+rej, 72), "  "))
	}
	for _, inv := range r.Invariants {
		fmt.Fprintln(env.Out, indent(wrapText("invariant: "+inv, 72), "  "))
	}
	for _, d := range r.Disputed {
		fmt.Fprintln(env.Out, indent(wrapText("⚠ unconfirmed: "+d, 72), "  "))
	}
	meta := []string{}
	if r.Agent != "" {
		meta = append(meta, r.Agent)
	}
	if r.Confidence != "" {
		meta = append(meta, r.Confidence)
	}
	if len(meta) > 0 {
		fmt.Fprintf(env.Out, "  [%s]\n", strings.Join(meta, ", "))
	}
	fmt.Fprintln(env.Out)
}

// splitPathLine accepts "file.go:47" as well as "file.go". The line number is
// displayed but not used to filter: git log cannot filter by line without -L,
// which needs a range and hides merge commits.
func splitPathLine(arg string) (string, int) {
	if i := strings.LastIndexByte(arg, ':'); i > 0 {
		if n, err := strconv.Atoi(arg[i+1:]); err == nil {
			return arg[:i], n
		}
	}
	return arg, 0
}

func wrapText(s string, width int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	n := 0
	for i, w := range words {
		switch {
		case i == 0:
			b.WriteString(w)
			n = len(w)
		case n+1+len(w) > width:
			b.WriteString("\n" + w)
			n = len(w)
		default:
			b.WriteString(" " + w)
			n += 1 + len(w)
		}
	}
	return b.String()
}
