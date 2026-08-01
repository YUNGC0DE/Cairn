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
	c := Compact([]*Session{s}, 0)
	requests, body := c.Requests, c.Body
	got := requests + "\n" + body

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
	// The human's own words are their own section: what the record must explain
	// cannot be left to compete with tool calls for space.
	if !strings.Contains(requests, "add rate limiting to /login") {
		t.Errorf("the request must appear in the requests section:\n%s", requests)
	}
	// Tool results are bulk without signal; only their failures matter.
	if strings.Contains(got, "noise that should not appear") {
		t.Errorf("tool-result content must not be included:\n%s", got)
	}
}

// TestCompactKeepsEveryRequestWhenToolCallsFlood covers the failure this split
// exists for: a real six-session commit whose record read like a changelog,
// because 88 human turns lost the budget to `Edit foo.go` repeated hundreds of
// times and only 9 survived — 4 of them harness noise.
func TestCompactKeepsEveryRequestWhenToolCallsFlood(t *testing.T) {
	var msgs []Message
	msgs = append(msgs, Message{Role: RoleUser, Text: "add rate limiting, ADR-412 forbids new datastores"})
	for i := 0; i < 400; i++ {
		msgs = append(msgs, Message{Role: RoleAssistant,
			Tools: []ToolCall{{Name: "Edit", Files: []string{"internal/auth/limit.go"}}}})
	}
	msgs = append(msgs, Message{Role: RoleUser, Text: "now cover /register too"})
	for i := 0; i < 400; i++ {
		msgs = append(msgs, Message{Role: RoleAssistant,
			Tools: []ToolCall{{Name: "Bash", Summary: "go test ./..."}}})
	}

	c := Compact([]*Session{session(msgs...)}, 4000)
	requests, body := c.Requests, c.Body

	for _, want := range []string{"add rate limiting", "now cover /register too"} {
		if !strings.Contains(requests, want) {
			t.Errorf("request %q was crowded out by tool calls:\n%s", want, requests)
		}
	}
	// Repetition says nothing the count does not.
	if !strings.Contains(body, "×") {
		t.Errorf("a run of identical tool calls must collapse into a count:\n%s", body)
	}
	if n := strings.Count(body, "Edit internal/auth/limit.go"); n > 1 {
		t.Errorf("the same tool line appears %d times; it should be collapsed", n)
	}
}

// TestEveryRequestSurvivesAnyBudget is the rule the compactor is built around:
// a request the human made and cairn never showed the model is an intention the
// record cannot carry. Length gives; the count never does.
func TestEveryRequestSurvivesAnyBudget(t *testing.T) {
	var msgs []Message
	for i := 0; i < 120; i++ {
		msgs = append(msgs,
			Message{Role: RoleUser, Text: fmt.Sprintf("REQUEST-%03d %s", i, strings.Repeat("detail ", 300))},
			Message{Role: RoleAssistant, Text: strings.Repeat("agent chatter ", 200),
				Tools: []ToolCall{{Name: "Edit", Files: []string{fmt.Sprintf("file%d.go", i)}}}})
	}

	// A budget far too small for 120 requests at full length.
	c := Compact([]*Session{session(msgs...)}, 8000)

	for i := 0; i < 120; i++ {
		if !strings.Contains(c.Requests, fmt.Sprintf("REQUEST-%03d", i)) {
			t.Fatalf("request %d was dropped; every one must survive:\n%s", i, transcriptHead(c.Requests))
		}
	}
	// Shortening is a degradation and must be reported, not hidden.
	if len(c.Notes) == 0 {
		t.Error("shortening every request to fit must be stated in the notes")
	}
	// The body is what gives way, and that is also said out loud.
	if len(c.Body) > len(c.Requests) {
		t.Errorf("the body took %d bytes against %d of requests; requests come first",
			len(c.Body), len(c.Requests))
	}
}

func transcriptHead(s string) string { return Truncate(s, 300) }

