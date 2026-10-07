package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	Loaded(ctx context.Context) (bool, error)
}

// installer installs and removes the daemon's LaunchAgent.
type installer struct {
	home  string
	exe   string // the running binary
	bin   string // where the agent's copy goes
	agent agentControl
	// ping reports whether a daemon answers on the socket.
	ping   func(context.Context) error
	getenv func(string) string
	out    io.Writer
	errw   io.Writer
	// How long install waits for the daemon to answer, and how often it asks.
	wait, poll time.Duration
}

func (in *installer) plistPath() string {
	return filepath.Join(in.home, "Library", "LaunchAgents", agentLabel+".plist")
}

func (in *installer) logDir() string { return filepath.Join(in.home, "Library", "Logs", "headroom") }

// install copies the binary, writes the plist, (re)loads the agent and waits
// for the daemon to answer.
func (in *installer) install(ctx context.Context) int {
	loaded, err := in.agent.Loaded(ctx)
	if err != nil {
		return in.fail(err)
	}
	if !loaded && in.ping(ctx) == nil {
		fmt.Fprintln(in.errw, "headroom: a daemon is already running outside launchd (`headroom daemon` in a terminal?); stop it, then run install again")
		return 1
	}
	if err := copyBinary(in.exe, in.bin); err != nil {
		return in.fail(fmt.Errorf("installing the binary: %w", err))
	}
	if err := os.MkdirAll(in.logDir(), 0o700); err != nil {
		return in.fail(err)
	}
	// launchd would create the crash log world-readable; create it private.
	crash, err := os.OpenFile(filepath.Join(in.logDir(), "daemon.stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return in.fail(err)
	}
	_ = crash.Close()
	_ = os.Chmod(crash.Name(), 0o600)
	env := map[string]string{}
	for _, k := range installEnv {
		env[k] = in.getenv(k)
	}
	plist := launchd.Plist(launchd.Spec{
		Label:  agentLabel,
		Args:   []string{in.bin, "daemon", "--log", filepath.Join(in.logDir(), "daemon.log")},
		Env:    env,
		Stderr: filepath.Join(in.logDir(), "daemon.stderr.log"),
	})
	if err := writeFile(in.plistPath(), plist, 0o644); err != nil {
		return in.fail(fmt.Errorf("writing the agent: %w", err))
	}
	if err := in.agent.Load(ctx, in.plistPath()); err != nil {
		return in.fail(err)
	}
	if !in.waitUp(ctx) {
		fmt.Fprintf(in.errw, "headroom: the daemon did not answer within %s; its logs:\n", in.wait)
		for _, name := range []string{"daemon.log", "daemon.stderr.log"} {
			tail(in.errw, filepath.Join(in.logDir(), name), 20)
		}
		return 1
	}
	fmt.Fprintf(in.out, "headroom daemon running under launchd (%s)\n  binary  %s\n  agent   %s\n  log     %s (headroom logs)\n",
		agentLabel, in.bin, in.plistPath(), filepath.Join(in.logDir(), "daemon.log"))
	return 0
}

func (in *installer) waitUp(ctx context.Context) bool {
	deadline := time.Now().Add(in.wait)
	for {
		if in.ping(ctx) == nil {
			return true
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

// uninstall stops the agent and removes it and the installed binary. Config,
// samples and logs stay: weeks of samples are worth keeping.
func (in *installer) uninstall(ctx context.Context) int {
	if err := in.agent.Unload(ctx); err != nil {
		return in.fail(err)
	}
	for _, p := range []string{in.plistPath(), in.bin} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return in.fail(err)
		}
	}
	fmt.Fprintln(in.out, "headroom daemon removed from launchd; kept:")
	if dir, err := config.Dir(in.getenv); err == nil {
		fmt.Fprintf(in.out, "  config   %s\n  samples  %s\n", dir, filepath.Join(dir, "samples"))
	}
	fmt.Fprintf(in.out, "  logs     %s\n", in.logDir())
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
// file is missing or empty.
func tail(w io.Writer, path string, n int) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(w, "==> %s <==\n%s\n", path, strings.Join(lines, "\n"))
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
	if bin == "" {
		bin = filepath.Join(home, ".local", "bin", "headroom")
	}
	cfg, err := config.Load(e.Getenv)
	if err != nil {
		return nil, err
	}
	return &installer{
		home: home, exe: exe, bin: bin,
		agent: launchd.Agent{Label: agentLabel, UID: os.Getuid(), Run: launchd.Exec{}},
		ping: func(ctx context.Context) error {
			_, err := client.Status(ctx, cfg.Socket, cfg.Policy.DaemonTimeout.Duration)
			return err
		},
		getenv: e.Getenv, out: e.Stdout, errw: e.Stderr,
		wait: 10 * time.Second, poll: 200 * time.Millisecond,
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
