package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cybagard/cyba-headroom/internal/client"
	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/shim"
)

const (
	// shimCheckedVar marks a checked call with the shim's PID, which the
	// real binary keeps. A binary that replaces itself with another shimmed
	// one (podman-docker's `exec podman`) has the same PID and is not
	// checked, and leased, twice; a child it starts has another PID and is.
	shimCheckedVar = "HEADROOM_SHIM_CHECKED"
	// waitPoll is how often a waiting call (BUDGET_WAIT=1) asks again.
	waitPoll = 2 * time.Second
)

// errCannotCheck means the daemon answered but gave no decision; a
// daemonError carries what it said.
var errCannotCheck = errors.New("the daemon cannot check calls")

// daemonError is a daemon's answer without a decision.
type daemonError struct{ said string }

func (e daemonError) Error() string { return errCannotCheck.Error() + " (" + e.said + ")" }
func (e daemonError) Unwrap() error { return errCannotCheck }

// olderDaemon is the answer of a daemon built before the check (#24).
const olderDaemon = `unknown op "check"`

// gated is how gate decided.
type gated struct {
	proceed bool
	code    int       // the exit code when not proceeding
	signal  os.Signal // the signal that ended a wait, to die of
	checked bool      // a daemon allowed the call: mark it for later shims
	lease   string    // the allow's lease, to release if the exec fails
	cfg     config.Config
}

