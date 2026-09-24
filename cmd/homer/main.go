package main

import (
	"os"

	"github.com/zzjcool/homer-cli/internal/cli"
)

// version is populated by the release build. P0 keeps the entry point thin;
// command behavior belongs in internal/cli.
var version = "dev"

func main() {
	cli.SetVersion(version)
	os.Exit(cli.Run(os.Args))
}
