package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/YUNGC0DE/git-cairn/internal/capture"
	"github.com/YUNGC0DE/git-cairn/internal/config"
	"github.com/YUNGC0DE/git-cairn/internal/gitx"
	"github.com/YUNGC0DE/git-cairn/internal/llm"
	"github.com/YUNGC0DE/git-cairn/internal/sqlitex"
	"github.com/YUNGC0DE/git-cairn/internal/transcript/claudecode"
	"github.com/YUNGC0DE/git-cairn/internal/transcript/cursorcli"
	"github.com/YUNGC0DE/git-cairn/internal/transcript/cursoride"
)

func cmdDoctor(env *Env, args []string) error {
	fs := flags("doctor", prog+" doctor", env.Out)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ok, warn := "✓", "!"
	line := func(status, what, detail string) {
		if detail == "" {
			fmt.Fprintf(env.Out, "%s %s\n", status, what)
			return
		}
		fmt.Fprintf(env.Out, "%s %-28s %s\n", status, what, detail)
	}

	if v, err := gitx.Run(env.Dir, "--version"); err == nil {
		line(ok, "git", v)
	} else {
		line(warn, "git", "not found — cairn cannot work without it")
	}

	repo, repoErr := gitx.Open(env.Dir)
	if repoErr != nil {
		line(warn, "repository", "not inside a git repository")
	} else {
		line(ok, "repository", repo.Root)
		for _, h := range hookNames {
			if installedHook(repo, h) {
				line(ok, "hook "+h, "installed")
			} else {
				line(warn, "hook "+h, "missing — run `"+prog+" init`")
			}
		}
	}

	cfg := config.Load(repo, env.Getenv)
	line(ok, "mode", string(cfg.Mode))
	line(ok, "time budget", cfg.Budget.String()+" / session")
	if !cfg.Enabled {
		line(warn, "enabled", "cairn is switched off for this repository")
	}

	// Engines. Each installed one is *called*, because being on PATH says nothing
	// about being logged in or in quota — and that is the failure that silently
	// turns every commit into metadata-only.
	engineFound := false
	for _, e := range llm.Engines() {
		if !e.Available() {
			line(warn, "engine "+e.Name(), "not installed")
			continue
		}
		engineFound = true
		status, detail := probeEngine(e, cfg)
		line(status, "engine "+e.Name(), detail)
	}
	switch {
	case !engineFound:
		line(warn, "distillation", "no engine — records will be trailers only")
	case cfg.Engine != "" && cfg.Engine != "auto":
		line(ok, "distillation order", cfg.Engine+" only (pinned by cairn.engine — no fallback)")
	default:
		if e, err := llm.Pick("auto"); err == nil {
			line(ok, "distillation order", e.Name()+" (next one runs if the first fails outright)")
		}
	}

	if sqlitex.Available() {
		line(ok, "sqlite3", pathOf("sqlite3"))
	} else {
		line(warn, "sqlite3", "missing — Cursor sessions degrade to prompts only")
	}

	// Transcript sources.
	if d := claudecode.DefaultRoot(); dirExists(d) {
		line(ok, "claude-code transcripts", d)
	} else {
		line(warn, "claude-code transcripts", d+" (absent)")
	}
	if d := cursorcli.DefaultRoot(); dirExists(d) {
		line(ok, "cursor-cli transcripts", d)
	} else {
		line(warn, "cursor-cli transcripts", d+" (absent)")
	}
	if db := cursoride.New().GlobalDB(); fileExists(db) {
		line(ok, "cursor-ide transcripts", db)
	} else {
		line(warn, "cursor-ide transcripts", db+" (absent)")
	}

	if repoErr == nil {
		refs, errs := capture.Discover(repo.Root, time.Time{})
		line(ok, "sessions for this repo", plural(len(refs), "session", "sessions"))
		for _, e := range errs {
			line(warn, "discovery", e.Error())
		}
		off, err := capture.LoadOffsets(repo.CairnDir())
		if err == nil {
			line(ok, "offsets", fmt.Sprintf("%s tracked in %s",
				plural(len(off.Cursors), "session", "sessions"), rel(repo.Root, repo.CairnDir())))
		}
		logPath := filepath.Join(repo.CairnDir(), runLogName)
		if fileExists(logPath) {
			line(ok, "run log", rel(repo.Root, logPath)+" — what cairn did on each commit")
		} else {
			line(ok, "run log", rel(repo.Root, logPath)+" (written from the first agent commit)")
		}
	}
	return nil
}

// probePrompt is the smallest request that exercises everything a commit needs:
// the process spawns, the CLI is logged in and in quota, the flags are accepted,
// the model id resolves, and the answer survives ExtractJSON — which is how every
// real distillation reads a reply.
const probePrompt = `Reply with only this JSON object and nothing else: {"ok": true}`

// probeEngine calls one engine and describes what came back.
//
// One call, on the extraction pass: that is the model a commit spends its time
// on, and a second call would double the cost of running `doctor` to re-test the
// same login.
func probeEngine(e llm.Engine, cfg config.Config) (status, detail string) {
	start := time.Now()
	resp, err := e.Complete(context.Background(), llm.Request{
		Prompt: probePrompt,
		Model:  cfg.Model,
		Effort: cfg.Effort,
		Budget: cfg.Budget,
	})
	took := time.Since(start).Round(100 * time.Millisecond)
	switch {
	case err != nil:
		return "✗", fmt.Sprintf("installed, but failed after %s — %s", took, trimTo(collapseSpace(err.Error()), 110))
	case !answersWithJSON(resp.Text):
		// The call worked and the answer is unusable, which is how a record ends
		// up metadata-only with a healthy-looking engine.
		return "✗", fmt.Sprintf("answered in %s but not with JSON cairn can read — %s",
			took, trimTo(collapseSpace(resp.Text), 70))
	default:
		return "✓", fmt.Sprintf("%s · answered in %s", orDefault(resp.Model, "engine default"), took)
	}
}

func answersWithJSON(text string) bool {
	var reply struct {
		OK bool `json:"ok"`
	}
	return llm.ExtractJSON(text, &reply) == nil
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func cmdSessions(env *Env, args []string) error {
	fs := flags("sessions", prog+" sessions [--all]", env.Out)
	all := fs.Bool("all", false, "list sessions regardless of the last commit time")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repo, err := openRepo(env)
	if err != nil {
		return err
	}
	since := time.Time{}
	if !*all {
		since, _ = repo.HeadTime()
	}
	refs, errs := capture.Discover(repo.Root, since)
	for _, e := range errs {
		fmt.Fprintf(env.Err, "cairn: %v\n", e)
	}
	if len(refs) == 0 {
		fmt.Fprintln(env.Out, "No agent sessions found for this repository.")
		if !*all {
			fmt.Fprintln(env.Out, "(only sessions touched since the last commit are listed; use --all)")
		}
		return nil
	}
	offsets, _ := capture.LoadOffsets(repo.CairnDir())
	for _, r := range refs {
		c := offsets.Get(r.Key)
		state := "new"
		if c.Bytes > 0 || c.Count > 0 {
			state = "read to " + c.String()
		}
		fmt.Fprintf(env.Out, "%-12s %-10s %s  %s\n", r.Agent, short(r.ID),
			r.Updated.Local().Format("2006-01-02 15:04"), state)
		if r.Title != "" {
			fmt.Fprintf(env.Out, "             %s\n", r.Title)
		}
	}
	return nil
}

func pathOf(bin string) string {
	if p, err := exec.LookPath(bin); err == nil {
		return p
	}
	return ""
}

func dirExists(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func trimTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}
