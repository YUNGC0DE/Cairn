package distill

import "strings"

// Near-duplicate detection, and why a record needs it at all.
//
// Extraction runs once per session, and the sessions behind one commit are
// usually the same person circling the same problem. Each call independently
// writes down the intention it saw, so a four-session commit used to carry the
// same paragraph four times in four wordings, and the same rejected option twice
// with different phrasing. Exact-string dedup — what this replaced — caught none
// of it: measured over this repository's own history, 39 rejected alternatives
// across 7 commits included at least four pairs that were the same option said
// twice.
//
// The measure is lexical on purpose. A model call could judge sameness better,
// but it would be a third pass inside a git hook, and being wrong here is cheap
// in one direction only: keeping a near-duplicate wastes a few lines, while
// dropping a distinct rejection loses it permanently. So the threshold is set
// where it catches rewordings of the same sentence and leaves anything arguable
// alone.

// dupThreshold is the token overlap above which two entries are treated as one.
//
// Calibrated against real pairs from this repository: "Snapshot/copy state.vscdb
// before every read" against "Snapshot/copy Cursor's state.vscdb before every
// read" scores 0.88, and "Keep one shared prompt/time budget across all sessions
// in a commit" against "One shared prompt/time budget split across however many
// sessions are in a commit" scores 0.62. Distinct rejections from the same
// session score below 0.3. Two sentences that mean the same thing with almost no
// shared vocabulary are missed, and that is the tolerable failure.
const dupThreshold = 0.55

// invSoftThreshold is the floor for the heavier invariant check in
// similarInvariant. Plain overlap at this level is not enough — see that
// function — because "first rule that must hold" against "second rule that must
// hold" scores 0.50 on boilerplate alone.
const invSoftThreshold = 0.32

// whyDupThreshold is deliberately stricter than dupThreshold.
//
// Two sessions behind one commit routinely restate the same intention in words
// that share almost no vocabulary — measured on this repository's own history, a
// real pair of restated intentions scored 0.25 — so a lexical measure was never
// going to catch those, and lowering the bar until it did would start folding two
// genuinely different intentions into one. Length is what catches restatement
// here (see mergeWhy); similarity only catches the near-verbatim case, and it has
// to be sure, because a folded-away "why" is a session's purpose lost.
const whyDupThreshold = 0.7

// sameThreshold is for asking whether one string is a rewrite of another (a
// reason that only repeats its option, a "why" that is the subject line again).
// It is higher because the answer means "this says nothing new", which is a
// stronger claim than "these are the same topic".
const sameThreshold = 0.75

// similar reports whether two strings say the same thing, by token overlap.
func similar(a, b string, threshold float64) bool {
	ta, tb := tokens(a), tokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	// Two entries that quote different numbers are not one entry. In a record the
	// number is usually the whole argument — "raise the timeout to 30s" against
	// "raise it to 60s", 24KB against 256MiB — and everything around it is the same
	// sentence, which is exactly the shape token overlap mistakes for a duplicate.
	if !sameNumbers(ta, tb) {
		return false
	}
	// A very short entry shares few tokens with anything, so overlap is noisy;
	// only an exact match counts for those.
	if len(ta) < 3 || len(tb) < 3 {
		return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
	}
	shared := 0
	for t := range ta {
		if tb[t] {
			shared++
		}
	}
	union := len(ta) + len(tb) - shared
	if union == 0 {
		return false
	}
	return float64(shared)/float64(union) >= threshold
}

// sameThing reports whether b adds nothing to a.
func sameThing(a, b string) bool { return similar(a, b, sameThreshold) }

// similarInvariant reports whether two rules are the same property.
//
// Plain dupThreshold misses the common case: several sessions independently
// write "binary is named git-cairn / help follows argv[0]" in different words
// and score only 0.32–0.40. Lowering the bar for everything would also fold
// "first rule that must hold" into "second rule that must hold" (0.50 on the
// words "rule" and "hold"). So a soft overlap is accepted only when the two
// rules also share a heavy token — an identifier, a path fragment, a number —
// which boilerplate never does and a restated rule almost always does.
func similarInvariant(a, b string) bool {
	if similar(a, b, dupThreshold) {
		return true
	}
	if !similar(a, b, invSoftThreshold) {
		return false
	}
	return sharedHeavy(a, b)
}

// sharedHeavy reports whether two strings share a token that is unlikely to be
// sentence furniture: six or more letters, or anything with a digit (argv0,
// 256mib, sha256).
func sharedHeavy(a, b string) bool {
	ta, tb := tokens(a), tokens(b)
	for t := range ta {
		if !tb[t] {
			continue
		}
		if len(t) >= 6 || strings.IndexFunc(t, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0 {
			return true
		}
	}
	return false
}

// tokens reduces a string to the set of words that carry its meaning: lowercased,
// split on anything that is not a letter or digit, stopwords removed, and crudely
// singularised so "sessions" and "session" are one word. Punctuation-splitting
// matters more than it looks — the identifiers a record argues about
// (`cairn.promptBudget`, `state.vscdb`, `--follow`) are where two wordings of the
// same point actually agree.
func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r >= 'а' && r <= 'я' || r == 'ё')
	}) {
		if stopword[w] {
			continue
		}
		out[singular(w)] = true
	}
	return out
}

