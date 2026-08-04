package distill

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/YUNGC0DE/git-cairn/internal/llm"
	"github.com/YUNGC0DE/git-cairn/internal/transcript"
)

// maxParallelExtractions bounds how many agent CLIs run at once.
//
// Not a throughput knob: measured on this machine, calls made back-to-back come
// back roughly three times slower than the same call in isolation — provider
// backoff under rapid repeated invocation. Four keeps a handful of sessions
// moving without tripping that backoff; the wall-clock budget scales with N.
const maxParallelExtractions = 4

// extractOutcome carries what the extraction phase learned besides the records
// themselves.
type extractOutcome struct {
	engine string
	model  string
	notes  []string
	err    error
}

// extractAll distils each session on its own, in parallel.
//
// One call per session is what makes a record independent of when the human
// committed: the same work produces the same prose whether it was committed
// session by session or all at once. Each call gets perSession of engine time —
// the same slice a solo commit would — and wall is the commit-wide ceiling so a
// hung agent cannot stall git forever. A session whose call does not finish is
// named in the notes rather than silently folded into someone else's intent.
func extractAll(ctx context.Context, engine llm.Engine, sessions []*transcript.Session,
	payloads []transcript.Compaction, in Input, opts Options, perSession, wall time.Duration,
	trace func(string, ...any)) ([]*Extraction, extractOutcome) {

	type result struct {
		i     int
		ex    *Extraction
		resp  *llm.Response
		err   error
		spent time.Duration
	}

	deadline := time.Now().Add(wall)
	results := make([]result, len(payloads))
	sem := make(chan struct{}, maxParallelExtractions)
	var wg sync.WaitGroup

	for i, p := range payloads {
		if strings.TrimSpace(p.Requests) == "" && strings.TrimSpace(p.Body) == "" {
			results[i] = result{i: i, err: fmt.Errorf("nothing to distil")}
			continue
		}
		wg.Add(1)
		go func(i int, p transcript.Compaction) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			left := time.Until(deadline)
			if left <= 0 {
				results[i] = result{i: i, err: fmt.Errorf("no time left in the budget")}
				return
			}
			// Cap at the per-session allowance; do not let one call eat the wall
			// time that belongs to the sessions still waiting on the semaphore.
			callBudget := perSession
			if callBudget > left {
				callBudget = left
			}
			start := time.Now()
			resp, err := engine.Complete(ctx, llm.Request{
				System: extractSystem,
				Prompt: extractPrompt(p.Requests, p.Body, in),
				Model:  opts.Model,
				Effort: opts.Effort,
				Budget: callBudget,
			})
			r := result{i: i, resp: resp, err: err, spent: time.Since(start)}
			if err == nil {
				var wire extraction
				if jsonErr := llm.ExtractJSON(resp.Text, &wire); jsonErr != nil {
					r.err = fmt.Errorf("unusable JSON: %w", jsonErr)
				} else {
					ex := wire.toExtraction()
					sanitize(ex, in)
					r.ex = ex
				}
			}
			results[i] = r
		}(i, p)
	}
	wg.Wait()

	var out extractOutcome
	var exs []*Extraction
	for i, r := range results {
		name := "session"
		if i < len(sessions) {
			name = short(sessions[i].ID) + " (" + sessions[i].Agent + ")"
		}
		if r.resp != nil {
			out.notes = append(out.notes, r.resp.Notes...)
			if out.engine == "" {
				out.engine, out.model = r.resp.Engine, r.resp.Model
			}
		}
		if r.err != nil {
			out.err = r.err
			out.notes = append(out.notes, fmt.Sprintf("%s not distilled: %v", name, r.err))
			trace("extract: %s failed after %s: %v", name, r.spent.Round(time.Millisecond), r.err)
			continue
		}
		trace("extract: %s done in %s", name, r.spent.Round(time.Millisecond))
		exs = append(exs, r.ex)
	}
	if len(exs) > 0 {
		out.err = nil
	}
	return exs, out
}

// merge stacks what each session produced into one record.
//
// Still naive concatenation rather than a second model call: a summarising pass
// is another chance to invent, and it would blur which session wanted what. What
// changed is that concatenation is no longer blind. Sessions behind one commit are
// usually one person circling one problem, so each independent extraction restates
// the same intention in its own words; joining them verbatim is how a record ends
// up saying the same thing four times, which is the single most common complaint
// about these records. Near-duplicates are folded here (see similar.go), and each
// distinct intention is kept as its own entry rather than glued into one
// paragraph — two sessions that wanted different things did not want one blended
// thing.
func merge(exs []*Extraction) (*Extraction, []string) {
	if len(exs) == 1 {
		return exs[0], nil
	}
	out := &Extraction{}
	for _, ex := range exs {
		out.Why = append(out.Why, ex.Why...)
		out.Rejected = append(out.Rejected, ex.Rejected...)
		out.Invariants = append(out.Invariants, ex.Invariants...)
		out.Claims = append(out.Claims, ex.Claims...)
	}
	rejectedIn, invariantsIn := len(out.Rejected), len(out.Invariants)
	why, droppedWhy := mergeWhy(out.Why)
	out.Why = why
	out.Rejected = dedupRejected(out.Rejected)
	out.Invariants = dedupInvariants(out.Invariants)
	out.Claims = dedupStrings(out.Claims)
	if len(out.Claims) > maxMergedClaims {
		out.Claims = out.Claims[:maxMergedClaims]
	}

	// Say what the merge removed. A record that silently drops one session's
	// account of what was wanted looks complete and is not, and the user is the
	// only one who can tell whether that session mattered.
	var notes []string
	if droppedWhy > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d of %d sessions restated an intention already recorded, or did not fit the "+
				"record's opening; theirs was left out", droppedWhy, len(exs)))
	}
	if n := rejectedIn - len(out.Rejected); n > 0 {
		notes = append(notes, fmt.Sprintf("%d rejected alternatives were the same option worded twice", n))
	}
	if n := invariantsIn - len(out.Invariants); n > 0 {
		notes = append(notes, fmt.Sprintf("%d invariants were the same rule worded twice", n))
	}
	return out, notes
}

// maxMergedClaims caps what the verification pass is asked to check. Claims are
// the only field where a merged commit is capped: they are consumed by one model
// call, and a list long enough to crowd out the diff makes every verdict worse.
// Rejections and invariants are not capped here — a commit that really spans four
// sessions carries what four commits would have, and a rejection dropped at this
// point is not stored anywhere else.
const maxMergedClaims = 8

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
