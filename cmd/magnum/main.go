// Command magnum is the PR review daemon and its CLI.
package main

import (
	"os"

	"github.com/zhuravel/magnum/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(cli.Main(version, os.Args[1:], os.Stdout, os.Stderr))
}
