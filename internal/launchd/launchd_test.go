package launchd_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

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

// fakeLaunchctl records calls and answers from a table keyed by subcommand.
type fakeLaunchctl struct {
	calls  []string
	answer map[string]error
}

func (f *fakeLaunchctl) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	return nil, f.answer[args[0]]
}

func agent(f *fakeLaunchctl) launchd.Agent {
	return launchd.Agent{Label: "io.example.agent", UID: 501, Run: f}
}

func TestLoadBootsOutThenBootstrapsAndEnables(t *testing.T) {
	f := &fakeLaunchctl{answer: map[string]error{"bootout": launchd.ExitError(3, "Boot-out failed: 3: No such process")}}
	if err := agent(f).Load(context.Background(), "/Users/dev/Library/LaunchAgents/io.example.agent.plist"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/bin/launchctl bootout gui/501/io.example.agent",
		"/bin/launchctl bootstrap gui/501 /Users/dev/Library/LaunchAgents/io.example.agent.plist",
		"/bin/launchctl enable gui/501/io.example.agent",
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

func TestFailuresCarryLaunchctlsMessage(t *testing.T) {
	f := &fakeLaunchctl{answer: map[string]error{"bootstrap": launchd.ExitError(5, "Bootstrap failed: 5: Input/output error")}}
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
	f := &fakeLaunchctl{answer: map[string]error{}}
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
