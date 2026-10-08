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

	"github.com/cybagard/cyba-headroom/internal/attribution"
	"github.com/cybagard/cyba-headroom/internal/budget"
	"github.com/cybagard/cyba-headroom/internal/client"
	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/lease"
	"github.com/cybagard/cyba-headroom/internal/logfile"
	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/samples"
	"github.com/cybagard/cyba-headroom/internal/source/docker"
	"github.com/cybagard/cyba-headroom/internal/source/host"
	"github.com/cybagard/cyba-headroom/internal/source/lmstudio"
	"github.com/cybagard/cyba-headroom/internal/source/orca"
	"github.com/cybagard/cyba-headroom/internal/source/tart"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
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
	// Terminal reports whether Stdout is a terminal, and its width. Nil
	// means: ask the OS about Stdout.
	Terminal func() (tty bool, width int)

	// Test hooks for --watch; zero values mean the real behaviour.
	watchEvery time.Duration  // poll interval (1s)
	suspend    chan os.Signal // delivers Ctrl-Z (SIGTSTP)
	stopSelf   func()         // stops the process (SIGSTOP)
}

// signalContext is e.Context (or Background) that also ends on sigs.
func signalContext(e Env, sigs ...os.Signal) (context.Context, context.CancelFunc) {
	base := e.Context
	if base == nil {
		base = context.Background()
	}
	return signal.NotifyContext(base, sigs...)
}

// unreachable tells the user the daemon is down and how to start it.
func unreachable(e Env, socket string, err error) {
	fmt.Fprintf(e.Stderr, "headroom: daemon not reachable at %s: %v (start it with `headroom install`, or `headroom daemon` in a terminal)\n", socket, err)
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
	case "", "--watch", "--all":
		return runView(e)
	case "daemon":
		return runDaemon(e)
	case "status":
		return runStatus(e)
	case "install", "uninstall":
		return runInstall(e, cmd == "uninstall")
	case "logs":
		return runLogs(e)
	case "suggest":
		return runSuggest(e)
	case "check":
		return runCheck(e)
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
	logPath := ""
	for a := e.Args[2:]; len(a) > 0; a = a[1:] {
		if a[0] != "--log" || len(a) < 2 {
			fmt.Fprintf(e.Stderr, "headroom: daemon: usage: headroom daemon [--log FILE]\n")
			return 2
		}
		logPath, a = a[1], a[1:]
	}
	// Open the log first, so a startup failure under launchd is in the log
	// that `headroom logs` shows, not only in the crash log.
	logOut := e.Stderr
	if logPath != "" {
		// Size-capped, so weeks under launchd cannot fill the disk.
		f, err := logfile.Open(logPath, 5<<20)
		if err != nil {
			fmt.Fprintln(e.Stderr, "headroom:", err)
			return 1
		}
		defer func() { _ = f.Close() }()
		logOut = f
		// launchd's crash log has no cap; a crash loop must not grow it forever.
		if crash, ok := e.Stderr.(*os.File); ok {
			trimCrashLog(crash, 1<<20)
		}
	}
	log := slog.New(slog.NewTextHandler(logOut, nil))
	fail := func(err error) int {
		if logPath != "" {
			log.Error("daemon failed to start", "err", err)
		}
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	cfg, err := config.Load(e.Getenv)
	if err != nil {
		return fail(err)
	}
	// Collectors register here as they land (#49). Docker and Tart
	// share one VM process listing per tick.
	vms := vmproc.NewShared(vmproc.New(vmproc.Host{}), time.Second, time.Now)
	var tartCLI tart.CLI
	if p := tart.Locate(cfg.Tart.Path, e.Getenv); p != "" {
		tartCLI = tart.Exec{Path: p}
	}
	var orcaCLI orca.CLI
	if p := orca.Locate(cfg.Orca.Path, e.Getenv); p != "" {
		orcaCLI = orca.Exec{Path: p}
	}
	var lmsCLI lmstudio.CLI
	if p := lmstudio.Locate(cfg.LMStudio.Path, e.Getenv); p != "" {
		lmsCLI = lmstudio.Exec{Path: p}
	}
	sources := []daemon.Source{
		host.New(host.System{}, cfg.Daemon.TrendWindow.Duration, time.Now),
		docker.New(cfg.Docker.Socket, vms),
		tart.New(tartCLI, vmproc.Host{}, vms, e.Getenv("HOME")),
		orca.New(orcaCLI),
		lmstudio.New(lmsCLI, vmproc.Host{}, e.Getenv("HOME")),
	}
	d, err := daemon.New(sources, cfg.Daemon.SourceTimeout.Duration, log)
	if err != nil {
		return fail(err)
	}
	wireGate(d, cfg, log)
	ln, err := daemon.Listen(cfg.Socket)
	if err != nil {
		return fail(err)
	}
	ctx, stop := signalContext(e, os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("daemon started", "socket", cfg.Socket, "interval", cfg.Daemon.Interval.Duration, "samples", cfg.Samples.Enabled)

	var wg sync.WaitGroup
	if cfg.Samples.Enabled {
		w := samples.NewWriter(cfg.SamplesDir(), cfg.Samples.Retention.Duration, log)
		d.OnPublish(w.Offer)
		wg.Go(func() { w.Run(ctx) })
	}
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

// runStatus prints the daemon's raw snapshot as JSON; runView is the human
// view.
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
		unreachable(e, cfg.Socket, err)
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

  headroom [--all]    observe view (one shot); --all also shows worktrees
                      with no agent and nothing running
  headroom --watch [--all]
                      observe view, redrawn in place; Ctrl-C to quit
  headroom config     print the effective config and its path
  headroom daemon [--log FILE]
                      run the collector daemon; --log writes its log to FILE,
                      rotated at 5 MB
  headroom check [--worktree ID] [--cost 2G] [--kind container|compose|tart] -- cmd...
                      ask the policy whether cmd may start; exit 0 allow, 75 deny
  headroom status [--json]
                      print the daemon's raw snapshot as JSON
  headroom run -- <agent> [args]
                      launch an agent with the shim dir first on PATH
  headroom doctor     check PATH order and identity in this shell
  headroom install [--bin PATH]
                      run the daemon as a LaunchAgent; copies this binary to
                      PATH (default ~/.local/bin/headroom)
  headroom uninstall [--bin PATH]
                      remove the agent and binary; keeps config, samples, logs
  headroom logs [-f]  show (or follow) the daemon's log
  headroom suggest [--since 14d] [--write]
                      suggest [budget] and [policy] values from the recorded
                      samples; --write merges them into config.toml
  headroom version    print the version
`)
}

// wireGate connects the budget (#19), attribution (#20), policy (#24) and
// leases (#25) to d. Every tick derives the budget and attribution, settles
// or expires leases against that snapshot, and lists the open ones in it.
// Checks decide against the latest snapshot and the open leases.
func wireGate(d *daemon.Daemon, cfg config.Config, log *slog.Logger) {
	params := cfg.Budget.Params()
	book := lease.New(cfg.Policy.LeaseTimeout.Duration, time.Now, log)
	d.SetDerive(func(s *protocol.Snapshot) {
		b := budget.Compute(s, params)
		s.Budget = &b
		at := attribution.Attribute(s)
		s.Attribution = &at
		book.Observe(s)
		s.Leases = book.List()
	})
	pol := cfg.Policy.Config()
	d.SetCheck(func(r *protocol.CheckRequest, s *protocol.Snapshot) protocol.Decision {
		return book.Check(policy.Request{Worktree: r.Worktree, Kind: r.Kind, Command: r.Command, Args: r.Args, CostBytes: r.CostBytes}, s, pol)
	})
}
