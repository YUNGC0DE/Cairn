package cli

import (
	"fmt"
	"strings"

	"github.com/YUNGC0DE/git-cairn/internal/gitx"
	"github.com/YUNGC0DE/git-cairn/internal/record"
)

// loadRecords reads commits and parses whatever record each carries. Notes are
// merged into the message first, so a reader does not need to know which mode
// the repository writes in — two receivers, one format.
//
// Only commits carrying a record are returned. An ordinary commit message says
// what changed, which the diff already says; a record says what must not change,
// which is the only thing worth spending an agent's context on.
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
		if err != nil || !r.Has() {
			continue
		}
		recs = append(recs, r)
	}
	return recs, commits, nil
}

// cmdShow is the human half of the loop the hooks automate: the rules of one
// commit, each with the justification the injected line deliberately leaves out.
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
	commits, err := repo.Log([]string{"-n", "1", rev}, nil)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		return fmt.Errorf("no commit found for %q", rev)
	}
	c := commits[0]
	if note := repo.Note(c.SHA); note != "" {
		c.Body = strings.TrimRight(c.Body+"\n\n"+note, "\n")
	}
	r, err := record.Parse(repo.Root, c)
	if err != nil {
		return err
	}
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
	for _, e := range r.Rejected {
		printEntry(env, record.RejectKey, e)
	}
	for _, e := range r.Invariants {
		printEntry(env, record.InvariantKey, e)
	}
	for _, d := range r.Disputed {
		fmt.Fprintln(env.Out, indent(wrapText("⚠ the diff contradicts: "+d, 72), "  "))
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

func printEntry(env *Env, key string, e record.Entry) {
	fmt.Fprintln(env.Out, indent(wrapText(key+" "+e.Rule, 72), "  "))
	if e.Why != "" {
		fmt.Fprintln(env.Out, indent(wrapText("why: "+e.Why, 68), "      "))
	}
	if len(e.Files) > 0 {
		fmt.Fprintf(env.Out, "      file: %s\n", strings.Join(e.Files, ", "))
	}
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
