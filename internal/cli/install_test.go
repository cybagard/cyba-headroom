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
	for _, want := range []string{f.in.bin + " run -- claude", f.in.bin + " run -- kilo", "Orca"} {
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
	// The links go; the dir stays, as it is not the default one.
	if _, err := os.Lstat(filepath.Join(f.in.shimDir, "docker")); !os.IsNotExist(err) {
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

// A shim that cannot be removed does not stop the uninstall halfway.
func TestUninstallGoesOnWhenAShimStays(t *testing.T) {
	f := newInstallFixture(t)
	f.in.install(context.Background())
	if err := os.Chmod(f.in.shimDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.in.shimDir, 0o755) })
	if os.Geteuid() == 0 {
		t.Skip("root removes whatever the mode")
	}
	code := f.in.uninstall(context.Background())
	if _, err := os.Stat(f.plistPath()); code != 0 || !os.IsNotExist(err) || !strings.Contains(f.errb.String(), "shims") {
		t.Fatalf("exit %d, plist %v, stderr %q", code, err, f.errb.String())
	}
}

// A config dir set for install is carried into the printed Orca commands,
// so the agents' shims find the same config and socket.
func TestInstallPrintsTheConfigDir(t *testing.T) {
	f := newInstallFixture(t)
	f.in.install(context.Background())
	if !strings.Contains(f.out.String(), "claude: env HEADROOM_CONFIG_DIR=/Users/dev/cfg "+f.in.bin+" run -- claude") {
		t.Fatalf("output:\n%s", f.out.String())
	}
}

// --bin with a relative path and another name: uninstall still removes the
// links install made.
func TestUninstallRemovesLinksToARelativeBin(t *testing.T) {
	f := newInstallFixture(t)
	t.Chdir(f.home)
	f.in.bin, f.in.binGiven = filepath.Join("tools", "hr"), true
	if code := f.in.install(context.Background()); code != 0 {
		t.Fatalf("install: %s", f.errb.String())
	}
	if code := f.in.uninstall(context.Background()); code != 0 {
		t.Fatalf("uninstall: %s", f.errb.String())
	}
	// The links go; the dir stays, as it is not the default one.
	if _, err := os.Lstat(filepath.Join(f.in.shimDir, "docker")); !os.IsNotExist(err) {
		t.Fatalf("shims stayed: %v", err)
	}
}

// brewLayout makes a fake Homebrew prefix in a short temp dir: the formula's
// binary in the Cellar, and prefix/bin/headroom linking to it (#181). It
// returns the link and the binary it resolves to.
func brewLayout(t *testing.T, data []byte) (link, target string) {
	t.Helper()
	prefix, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prefix) })
	target = filepath.Join(prefix, "Cellar", "headroom", "0.1.0", "bin", "headroom")
	link = filepath.Join(prefix, "bin", "headroom")
	for _, d := range []string{filepath.Dir(target), filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(target, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "Cellar", "headroom", "0.1.0", "bin", "headroom"), link); err != nil {
		t.Fatal(err)
	}
	return link, target
}

// newBrewFixture is `$(brew --prefix)/bin/headroom install --bin <link>`:
// the running binary is the Cellar one the link resolves to.
func newBrewFixture(t *testing.T) (f *installFixture, link, target string) {
	t.Helper()
	f = newInstallFixture(t)
	link, target = brewLayout(t, f.exeData)
	f.in.exe, f.in.bin, f.in.binGiven = target, link, true
	return f, link, target
}

// A Homebrew install makes no copy: the agent runs the stable link, which
// survives `brew upgrade` (#181).
func TestInstallWithABrewLinkMakesNoCopy(t *testing.T) {
	f, link, target := newBrewFixture(t)
	if code := f.in.install(context.Background()); code != 0 {
		t.Fatalf("exit %d: %s", code, f.errb.String())
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is no longer a link: %v %v", link, fi, err)
	}
	if b, err := os.ReadFile(target); err != nil || !bytes.Equal(b, f.exeData) {
		t.Fatalf("Cellar binary: %q %v", b, err)
	}
	if p, _ := os.ReadFile(f.plistPath()); !strings.Contains(string(p), "<string>"+link+"</string>") {
		t.Fatalf("the agent does not run %s:\n%s", link, p)
	}
	if got, err := os.Readlink(filepath.Join(f.in.shimDir, "docker")); err != nil || got != link {
		t.Fatalf("docker -> %q, %v", got, err)
	}
}

