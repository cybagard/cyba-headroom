package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cybagard/cyba-headroom/internal/client"
	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// exitDenied is the shim's exit code for a denied call: 75, EX_TEMPFAIL (R5).
const exitDenied = 75

// runCheck asks the daemon's policy (#24) whether a call may go ahead:
// headroom check [--worktree ID] [--cost 2G] [--kind container] -- cmd...
// It exits 0 on allow and 75 on deny.
func runCheck(e Env) int {
	req := protocol.CheckRequest{Kind: "container", Worktree: e.Getenv("HEADROOM_WORKTREE")}
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

	cfg, err := config.Load(e.Getenv)
	if err != nil {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	rep, err := client.Do(context.Background(), cfg.Socket, cfg.Policy.DaemonTimeout.Duration,
		protocol.Request{Op: protocol.OpCheck, Check: &req})
	if err != nil {
		unreachable(e, cfg.Socket, err)
		return 1
	}
	fmt.Fprintln(e.Stdout, rep.Decision.Message)
	if !rep.Decision.Allow {
		return exitDenied
	}
	return 0
}

func checkUsage(e Env) int {
	fmt.Fprintln(e.Stderr, "headroom: usage: headroom check [--worktree ID] [--cost 2G] [--kind container|compose|tart] -- command...")
	return 2
}

// parseBytes reads a size: 512M, 2G, 1.5G, or a plain number of GB.
func parseBytes(s string) (uint64, error) {
	unit := float64(1 << 30)
	switch {
	case strings.HasSuffix(s, "M"):
		s, unit = strings.TrimSuffix(s, "M"), 1<<20
	case strings.HasSuffix(s, "G"):
		s = strings.TrimSuffix(s, "G")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || !(v > 0) || v > 1024*float64(1<<30)/unit {
		return 0, errors.New("want e.g. 512M or 2G")
	}
	return uint64(v * unit), nil
}
