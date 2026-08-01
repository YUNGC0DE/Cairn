// Command cairn records why a commit exists and hands it back to the next agent.
package main

import (
	"fmt"
	"os"

	"github.com/YUNGC0DE/Cairn/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if err != cli.ErrUsage {
			fmt.Fprintln(os.Stderr, "cairn: "+err.Error())
		}
		os.Exit(1)
	}
}
