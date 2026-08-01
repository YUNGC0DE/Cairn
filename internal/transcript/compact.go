package transcript

import (
	"fmt"
	"strings"
)

// Budget defaults for compaction, in bytes of rendered prompt text. Payload
// size is the main lever on distillation latency: the agent CLIs cache their own
// ~25k-token preamble, so what we add is what we wait for.
const (
	DefaultBudget      = 24000
	maxAssistantText   = 2500
	maxThinking        = 1200
	maxToolLinesPerMsg = 12

	// The per-request cap slides between these two. Every request the human made
	// is rendered; when they do not all fit, each one is shortened instead — the
	// count is never reduced.
	maxHumanPrompt = 2000
	minHumanPrompt = 240

	// bodyReserve is the only part of the budget the requests may not take. The
	// rest — up to 75% — is theirs before a single one is shortened.
	//
	// Priority, in order: every request, then as much of each request as fits,
	// then the agent's reasoning, then its tool calls. A dropped request is a
	// dropped intention — the one thing a record exists to carry — while a
	// dropped `Edit foo.go` line costs nothing the diff does not already say.
	// The reserve exists because rejections are argued in the agent's reasoning,
	// so a body squeezed to nothing costs the record its second-best field.
	bodyReserve = 0.25
)

// Compaction is a session set rendered for a distillation prompt.
type Compaction struct {
	// Requests is every genuine human turn, verbatim and in order.
	Requests string
	// Body is the reasoning, visible output and tool calls, tail-first.
	Body string
	// Notes name anything that was sacrificed to the budget, so the caller can
	// say so rather than let the record look thinner than the session was.
	Notes []string
}

// CompactEach renders one payload per session, each with the full budget.
//
// The budget is per session because the record should not depend on when the
// human happened to commit. Two sessions committed one at a time produce two
// full records; committed together they must produce the same two, not one
// summary of both squeezed into a single session's worth of prompt. What scales
// with the number of sessions is the number of model calls (and their wall-clock
// budget) — not the size of each one's context.
func CompactEach(sessions []*Session, budget int) []Compaction {
	out := make([]Compaction, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, Compact([]*Session{s}, budget))
	}
	return out
}

// Compact renders sessions as two sections: what the human asked, and what the
// agent did about it.
//
// Splitting them is deliberate. Intent comes from the request; evidence comes
// from the body; a single stream lets the second bury the first — measured on a
// real six-session commit, 88 human turns went in and 9 lines came out.
//
// **No request is ever dropped.** Under budget pressure the requests are
// shortened, then the body is squeezed, and only the body is ever cut entirely.
func Compact(sessions []*Session, budget int) Compaction {
	if budget <= 0 {
		budget = DefaultBudget
	}
	reqs := collectRequests(sessions)
	// One byte for the newline that joins the two sections in the prompt.
	text, notes := renderRequests(reqs, budget-1)
	c := Compaction{Requests: text, Notes: notes}
	c.Body = compactBody(sessions, budget-len(c.Requests)-1)
	if c.Body == "" && len(reqs) > 0 {
		c.Notes = append(c.Notes,
			"the session body did not fit the prompt budget; only the human's requests were sent")
	}
	return c
}

// request is one human turn, with the session it came from.
type request struct {
	session string
	agent   string
	text    string
}

// collectRequests gathers every genuine human turn across all sessions, in the
// order they were said.
//
// Three things are removed, and only these three: text the harness wrote onto a
// user turn (`[Request interrupted by user]` and friends), the wrappers the
// harnesses inject around a prompt, and an exact repeat of something already
// said. Nothing else the human typed is filtered, ranked or judged relevant —
// deciding which of their requests mattered is the distillation's job, not the
// compactor's.
func collectRequests(sessions []*Session) []request {
	var reqs []request
	seen := map[string]bool{}
	for _, s := range sessions {
		for _, m := range s.Messages {
			if m.Role != RoleUser || m.Meta {
				continue
			}
			t := strip(m.Text)
			if t == "" || isHarnessArtifact(t) || seen[t] {
				continue
			}
			seen[t] = true
			reqs = append(reqs, request{short(s.ID), s.Agent, t})
		}
	}
	return reqs
}

