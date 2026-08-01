package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/YUNGC0DE/Cairn/internal/capture"
	"github.com/YUNGC0DE/Cairn/internal/config"
	"github.com/YUNGC0DE/Cairn/internal/gitx"
	"github.com/YUNGC0DE/Cairn/internal/llm"
	"github.com/YUNGC0DE/Cairn/internal/sqlitex"
	"github.com/YUNGC0DE/Cairn/internal/transcript/claudecode"
	"github.com/YUNGC0DE/Cairn/internal/transcript/cursorcli"
)

func cmdDoctor(env *Env, args []string) error {
	fs := flags("doctor", "cairn doctor", env.Out)
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
				line(warn, "hook "+h, "missing — run `cairn init`")
			}
		}
	}

	cfg := config.Load(repo, env.Getenv)
	line(ok, "mode", string(cfg.Mode))
	line(ok, "time budget", cfg.Budget.String())
	if !cfg.Enabled {
		line(warn, "enabled", "cairn is switched off for this repository")
	}

	// Engines. Without one, cairn still records trailers, so this is a warning.
	engineFound := false
	for _, e := range llm.Engines() {
		if e.Available() {
			engineFound = true
			line(ok, "engine "+e.Name(), e.Path())
		} else {
			line(warn, "engine "+e.Name(), "not on PATH")
		}
	}
	if !engineFound {
		line(warn, "distillation", "no engine — records will be trailers only")
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
	}
	return nil
}

func cmdSessions(env *Env, args []string) error {
	fs := flags("sessions", "cairn sessions [--all]", env.Out)
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

func trimTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}
