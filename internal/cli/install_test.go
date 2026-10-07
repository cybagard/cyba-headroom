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
	calls    []string
	loadErr  error
	onLoad   func() // e.g. bring the fake daemon up
	unloaded bool
}

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
	f.loaded, f.unloaded = false, true
	return nil
}

func (f *fakeAgent) Loaded(context.Context) (bool, error) { return f.loaded, nil }

type installFixture struct {
	in      *installer
	agent   *fakeAgent
	home    string
	up      bool
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
	f.agent = &fakeAgent{onLoad: func() { f.up = true }}
	f.in = &installer{
		home: home, exe: exe, bin: filepath.Join(home, ".local", "bin", "headroom"),
		agent: f.agent,
		ping: func(context.Context) error {
			if f.up {
				return nil
			}
			return errors.New("connection refused")
		},
		getenv: func(k string) string {
			return map[string]string{"HEADROOM_CONFIG_DIR": "/Users/dev/cfg", "HOME": home}[k]
		},
		out: &f.out, errw: &f.errb, wait: 200 * time.Millisecond, poll: 5 * time.Millisecond,
	}
	return f
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
	f.up = false
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
	f.up = false
	f.in.exe = f.in.bin // running ~/.local/bin/headroom install
	if code := f.in.install(context.Background()); code != 0 {
		t.Fatalf("exit %d: %s", code, f.errb.String())
	}
	if b, _ := os.ReadFile(f.in.bin); !bytes.Equal(b, f.exeData) {
		t.Fatalf("binary clobbered: %q", b)
	}
}

func TestInstallRefusesAManualDaemon(t *testing.T) {
	f := newInstallFixture(t)
	f.up = true // something answers, but launchd has no agent
	if code := f.in.install(context.Background()); code != 1 || !strings.Contains(f.errb.String(), "already running") {
		t.Fatalf("exit %d: %s", code, f.errb.String())
	}
	if len(f.agent.calls) != 0 {
		t.Errorf("touched launchd: %v", f.agent.calls)
	}
}

func TestInstallShowsLogsWhenTheDaemonDoesNotComeUp(t *testing.T) {
	f := newInstallFixture(t)
	f.agent.onLoad = nil // loads, but never answers
	logs := filepath.Join(f.home, "Library", "Logs", "headroom")
	_ = os.MkdirAll(logs, 0o700)
	_ = os.WriteFile(filepath.Join(logs, "daemon.stderr.log"), []byte("panic: boom\n"), 0o600)
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
