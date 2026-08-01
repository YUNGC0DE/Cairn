// Package cli is cairn's command surface.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/YUNGC0DE/Cairn/internal/gitx"
	"github.com/YUNGC0DE/Cairn/internal/llm"
)

// Version is the build version, overridden at link time by goreleaser.
var Version = "0.1.0-dev"

// ErrUsage signals that usage was already printed.
var ErrUsage = errors.New("usage")

// Env carries the process I/O so commands stay testable.
type Env struct {
	Out    io.Writer
	Err    io.Writer
	Getenv func(string) string
	Dir    string

	// Engine overrides engine selection. Tests inject a scripted engine here so
	// the whole hook path can run without spawning an agent; in production it is
	// nil and selection goes through llm.Pick.
	Engine llm.Engine
}

// engine resolves the engine to distil with.
func (e *Env) engine(name string) (llm.Engine, error) {
	if e.Engine != nil {
		return e.Engine, nil
	}
	return llm.Pick(name)
}

type command struct {
	name    string
	summary string
	usage   string
	run     func(*Env, []string) error
}

func commands() []command {
	return []command{
		{"init", "install the prepare-commit-msg hook in this repository",
			"cairn init [--force] [--mode message|notes]", cmdInit},
		{"hook", "run a git hook (invoked by git, not by hand)",
			"cairn hook prepare-commit-msg <file> [source] [sha]", cmdHook},
		{"why", "show the records behind a path, and why it looks like this",
			"cairn why <path>[:line] [-n N]", cmdWhy},
		{"rejected", "search alternatives that were already turned down",
			"cairn rejected <query> [-n N]", cmdRejected},
		{"show", "show the record of one commit",
			"cairn show [<commit>]", cmdShow},
		{"audit", "distil past commits to measure what the corpus actually contains",
			"cairn audit [-n N] [--since <date>] [--jobs N] [--out file.json] [--no-verify]", cmdAudit},
		{"sessions", "list agent sessions cairn can see for this repository",
			"cairn sessions [--all]", cmdSessions},
		{"doctor", "check every dependency, and call each engine to prove it answers",
			"cairn doctor", cmdDoctor},
		{"logs", "what cairn did on recent commits, and why a record degraded",
			"cairn logs [-n N] [--path]", cmdLogs},
		{"version", "print the version", "cairn version", cmdVersion},
	}
}

// Run dispatches a command.
func Run(args []string, out, errw io.Writer) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	env := &Env{Out: out, Err: errw, Getenv: os.Getenv, Dir: dir}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(out)
		if len(args) == 0 {
			return ErrUsage
		}
		return nil
	}
	for _, c := range commands() {
		if c.name == args[0] {
			if err := c.run(env, args[1:]); err != nil {
				if errors.Is(err, flag.ErrHelp) {
					fmt.Fprintln(out, "usage: "+c.usage)
					return nil
				}
				return err
			}
			return nil
		}
	}
	fmt.Fprintf(errw, "cairn: unknown command %q\n\n", args[0])
	usage(errw)
	return ErrUsage
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "cairn — the commit is the context carrier.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "usage: cairn <command> [flags]")
	fmt.Fprintln(w)
	cs := commands()
	sort.Slice(cs, func(i, j int) bool { return cs[i].name < cs[j].name })
	for _, c := range cs {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "settings (git config key, or the CAIRN_* environment variable):")
	fmt.Fprintln(w, "  cairn.enabled  CAIRN_ENABLED   turn cairn off for a repository")
	fmt.Fprintln(w, "  cairn.mode     CAIRN_MODE      message (default) | notes")
	fmt.Fprintln(w, "  cairn.engine   CAIRN_ENGINE    claude-code | cursor-agent")
	fmt.Fprintln(w, "  cairn.model    CAIRN_MODEL     model alias passed to the engine")
	fmt.Fprintln(w, "  cairn.verifyModel               model for the verification pass (default: same)")
	fmt.Fprintln(w, "  cairn.effort   CAIRN_EFFORT     reasoning effort, low…max (claude: low)")
	fmt.Fprintln(w, "  cairn.timeout  CAIRN_TIMEOUT   seconds per session (default 60; ×N sessions)")
	fmt.Fprintln(w, "                 CAIRN_SKIP=1    skip cairn for one commit")
	fmt.Fprintln(w, "                 CAIRN_DEBUG=1   explain what cairn is doing")
	fmt.Fprintln(w, "                 CAIRN_CLAUDE_ROOT / CAIRN_CURSOR_ROOT / CAIRN_CURSOR_IDE_ROOT")
	fmt.Fprintln(w, "                                 override where transcripts are read from")
}

// flags builds a flag set that reports errors through the command's usage.
func flags(name, usage string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintln(out, "usage: "+usage)
		fs.PrintDefaults()
	}
	return fs
}

// openRepo resolves the repository the command is running in.
func openRepo(env *Env) (*gitx.Repo, error) {
	repo, err := gitx.Open(env.Dir)
	if err != nil {
		return nil, fmt.Errorf("not inside a git repository (%s)", env.Dir)
	}
	return repo, nil
}

func cmdVersion(env *Env, args []string) error {
	fmt.Fprintf(env.Out, "cairn %s\n", Version)
	return nil
}

// plural renders "1 thing" / "3 things".
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
