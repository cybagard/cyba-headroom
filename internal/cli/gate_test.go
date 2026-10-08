package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/lease"
	"github.com/cybagard/cyba-headroom/internal/policy"
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

// The daemon resolves who is calling before deciding (#28).
func TestGateIdentifiesTheCaller(t *testing.T) {
	book := lease.New(time.Minute, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	check := gateCheck(book, config.Defaults("/x").PolicyConfig())
	headroom := int64(64 << 30)
	s := &protocol.Snapshot{
		Budget: &protocol.Budget{HeadroomBytes: &headroom},
		Orca: &protocol.Orca{Running: true, Worktrees: []protocol.Worktree{
			{ID: "repo::/Users/dev/src/project-a", Path: "/Users/dev/src/project-a", SessionPIDs: []int{100}},
		}},
	}
	d := check(&protocol.CheckRequest{Kind: "container", Command: "docker run a", Cwd: "/Users/dev/src/project-a/sub"}, s)
	if !d.Allow || d.Worktree != "repo::/Users/dev/src/project-a" || d.IdentifiedBy != protocol.IdentifiedByCwd || d.LeaseID == "" {
		t.Fatalf("by cwd: %+v", d)
	}
	if l := book.List(); len(l) != 1 || l[0].Worktree != "repo::/Users/dev/src/project-a" {
		t.Fatalf("lease not the worktree's: %+v", l)
	}
	d = check(&protocol.CheckRequest{Kind: "container", Command: "docker run b", Cwd: "/Users/dev", Ancestors: []int{50, 100}}, s)
	if d.Worktree != "repo::/Users/dev/src/project-a" || d.IdentifiedBy != protocol.IdentifiedByProcess {
		t.Fatalf("by ancestry: %+v", d)
	}
	d = check(&protocol.CheckRequest{Kind: "container", Command: "docker run s", Cwd: "/Users/dev/link/sub", RealCwd: "/Users/dev/src/project-a/sub"}, s)
	if d.Worktree != "repo::/Users/dev/src/project-a" || d.IdentifiedBy != protocol.IdentifiedByCwd {
		t.Fatalf("by resolved cwd: %+v", d)
	}
	d = check(&protocol.CheckRequest{Kind: "container", Command: "docker run c", Cwd: "/Users/dev"}, s)
	if !d.Allow || d.Worktree != "" || d.LeaseID != "" {
		t.Fatalf("manual: %+v", d)
	}
}

// The daemon gates a macOS tart run on its slots first (R6).
func TestGateCountsMacOSSlots(t *testing.T) {
	book := lease.New(time.Minute, time.Now, discardLog())
	cfg := config.Defaults("/x")
	check := gateCheck(book, cfg.PolicyConfig())
	headroom := int64(64 << 30)
	s := &protocol.Snapshot{
		Budget: &protocol.Budget{HeadroomBytes: &headroom},
		Tart: &protocol.Tart{Installed: true, MacOSRunning: 2, VMs: []protocol.TartVM{
			{Name: "a-mac", OS: "darwin"}, {Name: "b-mac", OS: "darwin"}}},
	}
	d := check(&protocol.CheckRequest{Worktree: "w", Kind: "tart", Command: "tart run c-mac", MacOS: true}, s)
	if d.Allow || d.Reasons[0].Code != policy.VMSlots || !strings.Contains(d.Message, "a-mac (manual)") {
		t.Fatalf("third macOS VM: %+v", d)
	}
}