// withDefaults is e with each unset test hook set to the real thing.
func (e Env) withDefaults() Env {
	if e.exec == nil {
		e.exec = syscall.Exec
	}
	if e.ask == nil {
		e.ask = askDaemon
	}
	if e.release == nil {
		e.release = releaseLease
	}
	if e.ancestors == nil {
		e.ancestors = shim.Ancestors
	}
	if e.getwd == nil {
		e.getwd = os.Getwd
	}
	if e.fallbacks == nil {
		e.fallbacks = shim.Fallbacks
	}
	if e.status == nil {
		e.status = daemonStatus
	}
	if e.loginShell == nil {
		e.loginShell = askLoginShell
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.raise == nil {
		e.raise = reraise
	}
	return e // wait stays nil until a wait starts: see gate
}

// gate asks the daemon whether call c may start (R5, #28). Calls that start
// nothing, calls already checked and calls to a remote engine are not asked
// about; nor, failing open (R7), is anything when the config or the daemon
// cannot answer.
func gate(e Env, name string, c shim.Call, getenv func(string) string) gated {
	if c.Kind == "" || getenv(shimCheckedVar) == shim.Self() ||
		(c.Kind != "tart" && shim.Remote(name, c.Endpoint, getenv)) {
		return gated{proceed: true}
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		notGated(e, "config: "+oneLine(err), c.Command)
		return gated{proceed: true}
	}
	h := e.withDefaults()
	req := callerRequest(getenv, h.ancestors, h.getwd)
	req.Kind, req.Command, req.CostBytes = c.Kind, c.Command, c.MemoryBytes
	req.Target, req.Name = c.Target, c.Name
	// A run or create carries its lease as a label: see runShim.
	req.Labelled = c.Kind == "container" && (c.Op == "run" || c.Op == "create")
	if c.Kind == "tart" {
		// The VM's own memory, and whether it takes a macOS slot (R6).
		if macOS, mem, ok := shim.TartVM(c.Target, getenv); ok {
			req.MacOS, req.CostBytes = macOS, mem
		} else {
			// Unknown: count it as macOS rather than let a third one by.
			req.MacOS, req.VMUnknown = true, true
			if getenv("HEADROOM_SHIM_DEBUG") != "" {
				fmt.Fprintf(e.Stderr, "headroom: no config for the VM in `%s` under TART_HOME: counted as a macOS VM\n", c.Command)
			}
		}
		// This process becomes tart run: when it exits before its VM
		// appears, the daemon ends the lease (#29).
		req.PID = os.Getpid()
	}
	wait := shim.IsTrue(getenv("BUDGET_WAIT"))
	var deadline time.Time
	for {
		d, err := h.ask(cfg, req)
		if h.wait != nil {
			// A Ctrl-C during the ask cancels the call, even one now allowed.
			if sig := h.wait(0); sig != nil {
				if err == nil && d.Allow && d.LeaseID != "" {
					_ = h.release(cfg, d.LeaseID)
				}
				return interrupted(sig)
			}
		}
		switch {
		case err != nil:
			// Also while waiting: a daemon that went away must not hold
			// the call (R7).
			notGated(e, "daemon "+daemonCause(err, cfg.Policy.DaemonTimeout.Duration, cfg.Socket), c.Command)
			return gated{proceed: true}
		case d.Allow:
			if getenv("HEADROOM_SHIM_DEBUG") != "" {
				if d.Worktree == "" {
					fmt.Fprintln(e.Stderr, "headroom: allowed as a manual call")
				} else {
					fmt.Fprintf(e.Stderr, "headroom: allowed for %s (by %s)\n", d.Worktree, d.IdentifiedBy)
				}
			}
			return gated{proceed: true, checked: true, lease: d.LeaseID, cfg: cfg}
		case !wait || !d.Retry:
			fmt.Fprintln(e.Stderr, d.Message)
			fmt.Fprintln(e.Stderr, denyHint(d))
			return gated{code: exitDenied}
		case deadline.IsZero():
			deadline = h.now().Add(cfg.Policy.WaitTimeout.Duration)
			if h.wait == nil {
				// One registration for the whole wait, asks included.
				w := newSignalWait()
				defer w.stop()
				h.wait = w.sleep
			}
			fmt.Fprintf(e.Stderr, "headroom: waiting for room for `%s`%s, up to %s (BUDGET_WAIT)\n",
				c.Command, why(d), cfg.Policy.WaitTimeout.Duration)
		case !h.now().Before(deadline):
			fmt.Fprintln(e.Stderr, d.Message)
			fmt.Fprintf(e.Stderr, "headroom: gave up after waiting %s (policy.wait_timeout)\n", cfg.Policy.WaitTimeout.Duration)
			return gated{code: exitDenied}
		}
		if sig := h.wait(waitPoll); sig != nil {
			return interrupted(sig)
		}
	}
}

// interrupted is a wait ended by sig: exit as the shell reports it.
func interrupted(sig os.Signal) gated {
	code := 128
	if n, ok := sig.(syscall.Signal); ok {
		code += int(n)
	}
	return gated{code: code, signal: sig}
}

// callerRequest is a check request carrying who is calling: the worktree
// from the environment (HEADROOM_WORKTREE, else ORCA_WORKTREE_ID), else
// the working directory and the parent PIDs for the daemon to match.
func callerRequest(getenv func(string) string, ancestors func() []int, getwd func() (string, error)) protocol.CheckRequest {
	var r protocol.CheckRequest
	if r.Worktree = getenv("HEADROOM_WORKTREE"); r.Worktree == "" {
		r.Worktree = getenv("ORCA_WORKTREE_ID")
	}
	if r.Worktree != "" {
		return r
	}
	r.Cwd, _ = getwd()
	if resolved, err := filepath.EvalSymlinks(r.Cwd); err == nil && resolved != r.Cwd {
		r.RealCwd = resolved
	}
	r.Ancestors = ancestors()
	return r
}

// notGated warns, in one line, that a call runs without a check (R7).
func notGated(e Env, cause, command string) {
	fmt.Fprintf(e.Stderr, "headroom: not gated (%s); running `%s` anyway. See: headroom status\n", cause, command)
}

// daemonCause says briefly why the daemon at socket gave no decision.
func daemonCause(err error, timeout time.Duration, socket string) string {
	var ne net.Error
	var de daemonError
	switch {
	case errors.Is(err, client.ErrVersion), errors.As(err, &de) && de.said == olderDaemon:
		return "is another version: restart it with this build: headroom install"
	case errors.As(err, &de) && de.said == "":
		return "gave no decision; restart it with this build: headroom install"
	case errors.As(err, &de):
		return "gave no decision (" + strings.Join(strings.Fields(de.said), " ") + "); restart it with this build: headroom install"
	case errors.Is(err, client.ErrBadReply):
		return "gave a bad reply"
	case errors.As(err, &ne) && ne.Timeout(): // deadlines of conn and context alike
		return fmt.Sprintf("gave no answer in %s", timeout)
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return "socket not accessible"
	}
	if fi, statErr := os.Lstat(socket); statErr == nil && fi.Mode()&os.ModeSocket == 0 {
		return "socket path " + socket + " is not a socket"
	}
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOTSOCK) {
		return "not running"
	}
	return "unreachable: " + oneLine(err)
}

