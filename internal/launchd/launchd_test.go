package launchd_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/launchd"
)

func TestPlist(t *testing.T) {
	got := string(launchd.Plist(launchd.Spec{
		Label:  "io.example.agent",
		Args:   []string{"/Users/dev/.local/bin/headroom", "daemon", "--log", "/Users/dev/Library/Logs/headroom/daemon.log"},
		Env:    map[string]string{"HEADROOM_CONFIG_DIR": "/Users/dev/cfg & <more>", "A": "1"},
		Stderr: "/Users/dev/Library/Logs/headroom/daemon.stderr.log",
	}))
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>io.example.agent</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Users/dev/.local/bin/headroom</string>
		<string>daemon</string>
		<string>--log</string>
		<string>/Users/dev/Library/Logs/headroom/daemon.log</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>A</key>
		<string>1</string>
		<key>HEADROOM_CONFIG_DIR</key>
		<string>/Users/dev/cfg &amp; &lt;more&gt;</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>LowPriorityIO</key>
	<true/>
	<key>Nice</key>
	<integer>5</integer>
	<key>StandardErrorPath</key>
	<string>/Users/dev/Library/Logs/headroom/daemon.stderr.log</string>
</dict>
</plist>
`
	if got != want {
		t.Fatalf("plist:\n%s\nwant:\n%s", got, want)
	}
}

func TestPlistWithoutEnv(t *testing.T) {
	got := string(launchd.Plist(launchd.Spec{Label: "l", Args: []string{"/bin/x"}, Stderr: "/tmp/e"}))
	if strings.Contains(got, "EnvironmentVariables") {
		t.Fatalf("empty env written:\n%s", got)
	}
}

// fakeLaunchctl records calls and answers from a table keyed by subcommand;
// queued answers (next) are used first, one per call.
type fakeLaunchctl struct {
	calls  []string
	answer map[string]error
	next   map[string][]error
	out    map[string]string
}

func (f *fakeLaunchctl) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	if q := f.next[args[0]]; len(q) > 0 {
		f.next[args[0]] = q[1:]
		return []byte(f.out[args[0]]), q[0]
	}
	return []byte(f.out[args[0]]), f.answer[args[0]]
}

func agent(f *fakeLaunchctl) launchd.Agent {
	return launchd.Agent{Label: "io.example.agent", UID: 501, Run: f, Poll: time.Millisecond}
}

var gone = launchd.ExitError(113, "Could not find service")

func TestLoadEnablesBeforeBootstrapping(t *testing.T) {
	// enable first: launchd refuses to bootstrap a disabled service.
	f := &fakeLaunchctl{answer: map[string]error{"bootout": launchd.ExitError(3, "Boot-out failed: 3: No such process")}}
	if err := agent(f).Load(context.Background(), "/Users/dev/Library/LaunchAgents/io.example.agent.plist"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/bin/launchctl bootout gui/501/io.example.agent",
		"/bin/launchctl enable gui/501/io.example.agent",
		"/bin/launchctl bootstrap gui/501 /Users/dev/Library/LaunchAgents/io.example.agent.plist",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
	}
}

func TestUnloadIgnoresNotLoaded(t *testing.T) {
	for _, code := range []int{3, 113} {
		f := &fakeLaunchctl{answer: map[string]error{"bootout": launchd.ExitError(code, "not there")}}
		if err := agent(f).Unload(context.Background()); err != nil {
			t.Errorf("exit %d: %v", code, err)
		}
	}
}

func TestReloadWaitsForTheOldInstanceToGo(t *testing.T) {
	// bootout returns while launchd is still tearing the service down; print
	// still finds it twice before it is gone.
	f := &fakeLaunchctl{answer: map[string]error{"print": gone},
		next: map[string][]error{"print": {nil, nil}}}
	if err := agent(f).Load(context.Background(), "/p.plist"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls, "\n")
	if strings.Count(got, " print ") != 3 || strings.Index(got, "print") > strings.Index(got, "bootstrap") {
		t.Fatalf("calls:\n%s", got)
	}
}

func TestBootstrapRetriesWhileLaunchdIsBusy(t *testing.T) {
	busy := launchd.ExitError(5, "Bootstrap failed: 5: Input/output error")
	f := &fakeLaunchctl{answer: map[string]error{"print": gone}, next: map[string][]error{"bootstrap": {busy, busy}}}
	if err := agent(f).Load(context.Background(), "/p.plist"); err != nil {
		t.Fatalf("err = %v after %d bootstraps", err, strings.Count(strings.Join(f.calls, "\n"), "bootstrap"))
	}
}

func TestPID(t *testing.T) {
	f := &fakeLaunchctl{answer: map[string]error{}, out: map[string]string{"print": "gui/501/io.example.agent = {\n\tstate = running\n\tpid = 4242\n}"}}
	if pid, err := agent(f).PID(context.Background()); pid != 4242 || err != nil {
		t.Fatalf("pid = %d, %v", pid, err)
	}
	f.out["print"] = "gui/501/io.example.agent = {\n\tstate = not running\n}"
	if pid, err := agent(f).PID(context.Background()); pid != 0 || err != nil {
		t.Fatalf("not running: pid = %d, %v", pid, err)
	}
	f.answer["print"] = gone
	if pid, err := agent(f).PID(context.Background()); pid != 0 || err != nil {
		t.Fatalf("not loaded: pid = %d, %v", pid, err)
	}
}

func TestProgramPath(t *testing.T) {
	p := launchd.Plist(launchd.Spec{Label: "l", Args: []string{"/opt/tools/a & b", "daemon"}, Stderr: "/e"})
	if got, err := launchd.ProgramPath(p); got != "/opt/tools/a & b" || err != nil {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := launchd.ProgramPath([]byte("<plist><dict></dict></plist>")); err == nil {
		t.Fatal("no ProgramArguments: want an error")
	}
}

func TestFailuresCarryLaunchctlsMessage(t *testing.T) {
	f := &fakeLaunchctl{answer: map[string]error{"print": gone, "bootstrap": launchd.ExitError(5, "Bootstrap failed: 5: Input/output error")}}
	err := agent(f).Load(context.Background(), "/p.plist")
	if err == nil || !strings.Contains(err.Error(), "Input/output error") || !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("err = %v", err)
	}
	f = &fakeLaunchctl{answer: map[string]error{"bootout": errors.New("exec: launchctl not found")}}
	if err := agent(f).Unload(context.Background()); err == nil {
		t.Fatal("an exec failure is not 'not loaded'")
	}
}

func TestLoaded(t *testing.T) {
	f := &fakeLaunchctl{answer: map[string]error{}, next: map[string][]error{}}
	if ok, err := agent(f).Loaded(context.Background()); !ok || err != nil {
		t.Fatalf("loaded = %v, %v", ok, err)
	}
	f.answer["print"] = launchd.ExitError(113, `Could not find service`)
	if ok, err := agent(f).Loaded(context.Background()); ok || err != nil {
		t.Fatalf("loaded = %v, %v", ok, err)
	}
	if got := f.calls[0]; got != "/bin/launchctl print gui/501/io.example.agent" {
		t.Fatalf("call %q", got)
	}
}

var _ = fmt.Sprint
