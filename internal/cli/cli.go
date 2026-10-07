// Package cli is the entry point for the multi-call headroom binary (ADR 0001).
// Invoked as docker, podman or tart it acts as the gate shim; otherwise it is
// the headroom CLI.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cybagard/cyba-headroom/internal/client"
	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/source/host"
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
	// Context ends long-running commands (the daemon) besides SIGINT/SIGTERM.
	// Nil means context.Background().
	Context context.Context
}

// trendWindow is how much host pressure history the observe view shows (R4).
const trendWindow = 5 * time.Minute

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
		return runDaemon(e)
	case "status":
		return runStatus(e)
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

// runDaemon runs the collector daemon until SIGINT or SIGTERM.
func runDaemon(e Env) int {
	cfg, err := config.Load(e.Getenv)
	if err != nil {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	log := slog.New(slog.NewTextHandler(e.Stderr, nil))
	// Collectors register here as they land (#14–#17).
	sources := []daemon.Source{host.New(host.System{}, trendWindow, time.Now)}
	d, err := daemon.New(sources, cfg.Daemon.SourceTimeout.Duration, log)
	if err != nil {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	ln, err := daemon.Listen(cfg.Socket)
	if err != nil {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	base := e.Context
	if base == nil {
		base = context.Background()
	}
	ctx, stop := signal.NotifyContext(base, os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("daemon started", "socket", cfg.Socket, "interval", cfg.Daemon.Interval.Duration)

	var wg sync.WaitGroup
	wg.Go(func() { d.Run(ctx, cfg.Daemon.Interval.Duration) })
	err = d.Serve(ctx, ln)
	stop()
	wg.Wait()
	if err != nil {
		log.Error("daemon stopped", "err", err)
		return 1
	}
	log.Info("daemon stopped")
	return 0
}

// runStatus prints the daemon's raw snapshot as JSON. The human view is #21.
func runStatus(e Env) int {
	for _, a := range e.Args[2:] {
		if a != "--json" {
			fmt.Fprintf(e.Stderr, "headroom: status: unknown argument %q\n", a)
			return 2
		}
	}
	cfg, err := config.Load(e.Getenv)
	if err != nil {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	snap, err := client.Status(context.Background(), cfg.Socket, cfg.Policy.DaemonTimeout.Duration)
	if err != nil {
		fmt.Fprintf(e.Stderr, "headroom: daemon not reachable at %s: %v (start it with `headroom daemon`)\n", cfg.Socket, err)
		return 1
	}
	enc := json.NewEncoder(e.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
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
  headroom status [--json]
                      print the daemon's raw snapshot as JSON
  headroom run -- <agent> [args]
                      launch an agent with the shim dir first on PATH
  headroom doctor     check PATH order and identity in this shell
  headroom install    install the launchd agent and shims
  headroom version    print the version
`)
}
