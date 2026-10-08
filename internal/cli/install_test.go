package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAgent stands in for launchd.Agent.
type fakeAgent struct {
	loaded   bool
	pid      int // the launchd daemon's PID; 0 = not running
	calls    []string
	loadErr  error
	onLoad   func() // e.g. bring the fake daemon up
	unloaded bool
}

func (f *fakeAgent) PID(context.Context) (int, error) { return f.pid, nil }

func (f *fakeAgent) Load(_ context.Context, plist string) error {
	f.calls = append(f.calls, "load "+plist)
	if f.loadErr != nil {
		return f.loadErr
	}
	f.loaded = true
	if f.onLoad != nil {
		f.onLoad()
	}
	return nil
}

func (f *fakeAgent) Unload(context.Context) error {
	f.calls = append(f.calls, "unload")
	f.loaded, f.unloaded, f.pid = false, true, 0
	return nil
}

func (f *fakeAgent) Loaded(context.Context) (bool, error) { return f.loaded, nil }

type installFixture struct {
	in      *installer
	agent   *fakeAgent
	home    string
	upPID   int // PID of whatever answers on the socket; 0 = nothing
	out     bytes.Buffer
	errb    bytes.Buffer
	exeData []byte
}

func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	home := t.TempDir()
	f := &installFixture{home: home, exeData: []byte("#!binary v1")}
	exe := filepath.Join(t.TempDir(), "headroom")
	if err := os.WriteFile(exe, f.exeData, 0o755); err != nil {
		t.Fatal(err)
	}
	f.agent = &fakeAgent{}
	f.agent.onLoad = func() { f.agent.pid = 4242; f.upPID = 4242 }
	f.in = &installer{
		home: home, exe: exe, bin: filepath.Join(home, ".local", "bin", "headroom"),
		agent: f.agent,
		ping: func(context.Context) (int, error) {
			switch {
			case f.upPID == -1:
				return 0, nil // an older daemon: answers without a PID
			case f.upPID != 0:
				return f.upPID, nil
			}
			return 0, errors.New("connection refused")
		},
		getenv: func(k string) string {
			return map[string]string{"HEADROOM_CONFIG_DIR": "/Users/dev/cfg", "HOME": home}[k]
		},
		out: &f.out, errw: &f.errb, wait: 200 * time.Millisecond, poll: 5 * time.Millisecond,
		shimDir:  filepath.Join(home, "shims"),
		lookPath: func(name string) bool { return name == "claude" || name == "kilo" },
	}
	return f
}

// Install links the shims to the installed binary and says how to launch
// Orca's agents through them; uninstall removes the links (#31).
func TestInstallLinksTheShims(t *testing.T) {
	f := newInstallFixture(t)
	if code := f.in.install(context.Background()); code != 0 {
		t.Fatalf("exit %d: %s", code, f.errb.String())
	}
	if target, err := os.Readlink(filepath.Join(f.in.shimDir, "docker")); err != nil || target != f.in.bin {
		t.Fatalf("docker -> %q, %v", target, err)
	}
	out := f.out.String()
	for _, want := range []string{"claude: " + f.in.bin + " run -- claude", "kilo: " + f.in.bin + " run -- kilo", "Orca"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "codex:") {
		t.Errorf("an agent not on PATH is listed:\n%s", out)
	}
	if code := f.in.uninstall(context.Background()); code != 0 {
		t.Fatalf("uninstall: %d %s", code, f.errb.String())
	}
	if _, err := os.Stat(f.in.shimDir); !os.IsNotExist(err) {
		t.Fatalf("shims stayed: %v", err)
	}
}

func (f *installFixture) plistPath() string {
	return filepath.Join(f.home, "Library", "LaunchAgents", agentLabel+".plist")
}

