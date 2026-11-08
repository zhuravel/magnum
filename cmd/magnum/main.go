// Command magnum is the PR review daemon and its CLI.
package main

import (
	"os"

	"github.com/zhuravel/magnum/internal/cli"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == hookEventCmd {
		runHookEvent(os.Args[2:]) // before cobra: an agent's hook, which must stay light
	}
	os.Exit(cli.Main(version, os.Args[1:], os.Stdout, os.Stderr))
}