// renderRequests writes every request, shortening them together until they fit.
//
// The count is never reduced — that is the whole point of this function. A
// request the human made and cairn never showed the model is an intention the
// record cannot carry, and no amount of tool-call detail makes up for it. What
// gives instead is length: the per-request cap slides down until the section
// fits, and only below minHumanPrompt does the section overshoot its share,
// which is reported rather than hidden.
func renderRequests(reqs []request, budget int) (string, []string) {
	if len(reqs) == 0 {
		return "", nil
	}
	// One long paste must not shorten everyone else: a single cap applied to all
	// of them keeps every short request whole and takes the length out of the
	// outliers, which is the fair reading of "keep as much of each as fits".
	allowance := budget - int(float64(budget)*bodyReserve)

	cap := maxHumanPrompt
	for {
		text, cut := renderAt(reqs, cap)
		switch {
		case len(text) <= allowance, cap <= minHumanPrompt:
			var notes []string
			if cut > 0 {
				notes = append(notes, fmt.Sprintf(
					"all %d human requests were sent; %d of them were longer than %d bytes and were "+
						"cut to that (raise cairn.promptBudget for more of each)", len(reqs), cut, cap))
			}
			if len(text) > budget {
				notes = append(notes, fmt.Sprintf(
					"the human's requests alone are %d bytes against a %d byte prompt budget; "+
						"they were kept and the session body dropped", len(text), budget))
			}
			return text, notes
		}
		if cap = cap * 3 / 4; cap < minHumanPrompt {
			cap = minHumanPrompt
		}
	}
}

// renderAt writes every request capped at cap bytes, reporting how many were
// actually long enough to be cut.
func renderAt(reqs []request, cap int) (string, int) {
	var out strings.Builder
	session, cut := "", 0
	for _, r := range reqs {
		if r.session != session {
			out.WriteString(sessionHeader(r.session, r.agent) + "\n")
			session = r.session
		}
		if len(r.text) > cap {
			cut++
		}
		out.WriteString(Truncate(r.text, cap))
		out.WriteByte('\n')
	}
	return strings.TrimRight(out.String(), "\n"), cut
}

// elisionReserve is the room left for the "[… N elided …]" marker, which is
// written after budgeting and would otherwise push a section over its ceiling.
const elisionReserve = 48

// bodyBlock is one renderable piece of the session body, with how much cairn
// wants it.
type bodyBlock struct {
	text string
	// filler marks a routine tool call — the first thing dropped. What it says
	// (`Edit internal/auth/limit.go`) the staged diff already says better, while
	// the reasoning beside it is the only place the *why* exists at all.
	filler bool
}

// compactBody renders what the agent thought and did, reasoning first.
//
// Measured before this split existed: on a four-session commit the body held
// 3.1 kB of `· Edit foo.go` lines and **zero bytes of reasoning**, while 68 kB of
// reasoning sat unread in the sessions. Keeping the tail by recency spends the
// budget on whatever the agent did most recently, and what an agent does most
// recently is call tools. So routine tool calls now fill what reasoning, visible
// text and tool *failures* have not taken.
func compactBody(sessions []*Session, budget int) string {
	budget -= elisionReserve
	if budget < 0 {
		budget = 0
	}
	parts := make([][]bodyBlock, len(sessions))
	for si, s := range sessions {
		var blocks []bodyBlock
		for _, m := range s.Messages {
			said, did := renderMessage(m)
			if said != "" {
				blocks = append(blocks, bodyBlock{text: said})
			}
			if did != "" {
				blocks = append(blocks, bodyBlock{text: did, filler: true})
			}
		}
		parts[si] = collapseRepeats(blocks)
	}

	keep := make([][]bool, len(parts))
	for i, p := range parts {
		keep[i] = make([]bool, len(p))
	}
	used := 0

	// Reasoning, one share per session, oldest first with the unspent remainder
	// rolling forward. A single tail across the whole set would spend everything
	// on the newest session — measured, that left three of four sessions
	// contributing a header and nothing else, and the body held zero reasoning
	// while 68 kB of it sat unread. Every session that argued something gets room
	// to say it; the newest still ends up with the most, because it inherits what
	// the others did not use.
	remaining := len(parts)
	for si, p := range parts {
		share := budget - used
		if remaining > 1 {
			share = (budget - used) / remaining
		}
		remaining--
		spent := 0
		for i := len(p) - 1; i >= 0; i-- {
			if p[i].filler {
				continue
			}
			n := len(p[i].text) + 1
			if spent+n > share {
				break
			}
			keep[si][i], spent = true, spent+n
		}
		used += spent
	}

	// Routine tool calls fill whatever is left, newest first.
	for si := len(parts) - 1; si >= 0; si-- {
		for i := len(parts[si]) - 1; i >= 0; i-- {
			if !parts[si][i].filler {
				continue
			}
			n := len(parts[si][i].text) + 1
			if used+n > budget {
				break
			}
			keep[si][i], used = true, used+n
		}
	}

	var out strings.Builder
	dropped := 0
	for si, p := range parts {
		kept := false
		for _, k := range keep[si] {
			if k {
				kept = true
				break
			}
		}
		if !kept {
			continue
		}
		s := sessions[si]
		hdr := fmt.Sprintf("=== session %s (%s)", short(s.ID), s.Agent)
		if s.Model != "" {
			hdr += " model=" + s.Model
		}
		out.WriteString(hdr + " ===\n")

		elided := 0
		flush := func() {
			if elided > 0 {
				fmt.Fprintf(&out, "[… %d earlier messages elided …]\n", elided)
				elided = 0
			}
		}
		for i, b := range p {
			if !keep[si][i] {
				if b.filler {
					dropped++
				} else {
					elided++
				}
				continue
			}
			flush()
			out.WriteString(b.text)
			out.WriteByte('\n')
		}
		flush()
	}
	if dropped > 0 {
		// Said once, at the end: the model should know the tool trail is partial,
		// but spending a line per gap on it would defeat the point of dropping it.
		fmt.Fprintf(&out, "[… %d routine tool calls not shown; see the staged diff …]\n", dropped)
	}
	return strings.TrimRight(out.String(), "\n")
}