func TestInstallCopiesBinaryWritesPlistAndLoads(t *testing.T) {
	f := newInstallFixture(t)
	if code := f.in.install(context.Background()); code != 0 {
		t.Fatalf("exit %d: %s", code, f.errb.String())
	}
	b, err := os.ReadFile(f.in.bin)
	if err != nil || !bytes.Equal(b, f.exeData) {
		t.Fatalf("binary not copied: %v", err)
	}
	if fi, _ := os.Stat(f.in.bin); fi.Mode().Perm() != 0o755 {
		t.Errorf("binary mode %v", fi.Mode().Perm())
	}
	p, err := os.ReadFile(f.plistPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<string>" + f.in.bin + "</string>", "<string>daemon</string>", "<string>--log</string>",
		filepath.Join(f.home, "Library", "Logs", "headroom", "daemon.log"),
		"<key>HEADROOM_CONFIG_DIR</key>", "daemon.stderr.log",
	} {
		if !strings.Contains(string(p), want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	if fi, _ := os.Stat(f.plistPath()); fi.Mode().Perm() != 0o644 {
		t.Errorf("plist mode %v", fi.Mode().Perm())
	}
	if fi, err := os.Stat(filepath.Join(f.home, "Library", "Logs", "headroom")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("log dir: %v %v", fi, err)
	}
	// launchd would create the crash log 0644; install creates it first.
	if fi, err := os.Stat(filepath.Join(f.home, "Library", "Logs", "headroom", "daemon.stderr.log")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("crash log: %v %v", fi, err)
	}
	if len(f.agent.calls) != 1 || f.agent.calls[0] != "load "+f.plistPath() {
		t.Errorf("calls %v", f.agent.calls)
	}
	for _, want := range []string{f.in.bin, f.plistPath(), "daemon.log", "running"} {
		if !strings.Contains(f.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, f.out.String())
		}
	}
}

func TestReinstallReplacesTheBinary(t *testing.T) {
	f := newInstallFixture(t)
	f.in.install(context.Background())
	if err := os.WriteFile(f.in.exe, []byte("#!binary v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := f.in.install(context.Background()); code != 0 {
		t.Fatalf("exit %d: %s", code, f.errb.String())
	}
	if b, _ := os.ReadFile(f.in.bin); string(b) != "#!binary v2" {
		t.Fatalf("binary = %q", b)
	}
}

func TestInstallFromTheInstalledBinary(t *testing.T) {
	f := newInstallFixture(t)
	f.in.install(context.Background())
	f.in.exe = f.in.bin // running ~/.local/bin/headroom install
	if code := f.in.install(context.Background()); code != 0 {
		t.Fatalf("exit %d: %s", code, f.errb.String())
	}
	if b, _ := os.ReadFile(f.in.bin); !bytes.Equal(b, f.exeData) {
		t.Fatalf("binary clobbered: %q", b)
	}
}

func TestInstallRefusesAManualDaemon(t *testing.T) {
	for name, setup := range map[string]func(*installFixture){
		"no agent":            func(f *installFixture) { f.upPID = 999 },
		"agent crash-looping": func(f *installFixture) { f.upPID, f.agent.loaded, f.agent.pid = 999, true, 0 },
	} {
		t.Run(name, func(t *testing.T) {
			f := newInstallFixture(t)
			setup(f)
			if code := f.in.install(context.Background()); code != 1 || !strings.Contains(f.errb.String(), "pid 999") {
				t.Fatalf("exit %d: %s", code, f.errb.String())
			}
			if len(f.agent.calls) != 0 {
				t.Errorf("touched launchd: %v", f.agent.calls)
			}
		})
	}
}

func TestInstallWaitsForTheLaunchdDaemonItself(t *testing.T) {
	f := newInstallFixture(t)
	// The agent loads, but what answers is another process.
	f.agent.onLoad = func() { f.agent.pid = 4242; f.upPID = 999 }
	if code := f.in.install(context.Background()); code != 1 {
		t.Fatalf("exit %d: want a failure, the launchd daemon never answered", code)
	}
}

func TestInstallMakesPathsAbsolute(t *testing.T) {
	f := newInstallFixture(t)
	dir := t.TempDir()
	t.Chdir(dir)
	f.in.bin = "rel/headroom"
	f.in.getenv = func(k string) string {
		return map[string]string{"HEADROOM_CONFIG_DIR": "cfg", "HOME": f.home}[k]
	}
	if code := f.in.install(context.Background()); code != 0 {
		t.Fatalf("exit %d: %s", code, f.errb.String())
	}
	p, _ := os.ReadFile(f.plistPath())
	for _, want := range []string{"<string>" + filepath.Join(dir, "rel", "headroom") + "</string>", "<string>" + filepath.Join(dir, "cfg") + "</string>"} {
		if !strings.Contains(string(p), want) {
			t.Errorf("plist lacks %q", want)
		}
	}
}

func TestInstallStartsAFreshCrashLog(t *testing.T) {
	f := newInstallFixture(t)
	crash := filepath.Join(f.home, "Library", "Logs", "headroom", "daemon.stderr.log")
	_ = os.MkdirAll(filepath.Dir(crash), 0o700)
	_ = os.WriteFile(crash, []byte("old panic from last week\n"), 0o644)
	f.in.install(context.Background())
	if b, _ := os.ReadFile(crash); len(b) != 0 {
		t.Fatalf("crash log kept %q", b)
	}
}

func TestUninstallRemovesTheInstalledBinary(t *testing.T) {
	f := newInstallFixture(t)
	f.in.bin = filepath.Join(f.home, "tools", "headroom")
	f.in.install(context.Background())
	other := filepath.Join(f.home, ".local", "bin", "headroom") // not ours
	_ = os.MkdirAll(filepath.Dir(other), 0o755)
	_ = os.WriteFile(other, []byte("hand-built"), 0o755)
	f.in.bin, f.in.binGiven = other, false // `headroom uninstall` with no --bin
	if code := f.in.uninstall(context.Background()); code != 0 {
		t.Fatalf("exit %d: %s", code, f.errb.String())
	}
	if _, err := os.Stat(filepath.Join(f.home, "tools", "headroom")); !os.IsNotExist(err) {
		t.Errorf("installed binary left: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("deleted a binary it did not install: %v", err)
	}
}

func TestUninstallWithoutInstallRemovesNothing(t *testing.T) {
	f := newInstallFixture(t)
	_ = os.MkdirAll(filepath.Dir(f.in.bin), 0o755)
	_ = os.WriteFile(f.in.bin, []byte("hand-built"), 0o755)
	if code := f.in.uninstall(context.Background()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(f.in.bin); err != nil {
		t.Fatalf("deleted %s with no agent installed: %v", f.in.bin, err)
	}
}

func TestInstallShowsLogsWhenTheDaemonDoesNotComeUp(t *testing.T) {
	f := newInstallFixture(t)
	// The new daemon crashes at once: it never answers, and leaves a trace.
	f.agent.onLoad = func() {
		crash := filepath.Join(f.home, "Library", "Logs", "headroom", "daemon.stderr.log")
		_ = os.WriteFile(crash, []byte("panic: boom\n"), 0o600)
	}
	if code := f.in.install(context.Background()); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(f.errb.String(), "panic: boom") || !strings.Contains(f.errb.String(), "did not answer") {
		t.Fatalf("stderr = %s", f.errb.String())
	}
}

func TestUninstallKeepsDataAndIsIdempotent(t *testing.T) {
	f := newInstallFixture(t)
	f.in.install(context.Background())
	for i := 0; i < 2; i++ {
		f.out.Reset()
		if code := f.in.uninstall(context.Background()); code != 0 {
			t.Fatalf("run %d: exit %d: %s", i, code, f.errb.String())
		}
		if _, err := os.Stat(f.plistPath()); !os.IsNotExist(err) {
			t.Fatalf("plist left: %v", err)
		}
		if _, err := os.Stat(f.in.bin); !os.IsNotExist(err) {
			t.Fatalf("binary left: %v", err)
		}
	}
	if !f.agent.unloaded {
		t.Error("agent not unloaded")
	}
	for _, want := range []string{"kept", "/Users/dev/cfg", "Logs/headroom"} {
		if !strings.Contains(f.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, f.out.String())
		}
	}
}

func TestUpgradeFromADaemonThatReportsNoPID(t *testing.T) {
	// Daemons from before #22 answer ping without a PID.
	f := newInstallFixture(t)
	f.upPID = -1 // answers, PID unknown (0 on the wire)
	f.agent.loaded, f.agent.pid = true, 4000
	if code := f.in.install(context.Background()); code != 0 {
		t.Fatalf("exit %d: %s", code, f.errb.String())
	}
}

func TestOldDaemonWithoutAnAgentIsStillRefused(t *testing.T) {
	f := newInstallFixture(t)
	f.upPID = -1
	if code := f.in.install(context.Background()); code != 1 {
		t.Fatalf("exit %d: want a refusal", code)
	}
}
