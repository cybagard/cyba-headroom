// Command headroom is admission control for parallel coding agents on one Mac.
package main

import (
	"os"

	"github.com/cybagard/cyba-headroom/internal/cli"
)

func main() {
	os.Exit(cli.Run(cli.Env{Args: os.Args, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, Environ: os.Environ}))
}
