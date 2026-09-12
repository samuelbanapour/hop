// Command hop is a fast, atomic package manager for command-line tools.
//
// Every operation builds a new generation of the installed set and activates
// it by replacing a single symlink, so installs are atomic, rollbacks are
// instant, and a failure never leaves a half-installed environment behind.
package main

import (
	"os"

	"github.com/samuelbanapour/hop/internal/cmds"
)

func main() { os.Exit(cmds.Main(os.Args[1:])) }
