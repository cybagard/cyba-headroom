// Package launchd installs the daemon as a per-user LaunchAgent (#22): it
// writes the property list and loads it into the user's GUI domain with
// launchctl.
package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

const launchctl = "/bin/launchctl"

// Spec describes the agent.
type Spec struct {
	Label string
	// Args is the program and its arguments; Args[0] must be absolute.
	Args []string
	// Env is set for the program; empty values are left out.
	Env map[string]string
	// Stderr catches what the program writes to stderr: crash traces.
	Stderr string
}

// Plist renders s as a LaunchAgent property list. The agent starts at login
// and restarts if it crashes, but stays stopped after a clean exit. It runs
// at background priority: the daemon does a little work every few seconds.
func Plist(s Spec) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	key := func(k string) { fmt.Fprintf(&b, "\t<key>%s</key>\n", k) }
	str := func(indent, v string) { fmt.Fprintf(&b, "%s<string>%s</string>\n", indent, esc(v)) }

	key("Label")
	str("\t", s.Label)
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range s.Args {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	var names []string
	for k, v := range s.Env {
		if v != "" {
			names = append(names, k)
		}
	}
	if len(names) > 0 {
		slices.Sort(names)
		key("EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		for _, k := range names {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n", esc(k))
			str("\t\t", s.Env[k])
		}
		b.WriteString("\t</dict>\n")
	}
	b.WriteString(`	<key>RunAtLoad</key>
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
`)
	key("StandardErrorPath")
	str("\t", s.Stderr)
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Runner runs a command and returns its stdout. A non-zero exit is an
// *Exit error carrying the code and stderr.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Exit is a command's non-zero exit.
type Exit struct {
	Code   int
	Stderr string
}

func (e *Exit) Error() string { return fmt.Sprintf("exit %d: %s", e.Code, e.Stderr) }

// ExitError returns an *Exit, for Runner implementations and tests.
func ExitError(code int, stderr string) error { return &Exit{Code: code, Stderr: stderr} }

// Exec runs commands for real.
type Exec struct{}

// Run implements Runner.
func (Exec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, ExitError(ee.ExitCode(), strings.TrimSpace(stderr.String()))
	}
	return out, err
}

// Agent is one LaunchAgent in a user's GUI domain.
type Agent struct {
	Label string
	UID   int
	Run   Runner
}

func (a Agent) domain() string  { return fmt.Sprintf("gui/%d", a.UID) }
func (a Agent) service() string { return a.domain() + "/" + a.Label }

func (a Agent) launchctl(ctx context.Context, args ...string) error {
	if _, err := a.Run.Run(ctx, launchctl, args...); err != nil {
		return fmt.Errorf("launchctl %s: %w", args[0], err)
	}
	return nil
}

// notLoaded reports launchctl's "no such service" answers: 3 (No such
// process) and 113 (Could not find specified service).
func notLoaded(err error) bool {
	var e *Exit
	return errors.As(err, &e) && (e.Code == 3 || e.Code == 113)
}

// Load (re)loads the agent from plist: an old instance is booted out first,
// so a reinstall picks up a new binary and plist.
func (a Agent) Load(ctx context.Context, plist string) error {
	if err := a.Unload(ctx); err != nil {
		return err
	}
	if err := a.launchctl(ctx, "bootstrap", a.domain(), plist); err != nil {
		return err
	}
	return a.launchctl(ctx, "enable", a.service())
}

// Unload stops the agent and removes it from the domain. An agent that is
// not loaded is fine.
func (a Agent) Unload(ctx context.Context) error {
	if err := a.launchctl(ctx, "bootout", a.service()); err != nil && !notLoaded(err) {
		return err
	}
	return nil
}

// Loaded reports whether the agent is in the domain.
func (a Agent) Loaded(ctx context.Context) (bool, error) {
	err := a.launchctl(ctx, "print", a.service())
	switch {
	case err == nil:
		return true, nil
	case notLoaded(err):
		return false, nil
	}
	return false, err
}