// oneLine is err's text on one line.
func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

// askDaemon sends one check to the daemon. errCannotCheck means it
// answered without a decision.
func askDaemon(cfg config.Config, req protocol.CheckRequest) (*protocol.Decision, error) {
	rep, err := client.Do(context.Background(), cfg.Socket, cfg.Policy.DaemonTimeout.Duration,
		protocol.Request{Op: protocol.OpCheck, Check: &req})
	switch {
	case err != nil && rep.Error != "":
		return nil, daemonError{said: rep.Error}
	case err != nil:
		return nil, err
	case rep.Decision == nil:
		return nil, daemonError{}
	}
	return rep.Decision, nil
}

// releaseLease hands back the lease of a call that did not start.
func releaseLease(cfg config.Config, id string) error {
	_, err := client.Do(context.Background(), cfg.Socket, cfg.Policy.DaemonTimeout.Duration,
		protocol.Request{Op: protocol.OpRelease, Release: id})
	return err
}

// signalWait sleeps between asks unless SIGINT or SIGTERM comes. A signal
// the shim was started ignoring (a background job's SIGINT) stays ignored.
type signalWait struct{ ch chan os.Signal }

func newSignalWait() *signalWait {
	w := &signalWait{ch: make(chan os.Signal, 1)}
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM} {
		if !signal.Ignored(s) {
			signal.Notify(w.ch, s)
		}
	}
	return w
}

func (w *signalWait) stop() { signal.Stop(w.ch) }

// sleep waits d; it returns the signal that cut it short, or nil. With d
// 0 it only reports a signal already pending.
func (w *signalWait) sleep(d time.Duration) os.Signal {
	if d == 0 {
		select {
		case s := <-w.ch:
			return s
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case s := <-w.ch:
		return s
	}
}

// reraise dies of sig with its default action, so the parent sees the
// signal and not just an exit code.
func reraise(sig os.Signal) {
	if n, ok := sig.(syscall.Signal); ok {
		signal.Reset(n)
		_ = syscall.Kill(os.Getpid(), n)
		time.Sleep(100 * time.Millisecond) // delivery is asynchronous
	}
}

// denyHint tells an agent what to do about a deny.
func denyHint(d *protocol.Decision) string {
	if d.Retry {
		if len(d.Reasons) > 0 && d.Reasons[0].Code == policy.VMSlots {
			return "headroom: retry later, or run it with BUDGET_WAIT=1 to wait for a slot"
		}
		return "headroom: retry later, or run it with BUDGET_WAIT=1 to wait for room"
	}
	// Speak to the first reason waiting cannot fix.
	for _, r := range d.Reasons {
		if !r.Retry {
			if r.Code == policy.VMSlots {
				return "headroom: waiting will not help: set budget.max_macos_vms in headroom's config to allow macOS VMs"
			}
			break
		}
	}
	return "headroom: waiting will not help: reuse or stop what this worktree holds, or ask for less memory"
}

// why is the first reason for a deny, as " (reason)"; "" if none is given.
func why(d *protocol.Decision) string {
	if len(d.Reasons) == 0 {
		return ""
	}
	return " (" + d.Reasons[0].Text + ")"
}
