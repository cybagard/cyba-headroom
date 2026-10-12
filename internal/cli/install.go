package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
	// socket is the daemon's; its lock file is beside it.
	socket string
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
	// A plain install still replaces a link at the default path.
	if fi, err := os.Lstat(in.bin); in.binGiven && err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		if _, err := os.Stat(in.bin); err != nil {
			target, _ := os.Readlink(in.bin)
			// The stat error would name the target again (#194).
			fmt.Fprintf(in.errw, "headroom: %s is a broken link to %s; remove it or choose another --bin\n", in.bin, target)
			return 1
		}
		if same, _ := sameFile(in.bin, in.exe); !same {
			fmt.Fprintf(in.errw, "headroom: %s links to another headroom binary; to install the binary it links to, run it: \"%s\" install --bin \"%s\"\n", in.bin, in.bin, in.bin)
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
	prefix := in.configPrefix()
	in.removeOldCopy(bin, prefix, in.installShims(bin, prefix))
	in.notePath(filepath.Dir(bin))
	return 0
}

// notePath says how to put dir on PATH, as the README does for zsh, if it
// is not on install's own (#194).
func (in *installer) notePath(dir string) {
	if onPath(in.getenv("PATH"), dir) {
		return
	}
	entry := dir
	if rel, err := filepath.Rel(in.home, dir); err == nil && filepath.IsLocal(rel) {
		entry = "$HOME/" + rel
	}
	fmt.Fprintf(in.out, "\n%s is not on your PATH. For zsh, run this and then open a new terminal:\n  echo 'export PATH=\"%s:$PATH\"' >> ~/.zprofile\n", dir, entry)
}

// defaultBin is where a plain install, without --bin, copies the binary.
func defaultBin(home string) string { return filepath.Join(home, ".local", "bin", "headroom") }

// removeOldCopy removes the copy a plain install left at the default path
// once install --bin chose another: first on PATH, it would shadow bin
// (#194). Anything else there stays, with a note. An Orca agent command
// that ran it must change: to the lines installShims printed, if it did
// (#219).
func (in *installer) removeOldCopy(bin, prefix string, printed bool) {
	old := defaultBin(in.home)
	if !in.binGiven || old == bin {
		return
	}
	fi, err := os.Lstat(old)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err == nil && fi.Mode().IsRegular() && isHeadroom(old, old, in.exe, bin) {
		if same, _ := sameFile(old, bin); same {
			return
		}
		if err := os.Remove(old); err != nil {
			fmt.Fprintf(in.errw, "headroom: removing the old copy %s: %v; it may shadow %s on PATH\n", old, err, bin)
			return
		}
		use := "use the lines above"
		if !printed {
			use = "now run " + prefix + shellWord(bin) + " run -- <agent>"
		}
		fmt.Fprintf(in.out, "removed: old copy %s (it would shadow %s on PATH); Orca agent commands that run %s must %s\n", old, bin, old, use)
		return
	}
	fmt.Fprintf(in.errw, "headroom: %s is not a copy headroom install made: left as it is; it may shadow %s on PATH\n", old, bin)
}

// orcaAgents are the agent CLIs whose Orca launch command install prints.
var orcaAgents = []string{"claude", "codex", "kilo", "opencode", "gemini", "goose", "amp"}

// installShims links the shims to bin and says how to launch Orca's agents
// through them (R9, #31), each command starting with prefix. A failure is
// reported, not fatal: the daemon runs. It reports whether it printed the
// commands.
func (in *installer) installShims(bin, prefix string) bool {
	if in.shimDir == "" {
		return false
	}
	notes, err := linkShims(in.shimDir, bin)
	for _, n := range notes {
		fmt.Fprintln(in.errw, "headroom:", n)
	}
	if err != nil {
		fmt.Fprintf(in.errw, "headroom: linking the shims in %s: %v\n", in.shimDir, err)
		return false
	}
	fmt.Fprintf(in.out, "  shims   %s (docker, podman, tart)\n", in.shimDir)
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
	return len(lines) > 0
}

