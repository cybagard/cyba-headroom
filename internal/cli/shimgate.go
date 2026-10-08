package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/cybagard/cyba-headroom/internal/client"
	"github.com/cybagard/cyba-headroom/internal/config"
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

// errCannotCheck means the daemon answered but cannot check calls: most
// likely an older build still running after an upgrade.
var errCannotCheck = errors.New("the daemon cannot check calls")

// gated is how gate decided.
type gated struct {
	proceed bool
	code    int    // the exit code when not proceeding
	checked bool   // a daemon allowed the call: mark it for later shims
	lease   string // the allow's lease, to release if the exec fails
	cfg     config.Config
}

// gate asks the daemon whether call c may start (R5, #28). Calls that start
// nothing, calls already checked and calls to a remote engine are not asked
// about; nor, failing open (R7), is anything when the config or the daemon
// cannot answer.
func gate(e Env, name string, c shim.Call, getenv func(string) string) gated {
	if c.Kind == "" || getenv(shimCheckedVar) == strconv.Itoa(os.Getpid()) ||
		(c.Kind != "tart" && shim.Remote(name, c.Endpoint, getenv)) {
		return gated{proceed: true}
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		fmt.Fprintf(e.Stderr, "headroom: %v; `%s` not gated\n", err, c.Command)
		return gated{proceed: true}
	}
	ask, now, sleep := e.ask, e.now, e.sleep
	if ask == nil {
		ask = askDaemon
	}
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = sleepUnlessSignalled
	}
	req := callerRequest(getenv, e.ancestors)
	req.Kind, req.Command, req.CostBytes = c.Kind, c.Command, c.MemoryBytes
	wait := shim.IsTrue(getenv("BUDGET_WAIT"))
	var deadline time.Time
	for {
		d, err := ask(cfg, req)
		switch {
		case errors.Is(err, errCannotCheck):
			fmt.Fprintf(e.Stderr, "headroom: %v; restart it with this build: headroom install. `%s` not gated\n", err, c.Command)
			return gated{proceed: true}
		case err != nil:
			// Also while waiting: a daemon that went away must not hold
			// the call (R7).
			fmt.Fprintf(e.Stderr, "headroom: daemon not reachable (%v); `%s` not gated\n", err, c.Command)
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
			deadline = now().Add(cfg.Policy.WaitTimeout.Duration)
			fmt.Fprintf(e.Stderr, "headroom: waiting for room for `%s`%s, up to %s (BUDGET_WAIT)\n",
				c.Command, why(d), cfg.Policy.WaitTimeout.Duration)
		case !now().Before(deadline):
			fmt.Fprintln(e.Stderr, d.Message)
			fmt.Fprintf(e.Stderr, "headroom: gave up after waiting %s (policy.wait_timeout)\n", cfg.Policy.WaitTimeout.Duration)
			return gated{code: exitDenied}
		}
		if code := sleep(waitPoll); code != 0 {
			return gated{code: code}
		}
	}
}

// callerRequest is a check request carrying who is calling: the worktree
// from the environment (HEADROOM_WORKTREE, else ORCA_WORKTREE_ID), else
// the working directory and the parent PIDs for the daemon to match.
func callerRequest(getenv func(string) string, ancestors func() []int) protocol.CheckRequest {
	var r protocol.CheckRequest
	if r.Worktree = getenv("HEADROOM_WORKTREE"); r.Worktree == "" {
		r.Worktree = getenv("ORCA_WORKTREE_ID")
	}
	if r.Worktree != "" {
		return r
	}
	if ancestors == nil {
		ancestors = shim.Ancestors
	}
	r.Cwd, _ = os.Getwd()
	if resolved, err := filepath.EvalSymlinks(r.Cwd); err == nil && resolved != r.Cwd {
		r.RealCwd = resolved
	}
	r.Ancestors = ancestors()
	return r
}

// askDaemon sends one check to the daemon. errCannotCheck means it
// answered without a decision.
func askDaemon(cfg config.Config, req protocol.CheckRequest) (*protocol.Decision, error) {
	rep, err := client.Do(context.Background(), cfg.Socket, cfg.Policy.DaemonTimeout.Duration,
		protocol.Request{Op: protocol.OpCheck, Check: &req})
	switch {
	case err != nil && rep.Error != "":
		return nil, fmt.Errorf("%w (%s)", errCannotCheck, rep.Error)
	case err != nil:
		return nil, err
	case rep.Decision == nil:
		return nil, fmt.Errorf("%w (no decision in its reply)", errCannotCheck)
	}
	return rep.Decision, nil
}

// releaseLease hands back the lease of a call that did not start.
func releaseLease(cfg config.Config, id string) error {
	_, err := client.Do(context.Background(), cfg.Socket, cfg.Policy.DaemonTimeout.Duration,
		protocol.Request{Op: protocol.OpRelease, Release: id})
	return err
}

// sleepUnlessSignalled sleeps for d. A SIGINT or SIGTERM first ends it with
// the shell's exit code for that signal (130, 143); one the shim was started
// ignoring (a background job's SIGINT) stays ignored.
func sleepUnlessSignalled(d time.Duration) int {
	var sigs []os.Signal
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM} {
		if !signal.Ignored(s) {
			sigs = append(sigs, s)
		}
	}
	ch := make(chan os.Signal, 1)
	if len(sigs) > 0 {
		signal.Notify(ch, sigs...)
		defer signal.Stop(ch)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return 0
	case s := <-ch:
		return 128 + int(s.(syscall.Signal))
	}
}

// denyHint tells an agent what to do about a deny.
func denyHint(d *protocol.Decision) string {
	if d.Retry {
		return "headroom: retry later, or run it with BUDGET_WAIT=1 to wait for room"
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
