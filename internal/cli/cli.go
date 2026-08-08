// Package cli is cairn's command surface.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/YUNGC0DE/git-cairn/internal/gitx"
	"github.com/YUNGC0DE/git-cairn/internal/llm"
)

// Version is the build version, overridden at link time by goreleaser.
var Version = "0.3.0"

// prog is the command name to print in help text.
//
// The binary is installed as `git-cairn`, which is how git finds it as a
// subcommand, and usually symlinked to `cairn` for the short form. Help that says
// `cairn init` to someone who typed `git cairn` (or the reverse) sends them to a
// command they do not have, so the name is taken from argv[0]: git execs external
// subcommands with argv[0] set to `git-cairn`.
var prog = "cairn"

// SetProgram records how cairn was invoked. Anything other than a git-cairn
// binary keeps the short name, including `go run` and the test binary.
func SetProgram(argv0 string) {
	if filepath.Base(argv0) == "git-cairn" {
		prog = "git cairn"
	}
}

// ErrUsage signals that usage was already printed.
var ErrUsage = errors.New("usage")

// Env carries the process I/O so commands stay testable.
type Env struct {
	Out    io.Writer
	Err    io.Writer
	Getenv func(string) string
	Dir    string

	// Stdin is where the reactive hooks read their event payload. Tests set it;
	// in production it is nil and In() falls back to os.Stdin.
	Stdin io.Reader

	// Engine overrides engine selection. Tests inject a scripted engine here so
	// the whole hook path can run without spawning an agent; in production it is
	// nil and selection goes through llm.Pick.
	Engine llm.Engine
}

// In returns the reader hooks take their event from.
func (e *Env) In() io.Reader {
	if e.Stdin != nil {
		return e.Stdin
	}
	return os.Stdin
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
		{"init", "install both halves: the commit hooks, and the harness hooks that serve records back",
			prog + " init [--force] [--mode message|notes] [--agent claude-code|cursor|all|none]", cmdInit},
		{"hook", "run a git hook (invoked by git, not by hand)",
			prog + " hook prepare-commit-msg <file> [source] [sha]", cmdHook},
		{"context", "the rules an agent is served when it touches a path (what the hooks send)",
			prog + " context --file <path> [--session <id>] [--reset] [--json]", cmdContext},
		{"show", "show the rules one commit recorded, with the reasoning behind each",
			prog + " show [<commit>]", cmdShow},
		{"sessions", "list agent sessions cairn can see for this repository",
			prog + " sessions [--all]", cmdSessions},
		{"doctor", "check every dependency, and call each engine to prove it answers",
			prog + " doctor", cmdDoctor},
		{"logs", "what cairn did on recent commits, and why a record degraded",
			prog + " logs [-n N] [--path]", cmdLogs},
		{"version", "print the version", prog + " version", cmdVersion},
	}
}

// Run dispatches a command.
func Run(args []string, out, errw io.Writer) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	env := &Env{Out: out, Err: errw, Getenv: os.Getenv, Dir: dir, Stdin: os.Stdin}
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
	fmt.Fprintln(w, "git-cairn — distils an agent session's decisions into per-file rules on the commit,")
	fmt.Fprintln(w, "            and serves them back when an agent next touches those files.")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "usage: %s <command> [flags]\n", prog)
	if prog == "cairn" {
		fmt.Fprintln(w, "       git cairn <command> [flags]   (same binary, found by git on PATH)")
	}
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
	fmt.Fprintf(env.Out, "git-cairn %s\n", Version)
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
