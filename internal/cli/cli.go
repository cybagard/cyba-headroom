// Package cli is the entry point for the multi-call headroom binary (ADR 0001).
// Invoked as docker, podman or tart it acts as the gate shim; otherwise it is
// the headroom CLI.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
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
	"github.com/cybagard/cyba-headroom/internal/shim"
	"github.com/cybagard/cyba-headroom/internal/source/docker"
	"github.com/cybagard/cyba-headroom/internal/source/host"
	"github.com/cybagard/cyba-headroom/internal/source/lmstudio"
	"github.com/cybagard/cyba-headroom/internal/source/ollama"
	"github.com/cybagard/cyba-headroom/internal/source/orca"
	"github.com/cybagard/cyba-headroom/internal/source/tart"
	"github.com/cybagard/cyba-headroom/internal/units"
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

	// Environ is the process environment, passed on to the real binary by
	// the shim. Nil means os.Environ.
	Environ func() []string

	// Test hooks; zero values mean the real behaviour.
	exec       func(path string, argv, env []string) error                            // syscall.Exec
	ask        func(config.Config, protocol.CheckRequest) (*protocol.Decision, error) // askDaemon
	release    func(config.Config, string) error                                      // releaseLease
	ancestors  func() []int                                                           // shim.Ancestors
	getwd      func() (string, error)                                                 // os.Getwd
	status     func(config.Config) (*protocol.Snapshot, error)                        // daemonStatus
	loginShell func(shell string) (string, error)                                     // askLoginShell
	now        func() time.Time                                                       // time.Now
	wait       func(time.Duration) os.Signal                                          // signalWait.sleep; 0: pending?
	raise      func(os.Signal)                                                        // reraise
	fallbacks  map[string][]string                                                    // shim.Fallbacks
	watchEvery time.Duration                                                          // poll interval (1s)
	suspend    chan os.Signal                                                         // delivers Ctrl-Z (SIGTSTP)
	stopSelf   func()                                                                 // stops the process (SIGSTOP)
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
		return runRun(e)
	case "doctor":
		return runDoctor(e)
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
	// An endpoint that is not on this Mac is never contacted.
	var ollamaAPI ollama.API
	if base, err := ollama.Endpoint(cfg.Ollama.Host, e.Getenv("OLLAMA_HOST")); err != nil {
		ollamaAPI = ollama.Unusable{Err: err}
	} else {
		ollamaAPI = ollama.NewHTTP(base)
	}
	dockerSrc := docker.New(cfg.Docker.Socket, vms)
	sources := []daemon.Source{
		host.New(host.System{}, cfg.Daemon.TrendWindow.Duration, time.Now),
		dockerSrc,
		tart.New(tartCLI, vmproc.Host{}, vms, e.Getenv("HOME")),
		orca.New(orcaCLI),
		lmstudio.New(lmsCLI, vmproc.Host{}, e.Getenv("HOME")),
		ollama.New(ollamaAPI, vmproc.Host{}, func() bool { return ollama.Installed(cfg.Ollama.Path, e.Getenv, ollama.Locations) }),
	}
	d, err := daemon.New(sources, cfg.Daemon.SourceTimeout.Duration, log)
	if err != nil {
		return fail(err)
	}
	wireGate(d, cfg, log, dockerSrc)
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

// shimSelvesVar lists the headroom binaries a shim call has passed through,
// so a later shim on the way (another headroom build's shim dir also on PATH)
// skips them all rather than exec back into one.
const shimSelvesVar = "HEADROOM_SHIM_SELVES"

// runShim replaces this process with the real docker, podman or tart (R5,
// #26). It passes every call through for now: #27 and #28 add the gate.
func runShim(e Env, name string) int {
	// One source for the shim's view of the environment and the child's.
	env, getenv := e.envOf()
	self, err := os.Executable()
	if err == nil {
		_, err = os.Stat(self)
	}
	if err != nil {
		fmt.Fprintf(e.Stderr, "headroom: %s: headroom's own binary is gone (%v); was it upgraded? Run `headroom install` again\n", name, err)
		return 126
	}
	selves := []string{self}
	for _, s := range filepath.SplitList(getenv(shimSelvesVar)) {
		if s != "" && s != self {
			selves = append(selves, s)
		}
	}
	target, err := shim.Resolve(name, selves, getenv, e.withDefaults().fallbacks[name])
	if err != nil {
		fmt.Fprintf(e.Stderr, "headroom: %s %v\n", name, err)
		return 127 // as the shell says for a missing command
	}
	c := shim.Parse(name, e.Args[1:])
	if getenv("HEADROOM_SHIM_DEBUG") != "" {
		gate := "pass"
		if c.Kind != "" {
			gate = "gate: " + c.Command
			if c.MemoryBytes > 0 {
				gate += ", " + units.Size(c.MemoryBytes)
			}
		}
		fmt.Fprintf(e.Stderr, "headroom: %s → %s (%s; %s)\n", name, target, shim.Engine(name, target), gate)
	}
	h := e.withDefaults()
	g := gate(e, name, c, getenv)
	if g.signal != nil {
		// Die of it, as the real binary would have, so a shell loop
		// around the call stops too.
		h.raise(g.signal)
	}
	if !g.proceed {
		return g.code
	}
	if g.checked {
		env = setEnv(env, shimCheckedVar, shim.Self())
	}
	env = setEnv(env, shimSelvesVar, strings.Join(selves, string(filepath.ListSeparator)))
	// argv[0] is the bare name, as the shell passes a command found on PATH:
	// the shim's own path would point the real binary back at the shim dir.
	args := e.Args[1:]
	if g.lease != "" {
		// The container carries its lease, so the daemon tells it from one
		// started past the shim (#33).
		if la, ok := shim.Labelled(name, args, protocol.LeaseLabel, g.lease); ok {
			args = la
		}
	}
	argv := append([]string{name}, args...)
	// On success this never returns: the real binary takes over the
	// process, with its PID, terminal, signals and exit code.
	if err := h.exec(target, argv, env); err != nil {
		fmt.Fprintf(e.Stderr, "headroom: running %s: %v\n", target, err)
		if g.lease != "" {
			// Nothing started: hand the lease back rather than hold the
			// budget until it times out.
			_ = h.release(g.cfg, g.lease)
		}
		if errors.Is(err, syscall.ENOENT) {
			return 127 // gone since it was found (an upgrade): not found
		}
		return 126 // found but not runnable
	}
	return 0
}

