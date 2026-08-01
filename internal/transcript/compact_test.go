package transcript

import (
	"fmt"
	"strings"
	"testing"
)

func session(msgs ...Message) *Session {
	return &Session{Ref: Ref{Agent: "claude-code", ID: "abcd1234"}, Model: "claude-opus-5", Messages: msgs}
}

func TestCompactKeepsWhatMatters(t *testing.T) {
	s := session(
		Message{Role: RoleUser, Text: "add rate limiting to /login"},
		Message{Role: RoleAssistant, Thinking: "redis needs a datastore", Text: "using a token bucket",
			Tools: []ToolCall{{Name: "Edit", Files: []string{"internal/auth/limit.go"}}}},
		Message{Role: RoleUser, Meta: true, Text: "tool result noise that should not appear"},
		Message{Role: RoleAssistant, Tools: []ToolCall{{Name: "Bash", Error: "exit 1: tests failed"}}},
	)
	got := Compact([]*Session{s}, 0)

	for _, want := range []string{
		"add rate limiting to /login",
		"redis needs a datastore",
		"using a token bucket",
		"Edit internal/auth/limit.go",
		"Bash FAILED",
		"claude-opus-5",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("compaction dropped %q:\n%s", want, got)
		}
	}
	// Tool results are bulk without signal; only their failures matter.
	if strings.Contains(got, "noise that should not appear") {
		t.Errorf("tool-result content must not be included:\n%s", got)
	}
}

func TestCompactStripsHarnessWrappers(t *testing.T) {
	s := session(Message{Role: RoleUser, Text: strings.Join([]string{
		"<system-reminder>ignore me</system-reminder>",
		"<timestamp>Friday, Jul 24, 2026</timestamp>",
		"<user_query>",
		"the actual request",
		"</user_query>",
	}, "\n")})
	got := Compact([]*Session{s}, 0)
	if !strings.Contains(got, "the actual request") {
		t.Errorf("the request was lost:\n%s", got)
	}
	for _, junk := range []string{"ignore me", "Friday", "<user_query>"} {
		if strings.Contains(got, junk) {
			t.Errorf("wrapper %q survived compaction:\n%s", junk, got)
		}
	}
}

func TestCompactRespectsBudgetAndKeepsTheTail(t *testing.T) {
	var msgs []Message
	msgs = append(msgs, Message{Role: RoleUser, Text: "FIRST PROMPT anchors the intent"})
	for i := 0; i < 200; i++ {
		msgs = append(msgs, Message{Role: RoleAssistant,
			Text: fmt.Sprintf("filler message %d %s", i, strings.Repeat("x", 200))})
	}
	msgs = append(msgs, Message{Role: RoleAssistant, Text: "LAST MESSAGE before the commit"})

	const budget = 4000
	got := Compact([]*Session{session(msgs...)}, budget)

	if len(got) > budget*2 {
		t.Errorf("compaction produced %d bytes for a %d budget", len(got), budget)
	}
	// The tail is closest to the commit, so it must survive.
	if !strings.Contains(got, "LAST MESSAGE before the commit") {
		t.Error("the most recent message must be kept")
	}
	// The first human prompt is the intent anchor and is kept regardless.
	if !strings.Contains(got, "FIRST PROMPT anchors the intent") {
		t.Error("the first human prompt must be kept as the intent anchor")
	}
	// Elision must be visible, so a reader knows the middle is missing.
	if !strings.Contains(got, "earlier messages elided") {
		t.Errorf("elision must be announced:\n%s", got[:min(400, len(got))])
	}
}

func TestTruncateDoesNotSplitRunes(t *testing.T) {
	s := "инвариант" // multi-byte throughout
	for n := 1; n < len(s); n++ {
		got := Truncate(s, n)
		trimmed := strings.TrimSuffix(got, "…")
		for _, r := range trimmed {
			if r == '\uFFFD' {
				t.Fatalf("Truncate(%q, %d) = %q broke a rune", s, n, got)
			}
		}
	}
}

func TestPathsIn(t *testing.T) {
	cases := []struct {
		cwd, root string
		want      bool
	}{
		{"/repo", "/repo", true},
		{"/repo/internal/auth", "/repo", true},
		{"/repo-other", "/repo", false},
		{"/other", "/repo", false},
		{"", "/repo", false},
		{"/repo/", "/repo", true},
	}
	for _, c := range cases {
		if got := PathsIn(c.cwd, c.root); got != c.want {
			t.Errorf("PathsIn(%q, %q) = %v, want %v", c.cwd, c.root, got, c.want)
		}
	}
}
