// Command eve is the CLI on every machine and, as `eve serve`, the build server on the Mac.
package main

import (
	"os"

	"github.com/self-deprecated/eve/internal/cli"
)

// version is set at build time (-ldflags "-X main.version=…"): the release tag, or for Nix
// builds the package version plus the flake's revision ("0.1.0+4221ac1").
var version = "dev"

func main() {
	root, env := cli.Root(version)
	cli.AddServerCommands(root, env)
	os.Exit(cli.Main(root, env))
}
