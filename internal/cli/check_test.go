package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

func TestCheckAllowAndDeny(t *testing.T) {
	env, d := serveDaemon(t)
	var seen *protocol.CheckRequest
	d.SetCheck(func(r *protocol.CheckRequest, _ *protocol.Snapshot) protocol.Decision {
		seen = r
		if r.CostBytes > 4<<30 {
			return protocol.Decision{Allow: false, Retry: true, Message: "headroom: not starting it: only 3.0 GB headroom"}
		}
		return protocol.Decision{Allow: true, Message: "headroom: allowed"}
	})
	code, out, _ := run(t, env, "headroom", "check", "--worktree", "w1", "--cost", "512M", "--", "docker", "run", "x")
	if code != 0 || !strings.Contains(out, "allowed") {
		t.Fatalf("allow: exit %d, out %q", code, out)
	}
	if seen.Worktree != "w1" || seen.CostBytes != 512<<20 || seen.Command != "docker run x" || seen.Kind != "container" {
		t.Fatalf("request %+v", seen)
	}
	code, out, _ = run(t, env, "headroom", "check", "--worktree", "w1", "--cost", "6G", "--kind", "tart", "--", "tart", "run", "vm")
	if code != 75 || !strings.Contains(out, "not starting") || seen.Kind != "tart" {
		t.Fatalf("deny: exit %d, out %q, request %+v", code, out, seen)
	}
}

func TestCheckWorktreeFromEnv(t *testing.T) {
	env, d := serveDaemon(t)
	var seen *protocol.CheckRequest
	d.SetCheck(func(r *protocol.CheckRequest, _ *protocol.Snapshot) protocol.Decision {
		seen = r
		return protocol.Decision{Allow: true}
	})
	env["HEADROOM_WORKTREE"] = "from-env"
	run(t, env, "headroom", "check", "--", "docker", "run", "x")
	if seen == nil || seen.Worktree != "from-env" || seen.CostBytes != 0 {
		t.Fatalf("request %+v", seen)
	}
}

func TestCheckArgs(t *testing.T) {
	env, _ := serveDaemon(t)
	for _, args := range [][]string{
		{}, {"--cost", "lots", "--", "docker"}, {"--kind", "vm", "--", "docker"}, {"--worktree"}, {"docker", "run"},
	} {
		if code, _, stderr := run(t, env, append([]string{"headroom", "check"}, args...)...); code != 2 {
			t.Errorf("%v: exit %d, %s", args, code, stderr)
		}
	}
}

var _ = context.Background