// configPrefix is what an agent's command starts with so that its shims
// find install's config dir, if that is not the default.
func (in *installer) configPrefix() string {
	if in.getenv("HEADROOM_CONFIG_DIR") == "" && in.getenv("XDG_CONFIG_HOME") == "" {
		return ""
	}
	dir, err := config.Dir(in.getenv)
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	return "env HEADROOM_CONFIG_DIR=" + shellWord(abs) + " "
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
	// Homebrew's agent alone: removing its shims would leave its daemon
	// running ungated (#186). With both, ours goes, as doctor advises.
	if brew := brewPlist(in.home); brew != "" {
		if _, err := os.Stat(in.plistPath()); errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(in.errw, "headroom: Homebrew runs the daemon (%s); remove it with `brew services stop headroom` and `brew uninstall cybagard/tap/headroom`, not headroom uninstall\n", brew)
			return 1
		}
	}
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
	var shims, brewShims []string // the shim links removed, and Homebrew's kept
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
		// With both agents, Homebrew's daemon may already hold the lock and
		// have linked the shims to its own path, also while ours stopped
		// (--wait): those links stay, or it would run ungated (#192).
		both := brewPlist(in.home) != ""
		var err error
		if shims, brewShims, err = unlinkShims(in.shimDir, bin, own, both); err != nil {
			fmt.Fprintf(in.errw, "headroom: removing the shims in %s: %v; remove them by hand\n", in.shimDir, err)
		}
	}
	var gone []string // the paths in remove that were there
	for _, p := range remove {
		err := os.Remove(p)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return in.fail(err)
		}
		if err == nil {
			gone = append(gone, p)
		}
	}
	cfgDir, cfgErr := config.Dir(in.getenv)
	lock := in.socket + ".lock"
	if in.socket != "" {
		// Ours is unloaded, so nothing of ours can be refused again; the
		// marker goes, held lock or not (#194).
		_ = os.Remove(lock + ".reported")
		// With both agents, Homebrew's --wait daemon may have the lock
		// file open to take it: unlinking it could leave two daemons
		// each holding a lock.
		if brewPlist(in.home) == "" {
			removeLock(lock, cfgDir)
		}
	}
	fmt.Fprintln(in.out, "headroom daemon removed from launchd.")
	if len(gone) > 0 || len(shims) > 0 {
		fmt.Fprintln(in.out, "removed:")
	}
	for _, p := range gone {
		what := "binary"
		if p == in.plistPath() {
			what = "agent"
		}
		fmt.Fprintf(in.out, "  %-8s %s\n", what, p)
	}
	if len(shims) > 0 {
		fmt.Fprintf(in.out, "  shims    %s (%s)\n", in.shimDir, strings.Join(shims, ", "))
	}
	fmt.Fprintln(in.out, "kept:")
	if kept != "" {
		fmt.Fprintf(in.out, "  binary   %s (a link install did not copy)\n", kept)
	}
	if len(brewShims) > 0 {
		whose := "another headroom's"
		if brewPlist(in.home) != "" {
			whose = "Homebrew's"
		}
		fmt.Fprintf(in.out, "  shims    %s (%s), %s\n", in.shimDir, strings.Join(brewShims, ", "), whose)
	}
	// The lock and its marker alone are not config worth listing (#194).
	if entries, err := os.ReadDir(cfgDir); cfgErr == nil && err == nil &&
		slices.ContainsFunc(entries, func(e fs.DirEntry) bool {
			p := filepath.Join(cfgDir, e.Name())
			return p != lock && p != lock+".reported"
		}) {
		fmt.Fprintf(in.out, "  config   %s\n  samples  %s\n", cfgDir, filepath.Join(cfgDir, "samples"))
	}
	dir, _, _ := logPaths(in.home)
	fmt.Fprintf(in.out, "  logs     %s\n", dir)
	return 0
}

// removeLock removes the daemon's lock file at path unless a daemon holds
// it, as Homebrew's does in the both state, then dir if that was its last
// entry (#194).
func removeLock(path, dir string) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return
	}
	if err := os.Remove(path); err == nil && filepath.Dir(path) == filepath.Clean(dir) {
		_ = os.Remove(dir) // only if empty
	}
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
		bin = defaultBin(home)
	}
	cfg, err := config.Load(e.Getenv)
	if err != nil {
		return nil, err
	}
	return &installer{
		home: home, exe: exe, bin: bin, binGiven: binGiven,
		agent:  launchd.Agent{Label: agentLabel, UID: os.Getuid(), Run: launchd.Exec{}},
		socket: cfg.Socket,
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
