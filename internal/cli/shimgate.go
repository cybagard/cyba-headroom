package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/cybagard/cyba-headroom/internal/client"
	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/shim"
)

const (
	// shimCheckedVar marks a call already checked, so a real binary that
	// runs another shimmed one (podman-docker's `exec podman`) is not
	// checked, and leased, twice.
	shimCheckedVar = "HEADROOM_SHIM_CHECKED"
	// waitPoll is how often a waiting call (BUDGET_WAIT=1) asks again.
	waitPoll = 2 * time.Second
	// exitInterrupted is the shell's code for a call stopped by Ctrl-C.
	exitInterrupted = 130
)

// gate asks the daemon whether call c may start (R5, #28). It returns
// proceed with the marker to pass the real binary, or the exit code. Calls
// that start nothing, calls already checked and calls to a remote engine
// are not asked about; nor, failing open (R7), is anything when the config
// or the daemon cannot answer.
func gate(e Env, c shim.Call, getenv func(string) string) (marker string, code int, proceed bool) {
	if c.Kind == "" || getenv(shimCheckedVar) != "" || (c.Kind != "tart" && shim.Remote(c.Endpoint, getenv)) {
		return "", 0, true
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		fmt.Fprintf(e.Stderr, "headroom: %v; `%s` not gated\n", err, c.Command)
		return "", 0, true
	}
	worktree := getenv("HEADROOM_WORKTREE")
	if worktree == "" {
		worktree = getenv("ORCA_WORKTREE_ID")
	}
	cwd, _ := os.Getwd()
	ask, ancestors, now, sleep := e.ask, e.ancestors, e.now, e.sleep
	if ask == nil {
		ask = askDaemon
	}
	if ancestors == nil {
		ancestors = shim.Ancestors
	}
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = sleepUnlessInterrupted
	}
	req := protocol.CheckRequest{Worktree: worktree, Kind: c.Kind, Command: c.Command, CostBytes: c.MemoryBytes,
		Cwd: cwd, Ancestors: ancestors()}
	wait := isTrue(getenv("BUDGET_WAIT"))
	var deadline time.Time
	for {
		d, err := ask(cfg, req)
		switch {
		case err != nil:
			fmt.Fprintf(e.Stderr, "headroom: daemon not reachable (%v); `%s` not gated\n", err, c.Command)
			return "", 0, true
		case d.Allow:
			if getenv("HEADROOM_SHIM_DEBUG") != "" {
				if d.Worktree == "" {
					fmt.Fprintln(e.Stderr, "headroom: allowed as a manual call")
				} else {
					fmt.Fprintf(e.Stderr, "headroom: allowed for %s (by %s)\n", d.Worktree, d.IdentifiedBy)
				}
			}
			if d.LeaseID != "" {
				return d.LeaseID, 0, true
			}
			return "allowed", 0, true
		case !wait || !d.Retry:
			fmt.Fprintln(e.Stderr, d.Message)
			fmt.Fprintln(e.Stderr, denyHint(d))
			return "", exitDenied, false
		case deadline.IsZero():
			deadline = now().Add(cfg.Policy.WaitTimeout.Duration)
			fmt.Fprintf(e.Stderr, "headroom: waiting for room for `%s`%s, up to %s (BUDGET_WAIT)\n",
				c.Command, why(d), cfg.Policy.WaitTimeout.Duration)
		case !now().Before(deadline):
			fmt.Fprintln(e.Stderr, d.Message)
			fmt.Fprintf(e.Stderr, "headroom: gave up after waiting %s (policy.wait_timeout)\n", cfg.Policy.WaitTimeout.Duration)
			return "", exitDenied, false
		}
		if !sleep(waitPoll) {
			return "", exitInterrupted, false
		}
	}
}

// askDaemon sends one check to the daemon.
func askDaemon(cfg config.Config, req protocol.CheckRequest) (*protocol.Decision, error) {
	rep, err := client.Do(context.Background(), cfg.Socket, cfg.Policy.DaemonTimeout.Duration,
		protocol.Request{Op: protocol.OpCheck, Check: &req})
	if err != nil {
		return nil, err
	}
	if rep.Decision == nil {
		return nil, errors.New("the daemon replied without a decision")
	}
	return rep.Decision, nil
}

// sleepUnlessInterrupted sleeps for d; false if Ctrl-C or SIGTERM came first.
func sleepUnlessInterrupted(d time.Duration) bool {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-sig:
		return false
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

// isTrue reads a boolean environment variable: 1, true, ...
func isTrue(v string) bool {
	b, err := strconv.ParseBool(v)
	return err == nil && b
}
