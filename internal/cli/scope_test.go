package cli

import (
	"strings"
	"testing"
)

func TestGlobCovers(t *testing.T) {
	cases := []struct {
		pattern, target string
		want            bool
	}{
		{"internal/auth/**", "internal/auth/limit.go", true},
		{"internal/auth/**", "internal/auth/deep/nested.go", true},
		{"internal/auth/**", "internal/auth", true},
		{"internal/auth/**", "internal/authz/limit.go", false},
		{"internal/auth/**", "README.md", false},
		{"internal/cli/init.go", "internal/cli/init.go", true},
		{"internal/cli/init.go", "internal/cli/read.go", false},
		{"internal/cli", "internal/cli/read.go", true},
		{"README.md", "README.md", true},
		{"README.md", "docs/README.md", true}, // bare name matches the base name
		{"*.go", "internal/cli/read.go", true},
		{"*.go", "README.md", false},
		{"internal/**/limit.go", "internal/auth/limit.go", true},
		{"internal/**/limit.go", "other/auth/limit.go", false},
		{"**", "anything.go", true},
	}
	for _, c := range cases {
		if got := globCovers(c.pattern, c.target); got != c.want {
			t.Errorf("globCovers(%q, %q) = %v, want %v", c.pattern, c.target, got, c.want)
		}
	}
}

// The point of the whole file: a rule that names its paths reaches those paths
// and nobody else, while everything around it is passed through untouched.
func TestDropOutOfScopeKeepsOnlyRulesThatBind(t *testing.T) {
	msg := `Add rate limiting

<git-cairn>
why: Credential stuffing hit /login, and the author wanted it stopped without
  new infrastructure.

rejected: Redis-backed sliding window
  because: ADR-412 disallows new external datastores.

invariant: No new external datastores without an ADR
  scope: internal/auth/**

invariant: Hook failures must never block a commit
  scope: internal/cli/**

invariant: Transcript contents are never committed, only a sha256 pointer
</git-cairn>`

	auth := dropOutOfScope(msg, "internal/auth/limit.go")
	if !strings.Contains(auth, "No new external datastores") {
		t.Error("the rule that binds this path was dropped")
	}
	if strings.Contains(auth, "Hook failures") {
		t.Error("a rule scoped to another subsystem was served anyway")
	}
	// An unscoped rule binds everyone; that is what an empty scope means.
	if !strings.Contains(auth, "sha256 pointer") {
		t.Error("an unscoped rule must still be served")
	}
	// Everything that is not an invariant passes through verbatim.
	for _, keep := range []string{"Add rate limiting", "why: Credential stuffing",
		"rejected: Redis-backed", "because: ADR-412", openTag, closeTag} {
		if !strings.Contains(auth, keep) {
			t.Errorf("filtering touched something it should not: %q missing", keep)
		}
	}

	cli := dropOutOfScope(msg, "internal/cli/hook.go")
	if !strings.Contains(cli, "Hook failures") || strings.Contains(cli, "No new external datastores") {
		t.Errorf("wrong rules served for internal/cli/hook.go:\n%s", cli)
	}
}

// Legacy records carry the scope in trailing parentheses, and there are years of
// them in any repository that already uses cairn.
func TestDropOutOfScopeReadsLegacyScopes(t *testing.T) {
	msg := `Ship reactive recall

Invariant: Reactive hooks must never fail the agent tool call: on error, exit
quietly with no output. (internal/cli/**)
Invariant: SQLite stores over 256MiB are opened read-only in place
(internal/sqlitex/**)`

	got := dropOutOfScope(msg, "internal/cli/hook.go")
	if !strings.Contains(got, "never fail the agent tool call") {
		t.Errorf("legacy rule for this path was dropped:\n%s", got)
	}
	if strings.Contains(got, "256MiB") {
		t.Errorf("legacy rule for another subsystem was served:\n%s", got)
	}
	if !strings.Contains(got, "Ship reactive recall") {
		t.Error("the subject was lost")
	}
}

// A legacy invariant whose parentheses hold prose rather than paths must not be
// read as scoped — otherwise it would be dropped for everyone.
func TestLegacyProseParenthesesAreNotScopes(t *testing.T) {
	msg := "x\n\nInvariant: A hook failure must never block a commit (every error path\ndegrades the record and still exits 0)"
	if got := dropOutOfScope(msg, "internal/auth/limit.go"); !strings.Contains(got, "never block a commit") {
		t.Errorf("prose in parentheses was mistaken for a path scope:\n%s", got)
	}
}

// Dropping the last rule must not leave an empty block announcing reasoning that
// is not there.
func TestEmptiedBlockIsRemoved(t *testing.T) {
	msg := "Subject\n\n<git-cairn>\ninvariant: Only for auth\n  scope: internal/auth/**\n</git-cairn>"
	got := dropOutOfScope(msg, "README.md")
	if strings.Contains(got, openTag) || strings.Contains(got, closeTag) {
		t.Errorf("an emptied block was still served:\n%q", got)
	}
	if !strings.Contains(got, "Subject") {
		t.Errorf("the subject was lost:\n%q", got)
	}
}
