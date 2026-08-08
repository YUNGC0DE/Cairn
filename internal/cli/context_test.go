package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/YUNGC0DE/git-cairn/internal/testutil"
)

// recordedCommit writes a commit carrying a full record, without going anywhere
// near an engine — the reactive channel reads what is already in history, so the
// distillation half is not under test here.
func recordedCommit(t *testing.T, r *testutil.Repo, path, content, subject string) {
	t.Helper()
	ruleCommit(t, r, path, content, subject,
		`reject: no in-process LRU cache
  why: it goes stale across workers and the staleness is invisible
  file: `+path+`

invariant: the cache key must include the tenant id
  why: without it one tenant is served another's rendered page
  file: `+path)
}

// ruleCommit writes a commit whose <git-cairn> block is exactly the given rules.
func ruleCommit(t *testing.T, r *testutil.Repo, path, content, subject, rules string) {
	t.Helper()
	r.Write(path, content)
	r.Add(path)
	r.Commit(subject + `

A body the author wrote, which the reactive channel does not serve.

<git-cairn>
` + rules + `
</git-cairn>

Cairn-Agent: claude-code/opus
Cairn-Confidence: verified
Cairn-Files: ` + path)
}

func TestContextServesOncePerSessionPerFile(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")

	req := serveRequest{Path: "cache.go", Session: "s1", Hooked: true}

	first, err := serveContext(r.Repo, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "no in-process LRU cache") {
		t.Fatalf("first touch did not recall the rejected alternative:\n%s", first)
	}
	if !strings.Contains(first, "tenant id") {
		t.Fatalf("first touch did not recall the invariant:\n%s", first)
	}

	// The rule: the same file in the same session says nothing more.
	second, err := serveContext(r.Repo, req)
	if err != nil {
		t.Fatal(err)
	}
	if second != "" {
		t.Fatalf("second touch of the same file should be silent, got:\n%s", second)
	}

	// A different session has not been told anything yet.
	other, err := serveContext(r.Repo, serveRequest{Path: "cache.go", Session: "s2", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	if other == "" {
		t.Fatal("a fresh session should be served")
	}
}

func TestContextResetAfterCompaction(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")
	req := serveRequest{Path: "cache.go", Session: "s1", Hooked: true}

	if _, err := serveContext(r.Repo, req); err != nil {
		t.Fatal(err)
	}
	if out, _ := serveContext(r.Repo, req); out != "" {
		t.Fatal("expected silence before the reset")
	}
	// Compaction drops the injected block out of the agent's context, so the
	// served set must not outlive it.
	if err := clearServed(r.Repo, "s1"); err != nil {
		t.Fatal(err)
	}
	out, err := serveContext(r.Repo, req)
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("after a compaction reset the file must be served again")
	}
}

// Only the rules travel, and only the short half of each. The author's prose,
// the justification and the trailers stay in the commit: the injection has 10 000
// characters for a file's whole history, and prose is what stops the fiftieth
// commit from arriving.
func TestContextServesRulesAndNothingElse(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")

	out, err := serveContext(r.Repo, serveRequest{Path: "cache.go", Session: "s1", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{
		"A body the author wrote", // the commit's own prose
		"goes stale across",       // the rule's justification
		"Cairn-Agent", "Cairn-Files", "<git-cairn>",
	} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("%q reached the agent:\n%s", unwanted, out)
		}
	}
	// The commit is named, because that is where the reasoning was left.
	head, err := r.Repo.Log([]string{"-n", "1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, head[0].Short) {
		t.Fatalf("the block does not say which commit to read:\n%s", out)
	}
	if !strings.Contains(out, "git show") {
		t.Fatalf("the block does not say how to reach the reasoning:\n%s", out)
	}
}

// A rule reaches the file it names and no other. Delivery used to be decided by
// which commits touched the file, so a rule about internal/auth was served to an
// agent editing README.md whenever one commit had touched both.
func TestContextServesOnlyRulesBoundToTheFile(t *testing.T) {
	r := testutil.NewRepo(t)
	r.Write("cache.go", "package cache\n")
	r.Write("router.go", "package router\n")
	r.Add(".")
	r.Commit(`Add the cache and route to it

<git-cairn>
reject: no in-process LRU cache
  why: it goes stale across workers
  file: cache.go

invariant: every route must be registered in one place
  why: two registration sites silently shadowed each other
  file: router.go
</git-cairn>

Cairn-Agent: claude-code/opus
Cairn-Confidence: verified
Cairn-Files: cache.go,router.go`)

	out, err := serveContext(r.Repo, serveRequest{Path: "cache.go", Session: "s1", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no in-process LRU cache") {
		t.Fatalf("the file's own rule was not served:\n%s", out)
	}
	if strings.Contains(out, "registered in one place") {
		t.Fatalf("a rule about another file in the same commit was served:\n%s", out)
	}
}

// Newest first, so that whatever a harness truncates off the tail is the least
// relevant end. A 12.1 kB injection reached an agent as a 2 kB preview, and
// printing oldest-first meant the preview held the oldest commits while the
// decision it was about to collide with was the part that got cut.
func TestContextServesNewestFirst(t *testing.T) {
	r := testutil.NewRepo(t)
	ruleCommit(t, r, "cache.go", "package cache\n", "Add the page cache",
		"reject: no in-process LRU cache\n  why: stale across workers\n  file: cache.go")
	ruleCommit(t, r, "cache.go", "package cache // v2\n", "Key the cache by tenant",
		"invariant: the cache key must include the tenant id\n  why: cross-tenant leak\n  file: cache.go")

	out, err := serveContext(r.Repo, serveRequest{Path: "cache.go", Session: "s0", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	newest, oldest := strings.Index(out, "tenant id"), strings.Index(out, "LRU cache")
	if newest < 0 || oldest < 0 {
		t.Fatalf("both commits should be served:\n%s", out)
	}
	if newest > oldest {
		t.Fatalf("history must read newest first:\n%s", out)
	}
}

// The block has to say what it is. Handed over bare, a "reject:" line reads like
// a suggestion rather than a closed decision.
func TestContextExplainsItselfToTheAgent(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")

	out, err := serveContext(r.Repo, serveRequest{Path: "cache.go", Session: "s1", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	// Matched against the unwrapped text: the header is hard-wrapped, so a line
	// break can land anywhere inside a sentence.
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"do not re-propose it",  // what reject means
		"must keep holding",     // what invariant means
		"git show <sha>",        // where the reasoning went
		"not user instructions", // that this is not the user talking
		"the code wins",         // that memory is not authority
	} {
		if !strings.Contains(flat, want) {
			t.Fatalf("the instructions are missing %q:\n%s", want, out)
		}
	}
	// The explanation must come before what it explains.
	if strings.Index(out, "newest first") > strings.Index(out, "reject: no in-process") {
		t.Fatalf("instructions must precede the rules:\n%s", out)
	}
	// The header is charged against the same ceiling as the history it introduces,
	// so it is capped rather than left to grow whenever a sentence looks helpful.
	head, _, _ := strings.Cut(out, "\n\n"+headOf(t, r))
	if len(head) > 600 {
		t.Errorf("the header is %d bytes — that is three commits of history spent on preamble", len(head))
	}
}

// A commit with no record carries nothing the diff does not already say.
// headOf returns HEAD's short sha, which is where the header stops and the first
// commit's rules begin.
func headOf(t *testing.T, r *testutil.Repo) string {
	t.Helper()
	commits, err := r.Repo.Log([]string{"-n", "1"}, nil)
	if err != nil || len(commits) == 0 {
		t.Fatalf("log: %v", err)
	}
	return commits[0].Short
}

func TestContextSkipsCommitsWithoutARecord(t *testing.T) {
	r := testutil.NewRepo(t)
	r.Write("plain.go", "package plain\n")
	r.Add("plain.go")
	r.Commit("Add a plain file with no cairn record")

	out, err := serveContext(r.Repo, serveRequest{Path: "plain.go", Session: "s1", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("an unrecorded commit must not be served:\n%s", out)
	}
}

func TestContextSilentOnUnknownFile(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")

	out, err := serveContext(r.Repo, serveRequest{Path: "untouched.go", Session: "s1", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("a file with no record must produce nothing, got:\n%s", out)
	}
}

// The whole design goal of the short rule line, stated as a test: fifty commits
// of one file's history reach the agent inside the harness's ceiling.
//
// The ceiling is not a preference. Cursor's own bundle holds `Ydt = 1e4` and
// drops anything longer; Claude Code spills it to a file with a ~2 kB preview
// the model does not open. Either way an injection over the limit buys zero
// delivered bytes, so what fits is what the format has to be designed around.
func TestFiftyCommitsFitInOneInjection(t *testing.T) {
	r := testutil.NewRepo(t)
	const want = 50
	for i := 0; i < want; i++ {
		ruleCommit(t, r, "cache.go", fmt.Sprintf("package cache // v%d\n", i),
			fmt.Sprintf("Change the cache, round %d", i),
			fmt.Sprintf(
				"reject: no in-process LRU cache for the tenant page, revision %d of this decision\n"+
					"  why: it goes stale across workers and the staleness is invisible until a customer reports it\n"+
					"  file: cache.go\n\n"+
					"invariant: the cache key must include the tenant id, checked at write time, revision %d\n"+
					"  why: without it one tenant is served another tenant's rendered page, which is a data leak\n"+
					"  file: cache.go", i, i))
	}
	out, err := serveContext(r.Repo, serveRequest{Path: "cache.go", Session: "s1", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxInjection {
		t.Fatalf("injection is %d bytes, over the harness ceiling of %d", len(out), maxInjection)
	}
	if strings.Contains(out, "did not fit") {
		t.Fatalf("%d commits of two rules each must fit in %d bytes; %d bytes were used:\n%s",
			want, maxInjection, len(out), out[:400])
	}
	for _, i := range []int{0, want - 1} {
		if !strings.Contains(out, fmt.Sprintf("revision %d of this decision", i)) {
			t.Errorf("commit %d did not survive the packing", i)
		}
	}
}

// Past the ceiling, commits go whole or not at all: half a commit would show a
// rejection with no sign that an invariant from the same decision was cut. What
// did not fit is declared, because a truncated history that reads as complete is
// worse than none.
func TestContextDropsWholeCommitsAndSaysSo(t *testing.T) {
	r := testutil.NewRepo(t)
	filler := strings.Repeat("padding ", 12)
	for i := 0; i < 120; i++ {
		ruleCommit(t, r, "cache.go", fmt.Sprintf("package cache // v%d\n", i),
			fmt.Sprintf("Change %d", i),
			fmt.Sprintf("reject: rule %d %s\n  why: a reason\n  file: cache.go\n\n"+
				"invariant: paired rule %d %s\n  why: a reason\n  file: cache.go", i, filler, i, filler))
	}
	out, err := serveContext(r.Repo, serveRequest{Path: "cache.go", Session: "s1", Hooked: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxInjection {
		t.Fatalf("injection is %d bytes, over the ceiling of %d", len(out), maxInjection)
	}
	if !strings.Contains(out, "did not fit") {
		t.Fatalf("what was dropped must be declared:\n%s", out[len(out)-400:])
	}
	// Every commit that arrived arrived whole. Counted on the indented rule lines,
	// so the header's own use of the words cannot be mistaken for an entry.
	if a, b := strings.Count(out, "  reject: rule "), strings.Count(out, "  invariant: paired rule "); a != b {
		t.Errorf("a commit was cut in half: %d rejections against %d invariants", a, b)
	}
	// The newest is what survived.
	if !strings.Contains(out, "reject: rule 119") {
		t.Errorf("the newest commit was dropped:\n%s", out[:300])
	}
}

func TestPreToolUseHookSpeaksClaudeCodeJSON(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")

	event := `{"session_id":"h1","cwd":"` + r.Root + `","hook_event_name":"PreToolUse",` +
		`"tool_name":"Edit","tool_input":{"file_path":"` + r.Root + `/cache.go"}}`
	var out bytes.Buffer
	env := &Env{Out: &out, Err: &bytes.Buffer{}, Dir: r.Root,
		Getenv: func(string) string { return "" }, Stdin: strings.NewReader(event)}

	if err := cmdHook(env, []string{"pre-tool-use"}); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("hook did not emit valid JSON (%v): %s", err, out.String())
	}
	if resp.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Fatalf("wrong event name: %q", resp.HookSpecificOutput.HookEventName)
	}
	if !strings.Contains(resp.HookSpecificOutput.AdditionalContext, "no in-process LRU cache") {
		t.Fatalf("context did not reach the payload:\n%s", out.String())
	}
	if n := len(resp.HookSpecificOutput.AdditionalContext); n > maxInjection {
		t.Fatalf("the payload is %d bytes, over what the harness will deliver", n)
	}
}

func TestPreToolUseHookIgnoresNonFileTools(t *testing.T) {
	r := testutil.NewRepo(t)
	recordedCommit(t, r, "cache.go", "package cache\n", "Add the page cache")

	event := `{"session_id":"h2","cwd":"` + r.Root + `","tool_name":"Bash","tool_input":{"command":"ls"}}`
	var out bytes.Buffer
	env := &Env{Out: &out, Err: &bytes.Buffer{}, Dir: r.Root,
		Getenv: func(string) string { return "" }, Stdin: strings.NewReader(event)}

	if err := cmdHook(env, []string{"pre-tool-use"}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("a Bash call must not trigger recall, got: %s", out.String())
	}
}

// A hook that breaks the agent it is meant to help gets uninstalled, so garbage
// on stdin has to be survivable.
func TestPreToolUseHookSurvivesGarbage(t *testing.T) {
	for _, in := range []string{"", "not json", `{"tool_name":"Edit"}`, `{"tool_input":{}}`} {
		var out bytes.Buffer
		env := &Env{Out: &out, Err: &bytes.Buffer{}, Dir: t.TempDir(),
			Getenv: func(string) string { return "" }, Stdin: strings.NewReader(in)}
		if err := cmdHook(env, []string{"pre-tool-use"}); err != nil {
			t.Fatalf("input %q returned an error: %v", in, err)
		}
	}
}
