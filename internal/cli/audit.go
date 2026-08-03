package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/YUNGC0DE/git-cairn/internal/capture"
	"github.com/YUNGC0DE/git-cairn/internal/config"
	"github.com/YUNGC0DE/git-cairn/internal/distill"
	"github.com/YUNGC0DE/git-cairn/internal/gitx"
	"github.com/YUNGC0DE/git-cairn/internal/transcript"
)

// auditOutcome is one commit replayed through distillation.
type auditOutcome struct {
	Commit  string                `json:"commit"`
	Short   string                `json:"short"`
	Subject string                `json:"subject"`
	When    time.Time             `json:"when"`
	Files   []string              `json:"files"`
	Extract *distill.Extraction   `json:"extraction,omitempty"`
	Verify  *distill.Verification `json:"verification,omitempty"`
	Conf    distill.Confidence    `json:"confidence"`
	Notes   []string              `json:"notes,omitempty"`
	Err     string                `json:"error,omitempty"`
	Elapsed string                `json:"elapsed,omitempty"`
}

// cmdAudit answers the question the whole product rests on: do real agent sessions
// actually contain rejected alternatives and invariants, or is there nothing here
// worth serving back to the next agent?
//
// It replays past commits through the same DISTILL path the hook uses, so the
// numbers are the ones cairn would really produce — not an optimistic hand
// count. The JSON output doubles as the first eval corpus.
func cmdAudit(env *Env, args []string) error {
	fs := flags("audit", prog+" audit [-n N] [--since <date>] [--jobs N] [--out file.json] [--no-verify]", env.Out)
	limit := fs.Int("n", 20, "how many commits to examine, newest first")
	since := fs.String("since", "", "only commits after this date (git --since syntax)")
	jobs := fs.Int("jobs", 3, "how many commits to distil concurrently")
	out := fs.String("out", "", "write the full corpus as JSON to this file")
	noVerify := fs.Bool("no-verify", false, "skip verification (faster, but no confabulation rate)")
	model := fs.String("model", "", "model to distil with")
	budget := fs.Duration("budget", 90*time.Second, "per-session time budget (×N sessions; audit is not a hook, so it can be generous)")
	verbose := fs.Bool("v", false, "print every record, not just the summary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repo, err := openRepo(env)
	if err != nil {
		return err
	}
	cfg := config.Load(repo, env.Getenv)
	engine, err := env.engine(cfg.Engine)
	if err != nil {
		return err
	}

	// One extra commit so the oldest commit examined still has a lower time bound.
	logArgs := []string{"-n", strconv.Itoa(*limit + 1), "--no-merges"}
	if *since != "" {
		logArgs = append(logArgs, "--since", *since)
	}
	commits, err := repo.Log(logArgs, nil)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		return fmt.Errorf("no commits to audit")
	}

	refs, discErrs := capture.Discover(repo.Root, time.Time{})
	for _, e := range discErrs {
		fmt.Fprintf(env.Err, "cairn: %v\n", e)
	}
	if len(refs) == 0 {
		return fmt.Errorf("no agent transcripts found for %s — nothing to audit", repo.Root)
	}
	// Audit reads whole sessions and must not disturb the hook's bookkeeping: it
	// loads from a zero cursor and never saves offsets.
	fresh := &capture.Offsets{Cursors: map[string]transcript.Cursor{}}
	sessions, loadErrs := capture.LoadNew(refs, fresh)
	for _, e := range loadErrs {
		fmt.Fprintf(env.Err, "cairn: %v\n", e)
	}

	examined := min(*limit, len(commits))
	fmt.Fprintf(env.Out, "auditing %s against %s in %s\n",
		plural(examined, "commit", "commits"), plural(len(sessions), "session", "sessions"), repo.Root)

	type job struct {
		commit gitx.Commit
		input  distill.Input
	}
	var queue []job
	for i := 0; i < examined; i++ {
		c := commits[i]
		from := time.Time{}
		if i+1 < len(commits) {
			from = commits[i+1].When
		}
		var slices []*transcript.Session
		for _, s := range sessions {
			if w := s.Window(from, c.When); hasContent(w) {
				slices = append(slices, w)
			}
		}
		if len(slices) == 0 {
			continue
		}
		diff, truncated, err := repo.DiffOf(c.SHA, cfg.DiffBudget)
		if err != nil {
			fmt.Fprintf(env.Err, "cairn: diff %s: %v\n", c.Short, err)
			continue
		}
		files, _ := repo.FilesOf(c.SHA)
		queue = append(queue, job{commit: c, input: distill.Input{
			Sessions:      slices,
			Diff:          diff,
			DiffTruncated: truncated,
			Files:         files,
			Subject:       c.Subject,
		}})
	}
	fmt.Fprintf(env.Out, "%s line up with a transcript window\n\n", plural(len(queue), "commit", "commits"))
	if len(queue) == 0 {
		fmt.Fprintln(env.Out, "That is itself a finding: the transcripts on disk do not cover this history.")
		fmt.Fprintln(env.Out, "Audit a repository you have been working in with an agent recently.")
		return nil
	}

	results := make([]auditOutcome, len(queue))
	sem := make(chan struct{}, max(1, *jobs))
	var wg sync.WaitGroup
	var mu sync.Mutex
	completed := 0
	for i, j := range queue {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res, rerr := distill.Run(context.Background(), engine, j.input, distill.Options{
				Budget:       *budget,
				Model:        *model,
				VerifyModel:  cfg.VerifyModel,
				Effort:       cfg.Effort,
				PromptBudget: cfg.PromptBudget,
				SkipVerify:   *noVerify,
			})
			o := auditOutcome{
				Commit: j.commit.SHA, Short: j.commit.Short, Subject: j.commit.Subject,
				When: j.commit.When, Files: j.input.Files, Conf: distill.MetadataOnly,
			}
			if res != nil {
				o.Extract, o.Verify, o.Conf, o.Notes = res.Extraction, res.Verification, res.Confidence, res.Notes
				o.Elapsed = res.Elapsed.Round(100 * time.Millisecond).String()
			}
			if rerr != nil {
				o.Err = rerr.Error()
			}
			results[i] = o

			mu.Lock()
			completed++
			fmt.Fprintf(env.Err, "\rdistilled %d/%d", completed, len(queue))
			mu.Unlock()
		}(i, j)
	}
	wg.Wait()
	fmt.Fprintln(env.Err)

	auditReport(env, results, *verbose)

	if *out != "" {
		b, err := json.MarshalIndent(map[string]any{
			"repo":         repo.Root,
			"generated":    time.Now().UTC(),
			"engine":       engine.Name(),
			"cairnVersion": Version,
			"commits":      results,
		}, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(env.Out, "\ncorpus written to %s (%s)\n", *out, plural(len(results), "record", "records"))
	}
	return nil
}

