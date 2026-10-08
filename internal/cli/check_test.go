package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

func TestCheckAllowAndDeny(t *testing.T) {
	var seen *protocol.CheckRequest
	env, _ := serveDaemonWith(t, func(d *daemon.Daemon) {
		d.SetCheck(func(r *protocol.CheckRequest, _ *protocol.Snapshot) protocol.Decision {
			seen = r
			if r.CostBytes > 4<<30 {
				return protocol.Decision{Allow: false, Retry: true, Message: "headroom: not starting it: only 3.0 GB headroom"}
			}
			return protocol.Decision{Allow: true, Message: "headroom: allowed"}
		})
	})
	code, out, _ := run(t, env, "headroom", "check", "--worktree", "w1", "--cost", "512MB", "--", "docker", "run", "x")
	if code != 0 || !strings.Contains(out, "allowed") {
		t.Fatalf("allow: exit %d, out %q", code, out)
	}
	if seen.Worktree != "w1" || seen.CostBytes != 512<<20 || seen.Command != "docker run x" || seen.Kind != "container" {
		t.Fatalf("request %+v", seen)
	}
	// The deny goes to stderr, where an agent looks for why a command failed.
	code, out, stderr := run(t, env, "headroom", "check", "--worktree", "w1", "--cost", "6gib", "--kind", "tart", "--", "tart", "run", "vm")
	if code != 75 || out != "" || !strings.Contains(stderr, "not starting") || seen.Kind != "tart" || seen.CostBytes != 6<<30 {
		t.Fatalf("deny: exit %d, out %q, stderr %q, request %+v", code, out, stderr, seen)
	}
}

func TestCheckWorktreeFromEnv(t *testing.T) {
	var seen *protocol.CheckRequest
	env, _ := serveDaemonWith(t, func(d *daemon.Daemon) {
		d.SetCheck(func(r *protocol.CheckRequest, _ *protocol.Snapshot) protocol.Decision {
			seen = r
			return protocol.Decision{Allow: true}
		})
	})
	env["HEADROOM_WORKTREE"] = "from-env"
	run(t, env, "headroom", "check", "--", "docker", "run", "x")
	if seen == nil || seen.Worktree != "from-env" || seen.CostBytes != 0 {
		t.Fatalf("request %+v", seen)
	}
}

// headroom check identifies its caller as the shim does (#28).
func TestCheckIdentifiesLikeTheShim(t *testing.T) {
	var seen *protocol.CheckRequest
	env, _ := serveDaemonWith(t, func(d *daemon.Daemon) {
		d.SetCheck(func(r *protocol.CheckRequest, _ *protocol.Snapshot) protocol.Decision {
			seen = r
			return protocol.Decision{Allow: true}
		})
	})
	env["ORCA_WORKTREE_ID"] = "orca-id"
	run(t, env, "headroom", "check", "--", "docker", "run", "x")
	if seen == nil || seen.Worktree != "orca-id" {
		t.Fatalf("request %+v", seen)
	}
	delete(env, "ORCA_WORKTREE_ID")
	run(t, env, "headroom", "check", "--", "docker", "run", "x")
	if seen.Worktree != "" || seen.Cwd == "" || len(seen.Ancestors) == 0 {
		t.Fatalf("without the env, want cwd and ancestry: %+v", seen)
	}
}

func TestCheckArgs(t *testing.T) {
	env, _ := serveDaemon(t)
	for _, args := range [][]string{
		{}, {"--cost", "lots", "--", "docker"}, {"--cost", "0.0000000001", "--", "docker"}, {"--kind", "vm", "--", "docker"},
		{"--worktree"}, {"docker", "run"},
	} {
		if code, _, stderr := run(t, env, append([]string{"headroom", "check"}, args...)...); code != 2 {
			t.Errorf("%v: exit %d, %s", args, code, stderr)
		}
	}
}

func TestCheckAgainstADaemonWithoutCheck(t *testing.T) {
	env, _ := serveDaemon(t) // no check hook: like an older daemon
	code, _, stderr := run(t, env, "headroom", "check", "--worktree", "w", "--", "docker", "run", "x")
	if code != 1 || strings.Contains(stderr, "not reachable") || !strings.Contains(stderr, "headroom install") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

var _ = context.Background

// headroom check gives the shim's advice for a daemon on another version.
func TestCheckAgainstAnotherVersion(t *testing.T) {
	r := newShimRig(t)
	serveOnce(t, filepath.Join(r.dir, "d.sock"), `{"v":99,"ok":true}`, 0)
	code, _, stderr := run(t, map[string]string{"HEADROOM_CONFIG_DIR": r.dir}, "headroom", "check", "--worktree", "w", "--", "docker", "run", "x")
	if code != 1 || !strings.Contains(stderr, "headroom install") || strings.Contains(stderr, "not reachable") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}