// envOf is the process environment (Environ, else os.Environ) and a lookup
// in it. The first of duplicates wins, as libc's getenv in the child reads
// it; setEnv leaves none of the keys it sets.
func (e Env) envOf() ([]string, func(string) string) {
	environ := e.Environ
	if environ == nil {
		environ = os.Environ
	}
	env := environ()
	return env, func(k string) string {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, k+"="); ok {
				return v
			}
		}
		return ""
	}
}

// setEnv sets k=v in env, replacing rather than adding: Go's and libc's
// getenv read the first of duplicates.
func setEnv(env []string, k, v string) []string {
	env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, k+"=") })
	return append(env, k+"="+v)
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
func wireGate(d *daemon.Daemon, cfg config.Config, log *slog.Logger, docker Inspector) {
	book := lease.New(cfg.Policy.LeaseTimeout.Duration, time.Now, log)
	d.SetDerive(derive(book, cfg.Budget.Params()))
	d.SetCheck(gateCheckOn(book, cfg.PolicyConfig(), docker, cfg.Docker.Socket))
	d.SetRelease(book.Release)
}

// derive fills in what each tick computes from the sources: the budget,
// attribution, and, once leases are settled, the open leases and the
// containers and VMs that appeared without a check (#33).
func derive(book *lease.Book, params budget.Params) func(*protocol.Snapshot) {
	return func(s *protocol.Snapshot) {
		b := budget.Compute(s, params)
		s.Budget = &b
		at := attribution.Attribute(s)
		s.Attribution = &at
		book.Observe(s)
		s.Leases = book.List()
		s.Ungated = book.Ungated()
	}
}

// Inspector resolves a container name or ID to its full ID and labels:
// the Docker source.
type Inspector interface {
	Inspect(ctx context.Context, ref string) (id string, labels map[string]string, running bool, err error)
}

// inspectTimeout bounds the Docker lookup a start's check makes: well within
// the shim's daemon timeout.
const inspectTimeout = 200 * time.Millisecond

// gateCheckOn answers a check: it finds the calling worktree (#28), then
// decides with the lease book. The daemon reads the Docker engine at
// socket: only a start on that engine is looked up there.
func gateCheckOn(book *lease.Book, pol policy.Config, docker Inspector, socket string) daemon.CheckFunc {
	return func(r *protocol.CheckRequest, s *protocol.Snapshot) protocol.Decision {
		id, by := attribution.Identify(s, attribution.Caller{Worktree: r.Worktree, Cwd: r.Cwd, RealCwd: r.RealCwd, Ancestors: r.Ancestors})
		req := policy.Request{Worktree: id, Kind: r.Kind, Command: r.Command, CostBytes: r.CostBytes, MacOS: r.MacOS, VMUnknown: r.VMUnknown, PID: r.PID,
			Target: r.Target, Name: r.Name, Labelled: r.Labelled}
		if r.ComposeDir != "" {
			// Resolved here, outside the book's lock: Compose's labels are
			// then compared as strings.
			req.ComposeDirs = []string{filepath.Clean(r.ComposeDir)}
			if resolved, err := filepath.EvalSymlinks(r.ComposeDir); err == nil && resolved != req.ComposeDirs[0] {
				req.ComposeDirs = append(req.ComposeDirs, resolved)
			}
		}
		if r.Kind == "container" && (r.Op == "start" || r.Op == "restart") && r.Target != "" && docker != nil && sameSocket(r.Engine, socket) {
			// The container exists: its ID is the lease's key, and a lease
			// label on it names the run or create this start takes over.
			// Asked afresh every check: whether it runs changes by the
			// second (docker stop && docker start).
			ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
			if cid, labels, running, err := docker.Inspect(ctx, r.Target); err == nil {
				req.ContainerID, req.TakesOver, req.Running = cid, labels[protocol.LeaseLabel], running
				req.MultiTarget = r.MultiTarget
			}
			cancel()
		}
		d := book.Check(req, s, pol)
		d.Worktree, d.IdentifiedBy = id, by
		return d
	}
}
