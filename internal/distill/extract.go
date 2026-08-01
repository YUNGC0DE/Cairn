package distill

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/YUNGC0DE/Cairn/internal/llm"
	"github.com/YUNGC0DE/Cairn/internal/transcript"
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
				var ex Extraction
				if jsonErr := llm.ExtractJSON(resp.Text, &ex); jsonErr != nil {
					r.err = fmt.Errorf("unusable JSON: %w", jsonErr)
				} else {
					sanitize(&ex, in)
					r.ex = &ex
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
// Prose is concatenated in session order — the same paragraphs separate commits
// would have carried. Lists are concatenated and deduplicated, because two
// sessions that rejected the same option said one thing, not two. next_step
// comes from the newest session: it is the only field where a later session
// genuinely supersedes an earlier one.
func merge(exs []*Extraction) *Extraction {
	if len(exs) == 1 {
		return exs[0]
	}
	out := &Extraction{}
	seenRejected := map[string]bool{}
	seenInvariant := map[string]bool{}
	seenOpen := map[string]bool{}
	var intents, decisions []string

	for _, ex := range exs {
		if s := strings.TrimSpace(ex.Intent); s != "" {
			intents = append(intents, s)
		}
		if s := strings.TrimSpace(ex.Decision); s != "" {
			decisions = append(decisions, s)
		}
		for _, r := range ex.Rejected {
			if k := strings.ToLower(strings.TrimSpace(r.Option)); k != "" && !seenRejected[k] {
				seenRejected[k] = true
				out.Rejected = append(out.Rejected, r)
			}
		}
		for _, iv := range ex.Invariants {
			if k := strings.ToLower(strings.TrimSpace(iv.Text)); k != "" && !seenInvariant[k] {
				seenInvariant[k] = true
				out.Invariants = append(out.Invariants, iv)
			}
		}
		for _, o := range ex.OpenItems {
			if k := strings.ToLower(strings.TrimSpace(o)); k != "" && !seenOpen[k] {
				seenOpen[k] = true
				out.OpenItems = append(out.OpenItems, o)
			}
		}
		out.Claims = append(out.Claims, ex.Claims...)
		if s := strings.TrimSpace(ex.NextStep); s != "" {
			out.NextStep = s
		}
	}
	out.Intent = strings.Join(intents, "\n\n")
	out.Decision = strings.Join(decisions, "\n\n")
	// The author's own subject wins over several sessions' guesses at one.
	out.Subject = ""
	out.Claims = dedupClaims(out.Claims)
	return out
}

func dedupClaims(claims []string) []string {
	seen := map[string]bool{}
	out := claims[:0]
	for _, c := range claims {
		k := strings.ToLower(strings.TrimSpace(c))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, c)
	}
	// Verification costs one line per claim and a reader's patience; keep the
	// sharpest handful rather than every session's full set.
	if len(out) > maxMergedClaims {
		out = out[:maxMergedClaims]
	}
	return out
}

// maxMergedClaims caps what the verification pass is asked to check.
const maxMergedClaims = 10

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
