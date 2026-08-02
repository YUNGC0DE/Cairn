package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/YUNGC0DE/Cairn/internal/config"
	"github.com/YUNGC0DE/Cairn/internal/gitx"
)

// marker identifies a hook cairn wrote, so re-running init upgrades its own hook
// and never clobbers somebody else's.
const marker = "# cairn:managed-hook"

// hookNames are the hooks cairn installs.
//
// prepare-commit-msg does the work. post-commit exists for two reasons: it is
// the only place a note can be attached (the commit does not exist yet when
// prepare-commit-msg runs), and it is where read offsets are committed — so an
// aborted commit does not silently consume a transcript.
var hookNames = []string{"prepare-commit-msg", "post-commit"}

func hookScript(bin, hook string) string {
	return fmt.Sprintf(`#!/bin/sh
%s
# Records why this commit exists. Cairn never fails or blocks a commit:
# every failure path below exits 0.
#
# Escape hatches:  CAIRN_SKIP=1 git commit …    (skip once)
#                  git config cairn.enabled false  (skip always)

CAIRN=%q
[ -x "$CAIRN" ] || CAIRN=$(command -v cairn 2>/dev/null)
[ -n "$CAIRN" ] || exit 0

"$CAIRN" hook %s "$@" || true
exit 0
`, marker, bin, hook)
}

func cmdInit(env *Env, args []string) error {
	fs := flags("init", "cairn init [--force] [--mode message|notes] [--agent claude-code|cursor|all|none]", env.Out)
	force := fs.Bool("force", false, "back up and replace a hook cairn does not own")
	mode := fs.String("mode", "", "where records are written: message (default) or notes")
	agent := fs.String("agent", "all", "wire reactive recall into: claude-code, cursor, all, none")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repo, err := openRepo(env)
	if err != nil {
		return err
	}

	if *mode != "" {
		m := config.Mode(strings.ToLower(*mode))
		if m != config.ModeMessage && m != config.ModeNotes {
			return fmt.Errorf("unknown mode %q: use message or notes", *mode)
		}
		if _, err := gitx.Run(repo.Root, "config", "cairn.mode", string(m)); err != nil {
			return err
		}
	}

	bin, err := os.Executable()
	if err != nil {
		bin = "cairn"
	}
	bin, _ = filepath.Abs(bin)

	// The commit hooks and the reactive channel are installed independently, and
	// a failure in one must not skip the other: they live in different places
	// (.git/hooks versus the harness's own settings) and fail for different
	// reasons — a read-only .git, a sandbox, a hooks path owned by someone else.
	dir, err := hooksDir(repo)
	if err != nil {
		fmt.Fprintf(env.Out, "! commit hooks not installed: %v\n", err)
		dir = ""
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(env.Out, "! commit hooks not installed: %v\n", err)
		dir = ""
	}

	for _, name := range hookNames {
		if dir == "" {
			break
		}
		path := filepath.Join(dir, name)
		existing, err := os.ReadFile(path)
		switch {
		case err == nil && !strings.Contains(string(existing), marker):
			if !*force {
				fmt.Fprintf(env.Out, "! %s already exists and is not cairn's.\n", rel(repo.Root, path))
				fmt.Fprintf(env.Out, "  Add this line to it, or re-run with --force to back it up and replace it:\n\n")
				fmt.Fprintf(env.Out, "      %q hook %s \"$@\" || true\n\n", bin, name)
				continue
			}
			backup := path + ".cairn-backup"
			if err := os.WriteFile(backup, existing, 0o755); err != nil {
				fmt.Fprintf(env.Out, "! %s not replaced: %v\n", rel(repo.Root, path), err)
				continue
			}
			fmt.Fprintf(env.Out, "  backed up %s -> %s\n", rel(repo.Root, path), rel(repo.Root, backup))
		case err != nil && !os.IsNotExist(err):
			fmt.Fprintf(env.Out, "! %s not readable: %v\n", rel(repo.Root, path), err)
			continue
		}
		if err := os.WriteFile(path, []byte(hookScript(bin, name)), 0o755); err != nil {
			fmt.Fprintf(env.Out, "! %s not installed: %v\n", rel(repo.Root, path), err)
			continue
		}
		fmt.Fprintf(env.Out, "✓ installed %s\n", rel(repo.Root, path))
	}

	// The reactive channel is wired separately from the git hooks: it lives in
	// the harness's own settings, not in .git/hooks, because it fires on file
	// access rather than on commit.
	switch strings.ToLower(*agent) {
	case "none", "":
	case "claude-code":
		if err := installClaudeHooks(env, repo, bin); err != nil {
			fmt.Fprintf(env.Out, "! %v\n", err)
		}
	case "cursor":
		if err := installCursorHooks(env, repo, bin); err != nil {
			fmt.Fprintf(env.Out, "! %v\n", err)
		}
	case "all":
		if err := installClaudeHooks(env, repo, bin); err != nil {
			fmt.Fprintf(env.Out, "! %v\n", err)
		}
		if err := installCursorHooks(env, repo, bin); err != nil {
			fmt.Fprintf(env.Out, "! %v\n", err)
		}
	default:
		return fmt.Errorf("unknown --agent %q: use claude-code, cursor, all or none", *agent)
	}

	cfg := config.Load(repo, env.Getenv)
	fmt.Fprintf(env.Out, "\nmode: %s\n", cfg.Mode)
	if cfg.Mode == config.ModeNotes {
		fmt.Fprintln(env.Out, "\nNotes do not travel with clone by default. Everyone on the repo needs:")
		fmt.Fprintf(env.Out, "    git config --add remote.origin.fetch '+%s:%s'\n", gitx.NotesRef, gitx.NotesRef)
		fmt.Fprintf(env.Out, "    git push origin %s\n", gitx.NotesRef)
		fmt.Fprintln(env.Out, "  and `git log --notes=cairn` to read them.")
	}
	fmt.Fprintln(env.Out, "\nRun `cairn doctor` to check the rest of the setup.")
	return nil
}

// hooksDir honours core.hooksPath, which teams often point at a versioned
// directory. Installing into .git/hooks when that is set would silently do
// nothing.
func hooksDir(repo *gitx.Repo) (string, error) {
	if p := repo.ConfigGet("core.hooksPath"); p != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(repo.Root, p)
		}
		return p, nil
	}
	return filepath.Join(repo.GitDir, "hooks"), nil
}

// installedHook reports whether cairn's hook of the given name is in place.
func installedHook(repo *gitx.Repo, name string) bool {
	dir, err := hooksDir(repo)
	if err != nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	return err == nil && strings.Contains(string(b), marker)
}

func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return path
}
