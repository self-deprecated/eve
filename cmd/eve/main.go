// Command eve is the CLI on every machine and, as `eve serve`, the build server on Quartz.
package main

import (
	"os"

	"github.com/self-deprecated/eve/internal/cli"
)

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

func main() {
	root, env := cli.Root(version)
	cli.AddServerCommands(root, env)
	os.Exit(cli.Main(root, env))
}
