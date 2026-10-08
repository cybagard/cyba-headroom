package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// exitDenied is the shim's exit code for a denied call: 75, EX_TEMPFAIL (R5).
const exitDenied = 75

// runCheck asks the daemon's policy (#24) whether a call may go ahead:
// headroom check [--worktree ID] [--cost 2G] [--kind container] -- cmd...
// It exits 0 on allow and 75 on deny.
func runCheck(e Env) int {
	req := protocol.CheckRequest{Kind: "container"}
	a := e.Args[2:]
	for len(a) > 0 && a[0] != "--" {
		if len(a) < 2 {
			return checkUsage(e)
		}
		switch a[0] {
		case "--worktree":
			req.Worktree = a[1]
		case "--kind":
			if a[1] != "container" && a[1] != "compose" && a[1] != "tart" {
				return checkUsage(e)
			}
			req.Kind = a[1]
		case "--cost":
			b, err := parseBytes(a[1])
			if err != nil {
				fmt.Fprintf(e.Stderr, "headroom: check: --cost %q: %v\n", a[1], err)
				return 2
			}
			req.CostBytes = b
		default:
			return checkUsage(e)
		}
		a = a[2:]
	}
	if len(a) < 2 {
		return checkUsage(e) // no -- or nothing after it
	}
	req.Command = strings.Join(a[1:], " ")
	if req.Worktree == "" {
		// Identify the caller as the shim does.
		h := e.withDefaults()
		who := callerRequest(e.Getenv, h.ancestors, h.getwd)
		req.Worktree, req.Cwd, req.RealCwd, req.Ancestors = who.Worktree, who.Cwd, who.RealCwd, who.Ancestors
	}

	cfg, err := config.Load(e.Getenv)
	if err != nil {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	d, err := askDaemon(cfg, req)
	if err != nil {
		// The shim's diagnosis; only a daemon not running needs starting.
		if cause := daemonCause(err, cfg.Policy.DaemonTimeout.Duration, cfg.Socket); cause != "not running" {
			fmt.Fprintf(e.Stderr, "headroom: the daemon %s\n", cause)
		} else {
			unreachable(e, cfg.Socket, err)
		}
		return 1
	}
	// A deny goes to stderr, where an agent looks for why a command failed.
	if !d.Allow {
		fmt.Fprintln(e.Stderr, d.Message)
		return exitDenied
	}
	fmt.Fprintln(e.Stdout, d.Message)
	return 0
}

func checkUsage(e Env) int {
	fmt.Fprintln(e.Stderr, "headroom: usage: headroom check [--worktree ID] [--cost 2G] [--kind container|compose|tart] -- command...")
	return 2
}

// parseBytes reads a size: 512M, 2G, 1.5GB, 6gib, or a plain number of
// GB. M and G mean MiB and GiB, as everywhere in headroom.
func parseBytes(s string) (uint64, error) {
	unit := float64(1 << 30)
	lower := strings.ToLower(s)
	for _, u := range []struct {
		suffix string
		bytes  float64
	}{{"mib", 1 << 20}, {"mb", 1 << 20}, {"m", 1 << 20}, {"gib", 1 << 30}, {"gb", 1 << 30}, {"g", 1 << 30}} {
		if n, ok := strings.CutSuffix(lower, u.suffix); ok {
			lower, unit = n, u.bytes
			break
		}
	}
	v, err := strconv.ParseFloat(lower, 64)
	b := v * unit
	if err != nil || !(b >= 1<<20) || b > 1024*float64(1<<30) {
		return 0, errors.New("want e.g. 512M or 2G, between 1M and 1024G")
	}
	return uint64(b), nil
}