// TestRelevantPicksTheSessionsThatWroteTheCommit covers the
// hundred-open-conversations case, and the reason the test is the staged file
// list rather than a guess: reading a file names it, and running a command
// changes nothing that lands in the commit.
func TestRelevantPicksTheSessionsThatWroteTheCommit(t *testing.T) {
	staged := []string{"internal/auth/limit.go"}

	chat := &Session{Ref: Ref{ID: "chat0001"}, Messages: []Message{
		{Role: RoleUser, Text: "what does this repository even do?"},
		{Role: RoleAssistant, Tools: []ToolCall{
			// Read the very file being committed — still not the session that wrote it.
			{Name: "read_file_v2", Files: []string{"/repo/internal/auth/limit.go"}},
			{Name: "ripgrep_raw_search", Summary: "Cairn-Agent"},
		}},
	}}
	terminal := &Session{Ref: Ref{ID: "term0001"}, Messages: []Message{
		{Role: RoleUser, Text: "run the tests"},
		{Role: RoleAssistant, Tools: []ToolCall{{Name: "Bash", Summary: "go test ./..."}}},
	}}
	elsewhere := &Session{Ref: Ref{ID: "else0001"}, Messages: []Message{
		{Role: RoleUser, Text: "fix the readme"},
		{Role: RoleAssistant, Tools: []ToolCall{{Name: "Edit", Files: []string{"/repo/README.md"}}}},
	}}
	work := &Session{Ref: Ref{ID: "work0001"}, Messages: []Message{
		{Role: RoleUser, Text: "add rate limiting"},
		{Role: RoleAssistant, Tools: []ToolCall{{Name: "Edit", Files: []string{"/repo/internal/auth/limit.go"}}}},
	}}

	kept, skipped := Relevant([]*Session{chat, terminal, elsewhere, work}, staged)
	if len(kept) != 1 || kept[0].ID != "work0001" {
		var ids []string
		for _, s := range kept {
			ids = append(ids, s.ID)
		}
		t.Fatalf("kept %v, want only the session that wrote a staged file", ids)
	}
	if skipped != 3 {
		t.Errorf("skipped = %d, want 3", skipped)
	}

	// Nothing staged: there is nothing to compare against, so nothing is judged.
	if kept, skipped = Relevant([]*Session{chat, terminal}, nil); len(kept) != 2 || skipped != 0 {
		t.Errorf("with no staged files everything must be kept, got %d kept %d skipped", len(kept), skipped)
	}
	// No session wrote a staged file — the human may have edited by hand, or
	// through a shell. A record from a conversation beats no record.
	if kept, skipped = Relevant([]*Session{chat, terminal}, staged); len(kept) != 2 || skipped != 0 {
		t.Errorf("with no writer among them everything must be kept, got %d kept %d skipped", len(kept), skipped)
	}
}

func TestStagedPathMatching(t *testing.T) {
	staged := []string{"internal/auth/limit.go", "cmd/cairn/main.go"}
	cases := map[string]bool{
		"/Users/you/repo/internal/auth/limit.go":     true,
		"internal/auth/limit.go":                     true,
		"./internal/auth/limit.go":                   true,
		"/Users/you/repo/internal/auth/limit.go.bak": false,
		"/other/limit.go":                            false,
		"internal/auth/limiter.go":                   false,
	}
	for path, want := range cases {
		if got := isStaged(path, staged); got != want {
			t.Errorf("isStaged(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestCompactDropsHarnessArtifactsFromRequests(t *testing.T) {
	s := session(
		Message{Role: RoleUser, Text: "[Request interrupted by user]"},
		Message{Role: RoleUser, Text: "<ide_opened_file>ROADMAP.md is open</ide_opened_file>\nadd the parser"},
		Message{Role: RoleUser, Text: "add the parser"}, // the same words again
	)
	requests := Compact([]*Session{s}, 0).Requests

	if strings.Contains(requests, "Request interrupted") {
		t.Errorf("a harness artifact is not a request:\n%s", requests)
	}
	if strings.Contains(requests, "ROADMAP.md is open") {
		t.Errorf("injected editor state is not a request:\n%s", requests)
	}
	if n := strings.Count(requests, "add the parser"); n != 1 {
		t.Errorf("a repeated request should appear once, appeared %d times:\n%s", n, requests)
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
	c := Compact([]*Session{s}, 0)
	requests, body := c.Requests, c.Body
	got := requests + "\n" + body
	if !strings.Contains(requests, "the actual request") {
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
	c := Compact([]*Session{session(msgs...)}, budget)
	requests, body := c.Requests, c.Body
	got := requests + "\n" + body

	// The whole payload, both sections together, must fit the budget: it is a
	// latency ceiling, not a suggestion.
	if len(got) > budget {
		t.Errorf("compaction produced %d bytes for a %d budget", len(got), budget)
	}
	// The tail is closest to the commit, so it must survive.
	if !strings.Contains(body, "LAST MESSAGE before the commit") {
		t.Error("the most recent message must be kept")
	}
	// The human's request survives regardless of how much the agent said after it.
	if !strings.Contains(requests, "FIRST PROMPT anchors the intent") {
		t.Error("the request must be kept whatever the session did afterwards")
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
