package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cybagard/cyba-headroom/internal/binpath"
	"github.com/cybagard/cyba-headroom/internal/client"
	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/launchd"
)

// agentLabel names the LaunchAgent (#22).
const agentLabel = "io.github.cybagard.headroom"

// installEnv is the environment the agent inherits from `headroom install`:
// what decides where the daemon finds its config and Docker.
var installEnv = []string{"HEADROOM_CONFIG_DIR", "XDG_CONFIG_HOME", "DOCKER_HOST"}

// agentControl is what install needs from launchd; launchd.Agent does it.
type agentControl interface {
	Load(ctx context.Context, plist string) error
	Unload(ctx context.Context) error
	// PID is the agent's running process; 0 if not loaded or not running.
	PID(ctx context.Context) (int, error)
}

// logPaths are the daemon's log directory, its log and its crash log, shared
// by install and logs.
func logPaths(home string) (dir, log, crash string) {
	dir = filepath.Join(home, "Library", "Logs", "headroom")
	return dir, filepath.Join(dir, "daemon.log"), filepath.Join(dir, "daemon.stderr.log")
}

// installer installs and removes the daemon's LaunchAgent.
type installer struct {
	home string
	exe  string // the running binary
	bin  string // where the agent's copy goes
	// binGiven is true when --bin chose bin; uninstall otherwise removes the
	// binary the installed agent runs.
	binGiven bool
	agent    agentControl
	// ping returns the PID of the daemon answering on the socket.
	ping   func(context.Context) (int, error)
	getenv func(string) string
	out    io.Writer
	errw   io.Writer

	// shimDir gets the docker, podman and tart links (#31); lookPath says
	// whether an agent CLI is on PATH, for the Orca settings to print.
	shimDir  string
	lookPath func(name string) bool
	// How long install waits for the daemon to answer, and how often it asks.
	wait, poll time.Duration
}

func (in *installer) plistPath() string { return agentPlist(in.home) }

// agentPlist is where install writes headroom's LaunchAgent.
func agentPlist(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist")
}