// sameNumbers reports whether both token sets quote the same numbers. A token
// like "sha256" or "v2" is a name, not a quantity, so only all-digit tokens count.
func sameNumbers(a, b map[string]bool) bool {
	na, nb := numbers(a), numbers(b)
	if len(na) != len(nb) {
		return false
	}
	for n := range na {
		if !nb[n] {
			return false
		}
	}
	return true
}

func numbers(t map[string]bool) map[string]bool {
	out := map[string]bool{}
	for w := range t {
		if strings.IndexFunc(w, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
			out[w] = true
		}
	}
	return out
}

func singular(w string) string {
	switch {
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		return w[:len(w)-3] + "y"
	case len(w) > 3 && strings.HasSuffix(w, "es") && !strings.HasSuffix(w, "ses"):
		return w[:len(w)-2]
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		return w[:len(w)-1]
	}
	return w
}

// stopword lists the words that appear in every sentence and so tell us nothing
// about whether two sentences are the same one.
//
// The modal verbs earn their place separately: every invariant ever written says
// "must", "never" or "always", so counting them as agreement made two unrelated
// rules look like one reworded rule — "first rule that must hold" against "second
// rule that must hold" scored 0.6 purely on boilerplate.
var stopword = map[string]bool{
	"must": true, "never": true, "always": true, "should": true, "shall": true,
	"may": true, "ought": true, "cannot": true, "ensure": true,
	"a": true, "an": true, "the": true, "and": true, "or": true, "but": true,
	"of": true, "to": true, "in": true, "on": true, "at": true, "by": true,
	"for": true, "with": true, "from": true, "as": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "been": true, "it": true, "its": true,
	"this": true, "that": true, "these": true, "those": true, "not": true,
	"no": true, "so": true, "than": true, "then": true, "would": true,
	"will": true, "can": true, "could": true, "do": true, "does": true,
	"did": true, "have": true, "has": true, "had": true, "we": true, "our": true,
	"they": true, "their": true, "which": true, "what": true, "when": true,
	"how": true, "why": true, "one": true, "all": true, "any": true, "each": true,
	"instead": true, "rather": true, "into": true, "over": true, "per": true,
}

// mergeWhy keeps the first statement of each distinct intention, within a budget,
// and reports what it left out.
//
// First, not last: the sessions arrive oldest-first, and the earliest statement of
// a purpose is the one closest to the request that prompted it. Later sessions
// restate it in terms of the work already done, which is drift.
//
// The budget is the part that actually fixes the complaint. A record is supposed
// to open with a few sentences on what was wanted; six sessions each contributing
// their own version is not six times as informative, it is the same paragraph six
// times, and it is the first thing a reader sees. Rejections and invariants have
// no equivalent cap — those are facts a later agent cannot re-derive from the
// diff, and dropping one loses it for good — but a seventh restatement of the
// same intention is not a fact, it is volume.
func mergeWhy(in []string) (kept []string, dropped int) {
	spent := 0
	for _, w := range in {
		if w = strings.TrimSpace(w); w == "" {
			continue
		}
		dup := false
		for _, k := range kept {
			if similar(w, k, whyDupThreshold) {
				dup = true
				break
			}
		}
		if dup {
			dropped++
			continue
		}
		if len(kept) >= maxMergedWhy || spent+len(w) > whyBudget {
			dropped++
			continue
		}
		spent += len(w)
		kept = append(kept, w)
	}
	return kept, dropped
}

// How much of a merged record may be spent on intention. One session's "why" is
// capped at maxWhy (600 bytes) by the prompt and the sanitiser; a commit made of
// many sessions gets room for a couple of genuinely different intentions and no
// more.
const (
	maxMergedWhy = 3
	whyBudget    = 900
)

// dedupRejected folds rewordings of the same rejected option together, keeping
// the entry whose reason says more — two sessions arguing the same option down
// rarely explain it equally well.
func dedupRejected(in []Rejected) []Rejected {
	var out []Rejected
	for _, r := range in {
		merged := false
		for i, kept := range out {
			if !similar(r.Option, kept.Option, dupThreshold) {
				continue
			}
			if len(r.Because) > len(kept.Because) {
				out[i] = r
			}
			merged = true
			break
		}
		if !merged {
			out = append(out, r)
		}
	}
	return out
}

// dedupInvariants folds rewordings of the same rule together, keeping the more
// tightly scoped one: a rule that names the paths it binds is served to the
// agents it applies to, while an unscoped copy of it is served to everyone.
func dedupInvariants(in []Invariant) []Invariant {
	var out []Invariant
	for _, c := range in {
		merged := false
		for i, kept := range out {
			if !similarInvariant(c.Rule, kept.Rule) {
				continue
			}
			switch {
			case len(kept.Scope) == 0 && len(c.Scope) > 0:
				out[i] = c
			case len(c.Scope) > 0 && len(kept.Scope) > 0 && len(c.Rule) > len(kept.Rule):
				// Prefer the clearer phrasing when both are scoped: a longer
				// rule usually names the constraint more completely.
				out[i] = c
			}
			merged = true
			break
		}
		if !merged {
			out = append(out, c)
		}
	}
	return out
}

// dedupStrings folds near-identical claims together. Verification costs one model
// call for the whole list, but a duplicated claim buys two verdicts on one fact
// and pushes a distinct claim past the cap.
func dedupStrings(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		dup := false
		for _, kept := range out {
			if similar(s, kept, dupThreshold) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, s)
		}
	}
	return out
}
