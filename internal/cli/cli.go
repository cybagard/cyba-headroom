// Package cli is the entry point for the multi-call headroom binary (ADR 0001).
// Invoked as docker, podman or tart it acts as the gate shim; otherwise it is
// the headroom CLI.
package cli

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/cybagard/cyba-headroom/internal/config"
)

// Version is set at build time with -ldflags "-X .../internal/cli.Version=...".
var Version = "dev"

// ShimNames are the argv[0] basenames that select shim mode.
var ShimNames = map[string]bool{"docker": true, "podman": true, "tart": true}

// Env carries the process environment so Run is testable without globals.
type Env struct {
	Args   []string
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
}

// Run executes one invocation and returns the process exit code.
func Run(e Env) int {
	if len(e.Args) == 0 {
		fmt.Fprintln(e.Stderr, "headroom: no argv[0]")
		return 2
	}
	if name := filepath.Base(e.Args[0]); ShimNames[name] {
		return runShim(e, name)
	}
	cmd := ""
	if len(e.Args) > 1 {
		cmd = e.Args[1]
	}
	switch cmd {
	case "version", "--version", "-v":
		fmt.Fprintf(e.Stdout, "headroom %s\n", Version)
		return 0
	case "help", "--help", "-h":
		usage(e.Stdout)
		return 0
	case "config":
		return runConfig(e)
	case "", "--watch":
		return notYet(e, "observe view", 21)
	case "daemon":
		return notYet(e, "daemon", 13)
	case "install", "uninstall":
		return notYet(e, cmd, 22)
	case "run":
		return notYet(e, "run", 31)
	case "doctor":
		return notYet(e, "doctor", 32)
	default:
		fmt.Fprintf(e.Stderr, "headroom: unknown command %q\n\n", cmd)
		usage(e.Stderr)
		return 2
	}
}

func runConfig(e Env) int {
	cfg, err := config.Load(e.Getenv)
	if err != nil {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	fmt.Fprintf(e.Stdout, "# %s\n", filepath.Join(cfg.Dir, config.FileName))
	if err := toml.NewEncoder(e.Stdout).Encode(cfg); err != nil {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	return 0
}

// runShim is the gate shim placeholder; the real passthrough lands in #26.
func runShim(e Env, name string) int {
	fmt.Fprintf(e.Stderr, "headroom: %s shim is not implemented yet (#26); remove the shim dir from PATH\n", name)
	return 127
}

func notYet(e Env, what string, issue int) int {
	fmt.Fprintf(e.Stderr, "headroom: %s is not implemented yet (#%d)\n", what, issue)
	return 1
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: headroom [command]

  headroom            observe view (one shot)
  headroom --watch    observe view, refreshing
  headroom config     print the effective config and its path
  headroom daemon     run the collector daemon
  headroom run -- <agent> [args]
                      launch an agent with the shim dir first on PATH
  headroom doctor     check PATH order and identity in this shell
  headroom install    install the launchd agent and shims
  headroom version    print the version
`)
}