// brewPlist is the LaunchAgent `brew services` wrote for headroom, or ""
// if there is none (#185). Homebrew 6 names it sh.brew.headroom; it still
// honours its legacy name, homebrew.mxcl.headroom.
func brewPlist(home string) string {
	for _, name := range []string{"sh.brew.headroom.plist", "homebrew.mxcl.headroom.plist"} {
		p := filepath.Join(home, "Library", "LaunchAgents", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// install copies the binary, writes the plist, (re)loads the agent and waits
// for the daemon to answer.
func (in *installer) install(ctx context.Context) int {
	// A second agent would start a second daemon, which crash-loops on the
	// socket the first holds (#185).
	if brew := brewPlist(in.home); brew != "" {
		fmt.Fprintf(in.errw, "headroom: Homebrew runs the daemon (%s); manage it with `brew services`, not headroom install\n", brew)
		return 1
	}
	// Copying over a link replaces it: Homebrew's link to the formula's
	// binary would become this older copy (#183).
	if fi, err := os.Lstat(in.bin); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		if same, _ := sameFile(in.bin, in.exe); !same {
			fmt.Fprintf(in.errw, "headroom: %s links to another headroom binary; to install it, run: \"%s\" install --bin \"%s\"\n", in.bin, in.bin, in.bin)
			return 1
		}
	}
	// Only the launchd daemon may hold the socket: a terminal daemon would
	// make the agent's daemon fail to start and crash-loop.
	if pid, err := in.ping(ctx); err == nil {
		agentPID, err := in.agent.PID(ctx)
		if err != nil {
			return in.fail(err)
		}
		// A daemon from before #22 reports no PID: it is ours if launchd
		// runs the agent.
		ours := agentPID != 0 && (pid == agentPID || pid == 0)
		if !ours {
			who := "a daemon outside launchd"
			if pid != 0 {
				who = fmt.Sprintf("a daemon outside launchd (pid %d)", pid)
			}
			fmt.Fprintf(in.errw, "headroom: %s holds the socket (`headroom daemon` in a terminal?); stop it, then run install again\n", who)
			return 1
		}
	}
	// launchd runs the agent from /, so every path it gets must be absolute.
	bin, err := filepath.Abs(in.bin)
	if err != nil {
		return in.fail(err)
	}
	env := map[string]string{}
	for _, k := range installEnv {
		v := in.getenv(k)
		if v != "" && k != "DOCKER_HOST" && !filepath.IsAbs(v) {
			if v, err = filepath.Abs(v); err != nil {
				return in.fail(err)
			}
		}
		env[k] = v
	}
	if err := copyBinary(in.exe, bin); err != nil {
		return in.fail(fmt.Errorf("installing the binary: %w", err))
	}
	dir, logPath, crashPath := logPaths(in.home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return in.fail(err)
	}
	// A fresh crash log, so a failure below shows this run's errors only.
	// Created here because launchd would create it world-readable.
	if err := os.WriteFile(crashPath, nil, 0o600); err != nil {
		return in.fail(err)
	}
	_ = os.Chmod(crashPath, 0o600)
	plist := launchd.Plist(launchd.Spec{
		Label:  agentLabel,
		Args:   []string{bin, "daemon", "--log", logPath},
		Env:    env,
		Stderr: crashPath,
	})
	if err := writeFile(in.plistPath(), plist, 0o644); err != nil {
		return in.fail(fmt.Errorf("writing the agent: %w", err))
	}
	if err := in.agent.Load(ctx, in.plistPath()); err != nil {
		return in.fail(err)
	}
	if !in.waitUp(ctx) {
		fmt.Fprintf(in.errw, "headroom: the daemon did not answer within %s; its logs:\n", in.wait)
		tail(in.errw, logPath, 20)
		tail(in.errw, crashPath, 20)
		return 1
	}
	fmt.Fprintf(in.out, "headroom daemon running under launchd (%s)\n  binary  %s\n  agent   %s\n  log     %s (headroom logs)\n",
		agentLabel, bin, in.plistPath(), logPath)
	in.installShims(bin)
	return 0
}

// orcaAgents are the agent CLIs whose Orca launch command install prints.
var orcaAgents = []string{"claude", "codex", "kilo", "opencode", "gemini", "goose", "amp"}

// installShims links the shims to bin and says how to launch Orca's agents
// through them (R9, #31). A failure is reported, not fatal: the daemon runs.
func (in *installer) installShims(bin string) {
	if in.shimDir == "" {
		return
	}
	notes, err := linkShims(in.shimDir, bin)
	for _, n := range notes {
		fmt.Fprintln(in.errw, "headroom:", n)
	}
	if err != nil {
		fmt.Fprintf(in.errw, "headroom: linking the shims in %s: %v\n", in.shimDir, err)
		return
	}
	fmt.Fprintf(in.out, "  shims   %s (docker, podman, tart)\n", in.shimDir)
	// A config dir other than the default must reach the agents' shims too.
	prefix := ""
	if in.getenv("HEADROOM_CONFIG_DIR") != "" || in.getenv("XDG_CONFIG_HOME") != "" {
		if dir, err := config.Dir(in.getenv); err == nil {
			if abs, err := filepath.Abs(dir); err == nil {
				prefix = "env HEADROOM_CONFIG_DIR=" + shellWord(abs) + " "
			}
		}
	}
	var lines []string
	for _, a := range orcaAgents {
		if in.lookPath != nil && in.lookPath(a) {
			lines = append(lines, fmt.Sprintf("  %s: %s%s run -- %s", a, prefix, shellWord(bin), a))
		}
	}
	if len(lines) > 0 {
		fmt.Fprintf(in.out, "\nTo gate the agents Orca launches, set each one's command in Orca → Settings → Agents:\n%s\n"+
			"Agents already running keep their old PATH until restarted.\n", strings.Join(lines, "\n"))
	}
}

// waitUp waits until the launchd daemon itself answers on the socket.
func (in *installer) waitUp(ctx context.Context) bool {
	deadline := time.Now().Add(in.wait)
	for {
		pid, err := in.ping(ctx)
		if err == nil && pid != 0 {
			if agentPID, _ := in.agent.PID(ctx); agentPID == pid {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(in.poll):
		}
	}
}

// uninstall stops the agent and removes it and the binary install copied. Config,
// samples and logs stay: weeks of samples are worth keeping.
func (in *installer) uninstall(ctx context.Context) int {
	// The binary to remove is the one the installed agent runs, unless --bin
	// names it; with neither, no binary is ours to delete.
	remove := []string{in.plistPath()}
	owned := "" // the binary the shims lead to, as install linked it
	if in.binGiven {
		owned, _ = filepath.Abs(in.bin)
	} else if p, err := os.ReadFile(in.plistPath()); err == nil {
		if bin, err := launchd.ProgramPath(p); err == nil && filepath.IsAbs(bin) {
			owned = bin
		}
	}
	// A link is not a copy install made: with --bin "$(brew --prefix)/bin/headroom"
	// it is Homebrew's, and removing it would break the formula (#181).
	kept := ""
	if fi, err := os.Lstat(owned); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		kept = owned
	} else if owned != "" {
		remove = append(remove, owned)
	}
	if err := in.agent.Unload(ctx); err != nil {
		return in.fail(err)
	}
	if in.shimDir != "" {
		bin := owned
		if bin == "" {
			bin, _ = filepath.Abs(in.bin)
		}
		// Best effort: the agent is already stopped, so finish the rest.
		// The dir goes only if it is headroom's own default, not one the
		// user chose (say ~/bin).
		own := false
		if dir, err := config.Dir(in.getenv); err == nil {
			own = in.shimDir == config.Defaults(dir).ShimDir
		}
		if err := unlinkShims(in.shimDir, bin, own); err != nil {
			fmt.Fprintf(in.errw, "headroom: removing the shims in %s: %v; remove them by hand\n", in.shimDir, err)
		}
	}
	for _, p := range remove {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return in.fail(err)
		}
	}
	fmt.Fprintln(in.out, "headroom daemon removed from launchd; kept:")
	if kept != "" {
		fmt.Fprintf(in.out, "  binary   %s (a link install did not copy)\n", kept)
	}
	if dir, err := config.Dir(in.getenv); err == nil {
		fmt.Fprintf(in.out, "  config   %s\n  samples  %s\n", dir, filepath.Join(dir, "samples"))
	}
	dir, _, _ := logPaths(in.home)
	fmt.Fprintf(in.out, "  logs     %s\n", dir)
	return 0
}

func (in *installer) fail(err error) int {
	fmt.Fprintln(in.errw, "headroom:", err)
	return 1
}

// copyBinary installs src at dst (0755) via a temp file and a rename, so a
// running agent never sees half a binary. Installing a binary over itself is
// a no-op.
func copyBinary(src, dst string) error {
	if same, _ := sameFile(src, dst); same {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".headroom-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = io.Copy(tmp, in)
	if err == nil {
		err = tmp.Chmod(0o755)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func sameFile(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

// writeFile writes data to path via a temp file and a rename.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// tail writes the last n lines of path to w, under a header; nothing if the
// file is missing or empty. It reads only the end of the file.
func tail(w io.Writer, path string, n int) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	tailFile(w, f, n)
}

// tailFile is tail on an open file. It returns the offset it read up to, where
// a follower carries on (#149).
func tailFile(w io.Writer, f *os.File, n int) int64 {
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return 0
	}
	const window = 256 << 10 // far more than n log lines
	start := max(fi.Size()-window, 0)
	buf := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return fi.Size()
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if start > 0 {
		lines = lines[1:] // the window may begin mid-line
	}
	lines = lines[max(len(lines)-n, 0):]
	fmt.Fprintf(w, "==> %s <==\n%s\n", f.Name(), strings.Join(lines, "\n"))
	return fi.Size()
}

// trimCrashLog empties the crash log launchd gives the daemon as stderr once
// it passes limit: it has no cap of its own.
func trimCrashLog(f *os.File, limit int64) {
	if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() && fi.Size() > limit {
		_ = f.Truncate(0)
		_, _ = f.Seek(0, io.SeekStart)
	}
}

// newInstaller wires an installer to the real system.
func newInstaller(e Env, bin string) (*installer, error) {
	home := e.Getenv("HOME")
	if home == "" {
		return nil, errors.New("HOME is not set")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	binGiven := bin != ""
	if !binGiven {
		bin = filepath.Join(home, ".local", "bin", "headroom")
	}
	cfg, err := config.Load(e.Getenv)
	if err != nil {
		return nil, err
	}
	return &installer{
		home: home, exe: exe, bin: bin, binGiven: binGiven,
		agent: launchd.Agent{Label: agentLabel, UID: os.Getuid(), Run: launchd.Exec{}},
		ping: func(ctx context.Context) (int, error) {
			return client.Ping(ctx, cfg.Socket, cfg.Policy.DaemonTimeout.Duration)
		},
		getenv: e.Getenv, out: e.Stdout, errw: e.Stderr,
		wait: 10 * time.Second, poll: 200 * time.Millisecond,
		shimDir:  cfg.ShimDir,
		lookPath: func(name string) bool { return binpath.Search(name, e.Getenv, nil, nil) != "" },
	}, nil
}

// runInstall runs install or uninstall with an optional --bin PATH.
func runInstall(e Env, uninstall bool) int {
	bin := ""
	for a := e.Args[2:]; len(a) > 0; a = a[1:] {
		if a[0] != "--bin" || len(a) < 2 {
			fmt.Fprintf(e.Stderr, "headroom: usage: headroom %s [--bin PATH]\n", e.Args[1])
			return 2
		}
		bin, a = a[1], a[1:]
	}
	in, err := newInstaller(e, bin)
	if err != nil {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	if uninstall {
		return in.uninstall(context.Background())
	}
	return in.install(context.Background())
}