// Uninstall removes only a binary install copied: Homebrew's link and the
// formula's binary stay; the agent and the shims go (#181).
func TestUninstallKeepsABrewLink(t *testing.T) {
	for name, binGiven := range map[string]bool{"with --bin": true, "without --bin": false} {
		t.Run(name, func(t *testing.T) {
			f, link, target := newBrewFixture(t)
			if code := f.in.install(context.Background()); code != 0 {
				t.Fatalf("install: exit %d: %s", code, f.errb.String())
			}
			if !binGiven {
				f.in.bin, f.in.binGiven = filepath.Join(f.home, ".local", "bin", "headroom"), false
			}
			f.out.Reset()
			if code := f.in.uninstall(context.Background()); code != 0 {
				t.Fatalf("uninstall: exit %d: %s", code, f.errb.String())
			}
			if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Errorf("link removed: %v %v", fi, err)
			}
			if _, err := os.Stat(target); err != nil {
				t.Errorf("Cellar binary removed: %v", err)
			}
			if _, err := os.Stat(f.plistPath()); !os.IsNotExist(err) {
				t.Errorf("plist left: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(f.in.shimDir, "docker")); !os.IsNotExist(err) {
				t.Errorf("shims stayed: %v", err)
			}
			if !strings.Contains(f.out.String(), link) {
				t.Errorf("output does not say %s was kept:\n%s", link, f.out.String())
			}
		})
	}
}

// Uninstall lists what it removed (the LaunchAgent, the binary, the shims),
// then what it kept; a second run removed nothing, so it lists nothing (#192).
func TestUninstallSaysWhatItRemoved(t *testing.T) {
	f := newInstallFixture(t)
	if code := f.in.install(context.Background()); code != 0 {
		t.Fatalf("install: exit %d: %s", code, f.errb.String())
	}
	removed := []string{f.plistPath(), f.in.bin, f.in.shimDir}
	for run, want := range [][]string{removed, nil} {
		f.out.Reset()
		if code := f.in.uninstall(context.Background()); code != 0 {
			t.Fatalf("run %d: exit %d: %s", run, code, f.errb.String())
		}
		before, after, ok := strings.Cut(f.out.String(), "kept:")
		if !ok || !strings.Contains(after, "/Users/dev/cfg") {
			t.Fatalf("run %d: no kept list:\n%s", run, f.out.String())
		}
		for _, p := range removed {
			if listed := strings.Contains(before, p); listed != (want != nil) {
				t.Errorf("run %d: %s listed as removed: %v, want %v:\n%s", run, p, listed, want != nil, f.out.String())
			}
		}
	}
}

// A link install did not copy is listed as kept, not removed (#192).
func TestUninstallListsABrewLinkAsKept(t *testing.T) {
	f, link, _ := newBrewFixture(t)
	if code := f.in.install(context.Background()); code != 0 {
		t.Fatalf("install: exit %d: %s", code, f.errb.String())
	}
	f.out.Reset()
	if code := f.in.uninstall(context.Background()); code != 0 {
		t.Fatalf("uninstall: exit %d: %s", code, f.errb.String())
	}
	before, after, _ := strings.Cut(f.out.String(), "kept:")
	if strings.Contains(before, link) || !strings.Contains(after, link) {
		t.Errorf("%s not listed as kept only:\n%s", link, f.out.String())
	}
	if !strings.Contains(before, f.plistPath()) || !strings.Contains(before, f.in.shimDir) {
		t.Errorf("LaunchAgent or shims not listed as removed:\n%s", f.out.String())
	}
}

