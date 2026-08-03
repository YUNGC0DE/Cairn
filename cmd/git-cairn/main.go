// Command git-cairn records why a commit exists and hands it back to the next
// agent.
//
// The binary is named git-cairn so that git finds it as a subcommand: with it on
// PATH, `git cairn why <path>` works with no alias, config or plugin. Installing a
// `cairn` symlink next to it keeps the short form, and help text follows whichever
// name was used to invoke it.
package main

import (
	"fmt"
	"os"

	"github.com/YUNGC0DE/git-cairn/internal/cli"
)

func main() {
	cli.SetProgram(os.Args[0])
	if err := cli.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if err != cli.ErrUsage {
			fmt.Fprintln(os.Stderr, "cairn: "+err.Error())
		}
		os.Exit(1)
	}
}