// collapseRepeats folds a run of identical blocks into one with a count.
//
// An agent editing one file eight times produces eight identical lines, and at
// the tail of a session — which is exactly what the budget keeps — they crowd out
// the reasoning that explains any of it. The count says as much as the repetition
// did, in one line.
func collapseRepeats(blocks []bodyBlock) []bodyBlock {
	out := make([]bodyBlock, 0, len(blocks))
	for i := 0; i < len(blocks); {
		j := i + 1
		for j < len(blocks) && blocks[j] == blocks[i] {
			j++
		}
		b := blocks[i]
		if n := j - i; n > 1 {
			b.text = fmt.Sprintf("%s ×%d", b.text, n)
		}
		out = append(out, b)
		i = j
	}
	return out
}

// isHarnessArtifact recognises text that arrives on a user turn without a human
// having written it. Presented as a request it is worse than useless: it reads
// as intent and there is none in it.
func isHarnessArtifact(s string) bool {
	switch {
	case strings.HasPrefix(s, "[Request interrupted"),
		strings.HasPrefix(s, "[Tool "),
		strings.HasPrefix(s, "API Error"),
		strings.HasPrefix(s, "Caveat: The messages below"):
		return true
	}
	return false
}

// renderMessage splits one turn into what was said and what was done.
//
// A failed tool call counts as something said: a failure is why an agent changes
// course, which is the kind of thing a record is for. A successful one is a fact
// the diff already carries.
func renderMessage(m Message) (said, did string) {
	var s, d strings.Builder
	switch {
	case m.Role == RoleUser && !m.Meta:
		if t := strip(m.Text); t != "" {
			fmt.Fprintf(&s, "[human] %s\n", Truncate(t, maxHumanPrompt))
		}
	case m.Role == RoleUser && m.Meta:
		// Tool results and injected context: only failures carry signal.
	case m.Role == RoleAssistant:
		if m.Thinking != "" {
			fmt.Fprintf(&s, "[agent thinking] %s\n", Truncate(strip(m.Thinking), maxThinking))
		}
		if m.Text != "" {
			fmt.Fprintf(&s, "[agent] %s\n", Truncate(strip(m.Text), maxAssistantText))
		}
	}
	for i, t := range m.Tools {
		if i >= maxToolLinesPerMsg {
			fmt.Fprintf(&d, "  · … %d more tool calls\n", len(m.Tools)-i)
			break
		}
		if t.Error != "" {
			s.WriteString(renderTool(t))
			continue
		}
		d.WriteString(renderTool(t))
	}
	return strings.TrimRight(s.String(), "\n"), strings.TrimRight(d.String(), "\n")
}

func renderTool(t ToolCall) string {
	if t.Error != "" {
		return fmt.Sprintf("  · %s FAILED: %s\n", t.Name, Truncate(collapse(t.Error), 300))
	}
	switch {
	case len(t.Files) > 0:
		return fmt.Sprintf("  · %s %s\n", t.Name, strings.Join(t.Files, ", "))
	case t.Summary != "":
		return fmt.Sprintf("  · %s: %s\n", t.Name, Truncate(collapse(t.Summary), 200))
	default:
		return fmt.Sprintf("  · %s\n", t.Name)
	}
}

// strip removes wrapper noise the harnesses inject into user turns so it does
// not eat the budget or get mistaken for the human's intent.
func strip(s string) string {
	for _, tag := range []string{
		"system-reminder", "ide_selection", "ide_opened_file", "user_info",
		"environment_details", "local-command-stdout", "command-name",
		"command-message", "command-args",
		// Cursor prefixes every user turn with a rendered timestamp; it costs
		// budget and distracts the extraction from the actual request.
		"timestamp",
	} {
		s = removeTag(s, tag)
	}
	s = strings.ReplaceAll(s, "<user_query>", "")
	s = strings.ReplaceAll(s, "</user_query>", "")
	return strings.TrimSpace(s)
}

func removeTag(s, tag string) string {
	open, close := "<"+tag+">", "</"+tag+">"
	for {
		i := strings.Index(s, open)
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], close)
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j+len(close):]
	}
}

// Truncate cuts s to at most n bytes without splitting a UTF-8 rune.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isBoundary(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func isBoundary(b byte) bool { return b&0xC0 != 0x80 }

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func sessionHeader(id, agent string) string {
	return fmt.Sprintf("--- session %s (%s) ---", id, agent)
}
