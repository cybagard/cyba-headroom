package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// The daemon's own wiring: a second check sees the first allow's lease.
func TestGateLeasesAcrossChecks(t *testing.T) {
	env, d := serveDaemonWith(t, func(d *daemon.Daemon) {
		wireGate(d, config.Defaults("/x"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	})
	// 64 GiB host, nothing else known: 64 GiB headroom. 40 + 40 do not fit.
	code, out, _ := run(t, env, "headroom", "check", "--worktree", "w", "--cost", "40G", "--", "docker", "run", "a")
	if code != 0 {
		t.Fatalf("first: exit %d, %s", code, out)
	}
	code, _, stderr := run(t, env, "headroom", "check", "--worktree", "w", "--cost", "40G", "--", "docker", "run", "b")
	if code != exitDenied || !strings.Contains(stderr, "40.0 GB of it promised") {
		t.Fatalf("second: exit %d, %s", code, stderr)
	}
	// After a tick, the open lease is in the snapshot.
	d.Tick(context.Background())
	_, js, _ := run(t, env, "headroom", "status", "--json")
	var s protocol.Snapshot
	if err := json.Unmarshal([]byte(js), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Leases) != 1 || s.Leases[0].Bytes != 40<<30 || s.Leases[0].Command != "docker run a" {
		t.Fatalf("leases = %+v", s.Leases)
	}
}