// A plain install copies the binary to ~/.local/bin; uninstall run from
// that copy, as it is once installed, still removes it (#181).
func TestUninstallFromTheCopyRemovesIt(t *testing.T) {
	f := newInstallFixture(t)
	if code := f.in.install(context.Background()); code != 0 {
		t.Fatalf("install: exit %d: %s", code, f.errb.String())
	}
	f.in.exe = f.in.bin // `~/.local/bin/headroom uninstall`
	if code := f.in.uninstall(context.Background()); code != 0 {
		t.Fatalf("uninstall: exit %d: %s", code, f.errb.String())
	}
	if _, err := os.Lstat(filepath.Join(f.home, ".local", "bin", "headroom")); !os.IsNotExist(err) {
		t.Fatalf("copy left: %v", err)
	}
}

// brewPlistNames are the names Homebrew gives headroom's LaunchAgent: its
// current one and its legacy one, which it still honours (#185).
var brewPlistNames = []string{"sh.brew.headroom.plist", "homebrew.mxcl.headroom.plist"}

// With Homebrew's LaunchAgent in place, install would start a second
// daemon: it refuses and changes nothing (#185).
func TestInstallRefusesWhenHomebrewRunsTheDaemon(t *testing.T) {
	for _, name := range brewPlistNames {
		t.Run(name, func(t *testing.T) {
			f := newInstallFixture(t)
			brew := filepath.Join(f.home, "Library", "LaunchAgents", name)
			if err := os.MkdirAll(filepath.Dir(brew), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(brew, []byte("<plist/>"), 0o644); err != nil {
				t.Fatal(err)
			}
			if code := f.in.install(context.Background()); code != 1 || !strings.Contains(f.errb.String(), "brew services") {
				t.Fatalf("exit %d: %s", code, f.errb.String())
			}
			for _, p := range []string{f.in.bin, f.plistPath(), f.in.shimDir} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Errorf("%s written: %v", p, err)
				}
			}
			if len(f.agent.calls) != 0 {
				t.Errorf("touched launchd: %v", f.agent.calls)
			}
		})
	}
}

// install --bin with Homebrew's link, run from an older copy, would replace
// the link with that copy: it refuses, changes nothing, and names the
// binary to run (#183).
func TestInstallRefusesALinkToAnotherBinary(t *testing.T) {
	f, link, target := newBrewFixture(t)
	f.in.exe = filepath.Join(f.home, ".local", "bin", "headroom") // an older copy
	if err := os.MkdirAll(filepath.Dir(f.in.exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.in.exe, []byte("#!binary v0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := f.in.install(context.Background()); code != 1 {
		t.Fatalf("exit %d: %s", code, f.errb.String())
	}
	if want := `"` + link + `" install --bin "` + link + `"`; !strings.Contains(f.errb.String(), want) {
		t.Errorf("message lacks %s:\n%s", want, f.errb.String())
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is no longer a link: %v %v", link, fi, err)
	}
	if b, err := os.ReadFile(target); err != nil || !bytes.Equal(b, f.exeData) {
		t.Errorf("Cellar binary: %q %v", b, err)
	}
	for _, p := range []string{f.plistPath(), f.in.shimDir} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s written: %v", p, err)
		}
	}
	if len(f.agent.calls) != 0 {
		t.Errorf("touched launchd: %v", f.agent.calls)
	}
}