// auditReport prints the numbers that decide whether the records in this repository
// are worth serving, and states plainly what they mean. The verdict is deliberately
// blunt: the point of the exercise is to be able to abandon the hypothesis, not to
// find comfort in it.
func auditReport(env *Env, results []auditOutcome, verbose bool) {
	var (
		distilled, failed                     int
		withRejected, withInvariants          int
		totalRejected, totalInvariants        int
		supported, contradicted, unverifiable int
		byConfidence                          = map[distill.Confidence]int{}
	)
	for _, r := range results {
		byConfidence[r.Conf]++
		if r.Extract == nil {
			failed++
			continue
		}
		distilled++
		if n := len(r.Extract.Rejected); n > 0 {
			withRejected++
			totalRejected += n
		}
		if n := len(r.Extract.Invariants); n > 0 {
			withInvariants++
			totalInvariants += n
		}
		if r.Verify != nil {
			for _, c := range r.Verify.Claims {
				switch c.Status {
				case distill.Supported:
					supported++
				case distill.Contradicted:
					contradicted++
				case distill.Unverifiable:
					unverifiable++
				}
			}
		}
	}

	if verbose {
		for _, r := range results {
			fmt.Fprintf(env.Out, "%s  %s\n", r.Short, r.Subject)
			if r.Err != "" {
				fmt.Fprintf(env.Out, "  ! %s\n\n", r.Err)
				continue
			}
			if r.Extract == nil {
				fmt.Fprintln(env.Out)
				continue
			}
			for _, w := range r.Extract.Why {
				fmt.Fprintln(env.Out, indent(wrapText("why: "+w, 72), "  "))
			}
			for _, rej := range r.Extract.Rejected {
				fmt.Fprintln(env.Out, indent(wrapText("rejected: "+rej.Option+" — "+rej.Because, 72), "  "))
			}
			for _, inv := range r.Extract.Invariants {
				line := "invariant: " + inv.Rule
				if len(inv.Scope) > 0 {
					line += " (" + strings.Join(inv.Scope, ", ") + ")"
				}
				fmt.Fprintln(env.Out, indent(wrapText(line, 72), "  "))
			}
			if r.Verify != nil {
				for _, c := range r.Verify.Claims {
					if c.Status != distill.Supported {
						fmt.Fprintf(env.Out, "    %s: %s (%s)\n", c.Status, trimTo(c.Claim, 70), c.Note)
					}
				}
			}
			fmt.Fprintf(env.Out, "  [%s, %s]\n\n", r.Conf, r.Elapsed)
		}
	}

	fmt.Fprintln(env.Out, "── corpus ──────────────────────────────────────────────")
	fmt.Fprintf(env.Out, "commits distilled          %d (%d failed)\n", distilled, failed)
	fmt.Fprintf(env.Out, "rejected alternatives      %d across %d commits\n", totalRejected, withRejected)
	fmt.Fprintf(env.Out, "invariant candidates       %d across %d commits\n", totalInvariants, withInvariants)
	if claims := supported + contradicted + unverifiable; claims > 0 {
		fmt.Fprintf(env.Out, "claims checked             %d (supported %d, unverifiable %d, contradicted %d)\n",
			claims, supported, unverifiable, contradicted)
		fmt.Fprintf(env.Out, "confabulation rate         %.1f%%  (contradicted / checked)\n",
			100*float64(contradicted)/float64(claims))
	} else {
		fmt.Fprintln(env.Out, "claims checked             0 — verification did not run, no confabulation rate")
	}
	var confs []string
	for _, c := range []distill.Confidence{distill.Verified, distill.Partial, distill.Disputed, distill.Unverified, distill.MetadataOnly} {
		if n := byConfidence[c]; n > 0 {
			confs = append(confs, fmt.Sprintf("%s %d", c, n))
		}
	}
	fmt.Fprintf(env.Out, "confidence                 %s\n", strings.Join(confs, ", "))

	fmt.Fprintln(env.Out, "\n── verdict ─────────────────────────────────────────────")
	if distilled == 0 {
		fmt.Fprintln(env.Out, "Nothing was distilled, so there is no verdict to give.")
		return
	}
	density := float64(totalRejected) / float64(distilled)
	fmt.Fprintf(env.Out, "%.1f rejected alternatives per commit (%d over %s)\n",
		density, totalRejected, plural(distilled, "commit", "commits"))

	// The question this answers: is there anything in your sessions worth serving
	// back? Rejections and invariants are the part of a record an agent cannot
	// re-derive from the diff, so their density is what the reactive channel has to
	// work with. A verdict needs a sample — pronouncing on three commits would be
	// noise dressed as evidence, so say so instead.
	const minSample = 5
	switch {
	case distilled < minSample:
		fmt.Fprintf(env.Out, "Too small a sample for a verdict — %s is not a month of history.\n",
			plural(distilled, "commit", "commits"))
		if totalRejected > 0 {
			fmt.Fprintln(env.Out, "The density above is encouraging; widen the range with -n or --since")
			fmt.Fprintln(env.Out, "and re-run before drawing any conclusion.")
		}
	case totalRejected >= 10 || density >= 0.4:
		fmt.Fprintln(env.Out, "Your sessions do settle things this way. That is exactly what the")
		fmt.Fprintln(env.Out, "reactive channel serves back, so records here are worth having.")
	case totalRejected >= 4:
		fmt.Fprintln(env.Out, "Thin. Audit more history before concluding anything. If it stays this")
		fmt.Fprintln(env.Out, "low, what an agent gets served is mostly intent — still useful, but the")
		fmt.Fprintln(env.Out, "'do not propose that again' half is barely there.")
	default:
		fmt.Fprintln(env.Out, "The negatives are not there. Take that seriously rather than tuning the")
		fmt.Fprintln(env.Out, "prompt: on this corpus a served record can only explain why the code is")
		fmt.Fprintln(env.Out, "the way it is — it has nothing to warn the next agent off.")
	}
	if totalInvariants == 0 && distilled >= minSample {
		fmt.Fprintln(env.Out, "No invariant candidates either — nothing here states what must stay true.")
	}
	if unverifiable > 0 && supported == 0 {
		fmt.Fprintln(env.Out, "\nEvery claim came back unverifiable. That usually means the diffs were")
		fmt.Fprintln(env.Out, "truncated — raise cairn.diffBudget and re-run before reading anything")
		fmt.Fprintln(env.Out, "into the confabulation rate.")
	}
}

func hasContent(s *transcript.Session) bool {
	for _, m := range s.Messages {
		if m.Text != "" || m.Thinking != "" || len(m.Tools) > 0 {
			return true
		}
	}
	return false
}
