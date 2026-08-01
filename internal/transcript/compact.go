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
	maxHumanPrompt     = 3000
	maxAssistantText   = 2500
	maxThinking        = 1200
	maxToolLinesPerMsg = 12
)

// Compact renders sessions as plain text for a distillation prompt, keeping the
// tail (most recent, most relevant to the commit) and the first human prompt
// (which anchors intent), eliding the middle when over budget.
func Compact(sessions []*Session, budget int) string {
	if budget <= 0 {
		budget = DefaultBudget
	}
	type block struct {
		text   string
		anchor bool // never elided
	}
	var blocks []block
	for _, s := range sessions {
		hdr := fmt.Sprintf("=== session %s (%s)", short(s.ID), s.Agent)
		if s.Model != "" {
			hdr += " model=" + s.Model
		}
		blocks = append(blocks, block{text: hdr + " ==="})
		firstHuman := true
		for _, m := range s.Messages {
			t := renderMessage(m)
			if t == "" {
				continue
			}
			anchor := false
			if m.Role == RoleUser && !m.Meta && firstHuman {
				anchor, firstHuman = true, false
			}
			blocks = append(blocks, block{text: t, anchor: anchor})
		}
	}

	// Walk backwards, keeping the tail; anchors are kept regardless.
	keep := make([]bool, len(blocks))
	used := 0
	for i := len(blocks) - 1; i >= 0; i-- {
		n := len(blocks[i].text) + 1
		if used+n <= budget {
			keep[i], used = true, used+n
		}
	}
	for i, b := range blocks {
		if b.anchor && !keep[i] && used+len(b.text)+1 <= budget*2 {
			keep[i], used = true, used+len(b.text)+1
		}
	}

	var out strings.Builder
	elided := 0
	flush := func() {
		if elided > 0 {
			fmt.Fprintf(&out, "[… %d earlier messages elided …]\n", elided)
			elided = 0
		}
	}
	for i, b := range blocks {
		if !keep[i] {
			elided++
			continue
		}
		flush()
		out.WriteString(b.text)
		out.WriteByte('\n')
	}
	flush()
	return strings.TrimRight(out.String(), "\n")
}

func renderMessage(m Message) string {
	var b strings.Builder
	switch {
	case m.Role == RoleUser && !m.Meta:
		if s := strip(m.Text); s != "" {
			fmt.Fprintf(&b, "[human] %s\n", Truncate(s, maxHumanPrompt))
		}
	case m.Role == RoleUser && m.Meta:
		// Tool results and injected context: only failures carry signal.
	case m.Role == RoleAssistant:
		if m.Thinking != "" {
			fmt.Fprintf(&b, "[agent thinking] %s\n", Truncate(strip(m.Thinking), maxThinking))
		}
		if m.Text != "" {
			fmt.Fprintf(&b, "[agent] %s\n", Truncate(strip(m.Text), maxAssistantText))
		}
	}
	for i, t := range m.Tools {
		if i >= maxToolLinesPerMsg {
			fmt.Fprintf(&b, "  · … %d more tool calls\n", len(m.Tools)-i)
			break
		}
		b.WriteString(renderTool(t))
	}
	return strings.TrimRight(b.String(), "\n")
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
		"system-reminder", "ide_selection", "user_info", "environment_details",
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