// writeBrewPlist puts Homebrew's LaunchAgent for headroom in the fixture's
// home under name, as `brew services start headroom` does.
func (f *installFixture) writeBrewPlist(t *testing.T, name string) {
	t.Helper()
	brew := filepath.Join(f.home, "Library", "LaunchAgents", name)
	if err := os.MkdirAll(filepath.Dir(brew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brew, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// When only Homebrew runs the daemon, uninstall would remove the formula's
// shims and leave its agent running: it refuses and changes nothing (#186).
func TestUninstallRefusesWhenOnlyHomebrewRunsTheDaemon(t *testing.T) {
	for _, name := range brewPlistNames {
		t.Run(name, func(t *testing.T) {
			f, link, target := newBrewFixture(t)
			if _, err := linkShims(f.in.shimDir, link); err != nil {
				t.Fatal(err)
			}
			f.writeBrewPlist(t, name)
			if code := f.in.uninstall(context.Background()); code != 1 || !strings.Contains(f.errb.String(), "brew services stop headroom") {
				t.Fatalf("exit %d: %s", code, f.errb.String())
			}
			// homebrew/cask has a headroom too: the bare name makes brew
			// warn "Treating headroom as a formula" (#192).
			if msg := f.errb.String(); !strings.Contains(msg, "`brew uninstall cybagard/tap/headroom`") || strings.Contains(msg, "`brew uninstall headroom`") {
				t.Errorf("refusal does not name the tap:\n%s", msg)
			}
			for _, n := range []string{"docker", "podman", "tart"} {
				if got, err := os.Readlink(filepath.Join(f.in.shimDir, n)); err != nil || got != link {
					t.Errorf("%s -> %q, %v", n, got, err)
				}
			}
			if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Errorf("link removed: %v %v", fi, err)
			}
			if _, err := os.Stat(target); err != nil {
				t.Errorf("Cellar binary removed: %v", err)
			}
			if len(f.agent.calls) != 0 {
				t.Errorf("touched launchd: %v", f.agent.calls)
			}
		})
	}
}

// With both agents, uninstall removes headroom's, as doctor advises (#186).
func TestUninstallWithBothAgentsRemovesHeadrooms(t *testing.T) {
	for _, name := range brewPlistNames {
		t.Run(name, func(t *testing.T) {
			f := newInstallFixture(t)
			if code := f.in.install(context.Background()); code != 0 {
				t.Fatalf("install: exit %d: %s", code, f.errb.String())
			}
			f.writeBrewPlist(t, name)
			if code := f.in.uninstall(context.Background()); code != 0 {
				t.Fatalf("uninstall: exit %d: %s", code, f.errb.String())
			}
			if _, err := os.Stat(f.plistPath()); !os.IsNotExist(err) {
				t.Errorf("plist left: %v", err)
			}
			if !f.agent.unloaded {
				t.Error("agent not unloaded")
			}
		})
	}
}

// A dangling link at --bin is refused as broken, with its target named and
// no command that would run it (#183).
func TestInstallRefusesABrokenLink(t *testing.T) {
	f := newInstallFixture(t)
	missing := filepath.Join(f.home, "Cellar", "headroom", "0.0.9", "bin", "headroom")
	f.in.bin, f.in.binGiven = filepath.Join(f.home, "opt", "bin", "headroom"), true
	if err := os.MkdirAll(filepath.Dir(f.in.bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(missing, f.in.bin); err != nil {
		t.Fatal(err)
	}
	if code := f.in.install(context.Background()); code != 1 {
		t.Fatalf("exit %d: %s", code, f.errb.String())
	}
	msg := f.errb.String()
	if !strings.Contains(msg, "broken") || !strings.Contains(msg, missing) {
		t.Errorf("message does not call the link broken or name %s:\n%s", missing, msg)
	}
	if strings.Contains(msg, "install --bin") {
		t.Errorf("message suggests running the broken link:\n%s", msg)
	}
	if got, err := os.Readlink(f.in.bin); err != nil || got != missing {
		t.Fatalf("%s -> %q, %v", f.in.bin, got, err)
	}
	for _, p := range []string{f.plistPath(), f.in.shimDir} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s written: %v", p, err)
		}
	}
	if len(f.agent.calls) != 0 {
		t.Errorf("touched launchd: %v", f.agent.calls)
	}
}

// A plain install still replaces a link at the default path with a copy:
// the refusal is for --bin only (#183).
func TestPlainInstallReplacesALinkWithACopy(t *testing.T) {
	f := newInstallFixture(t)
	other := filepath.Join(t.TempDir(), "headroom") // a dev build
	if err := os.WriteFile(other, []byte("#!binary dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.in.bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, f.in.bin); err != nil {
		t.Fatal(err)
	}
	if code := f.in.install(context.Background()); code != 0 {
		t.Fatalf("exit %d: %s", code, f.errb.String())
	}
	if fi, err := os.Lstat(f.in.bin); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("%s is not a copy: %v %v", f.in.bin, fi, err)
	}
	if b, err := os.ReadFile(f.in.bin); err != nil || !bytes.Equal(b, f.exeData) {
		t.Errorf("installed binary: %q %v", b, err)
	}
	if b, err := os.ReadFile(other); err != nil || string(b) != "#!binary dev" {
		t.Errorf("the dev build changed: %q %v", b, err)
	}
}
