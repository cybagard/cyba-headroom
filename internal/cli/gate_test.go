package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/cybagard/cyba-headroom/internal/source/docker"
)

// The daemon's own wiring: a second check sees the first allow's lease.
func TestGateLeasesAcrossChecks(t *testing.T) {
	env, d := serveDaemonWith(t, func(d *daemon.Daemon) {
		wireGate(d, config.Defaults("/x"), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
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
	check := gateCheckOn(book, config.Defaults("/x").PolicyConfig(), nil, "")
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
	// A manual call's lease reserves nothing; it marks the call as checked,
	// so its container is not flagged ungated (#33).
	if !d.Allow || d.Worktree != "" || d.LeaseID == "" {
		t.Fatalf("manual: %+v", d)
	}
	for _, l := range book.List() {
		if l.ID == d.LeaseID && l.Bytes != 0 {
			t.Fatalf("manual lease reserves %d", l.Bytes)
		}
	}
}

// The daemon gates a macOS tart run on its slots first (R6).
func TestGateCountsMacOSSlots(t *testing.T) {
	book := lease.New(time.Minute, time.Now, discardLog())
	cfg := config.Defaults("/x")
	check := gateCheckOn(book, cfg.PolicyConfig(), nil, "")
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

// Through the daemon's wiring: a snapshot the collector stopped refreshing
// is not decided on (#30).
func TestGateTreatsAnOldSnapshotAsUnknown(t *testing.T) {
	book := lease.New(time.Minute, time.Now, discardLog())
	check := gateCheckOn(book, config.Defaults("/x").PolicyConfig(), nil, "")
	headroom := int64(0) // would deny
	s := &protocol.Snapshot{Budget: &protocol.Budget{HeadroomBytes: &headroom}, CollectedAt: time.Now().Add(-5 * time.Minute)}
	book.Observe(s)
	d := check(&protocol.CheckRequest{Worktree: "w", Kind: "container", Command: "docker run a"}, s)
	if !d.Allow || d.Reasons[0].Code != policy.StaleSnapshot {
		t.Fatalf("%+v", d)
	}
}

// A container that appears without a check is in the snapshot as ungated.
func TestDeriveListsUngated(t *testing.T) {
	book := lease.New(time.Minute, time.Now, discardLog())
	tick := derive(book, config.Defaults("/x").Budget.Params())
	s := &protocol.Snapshot{Docker: &protocol.Docker{Running: true}}
	tick(s)
	s = &protocol.Snapshot{Docker: &protocol.Docker{Running: true, Containers: []protocol.Container{{ID: "c1", Name: "testcontainers-ryuk"}}}}
	tick(s)
	if len(s.Ungated) != 1 || s.Ungated[0].Name != "testcontainers-ryuk" {
		t.Fatalf("ungated = %+v", s.Ungated)
	}
}

type fakeInspector map[string]struct {
	id     string
	labels map[string]string
}

func (f fakeInspector) Inspect(_ context.Context, ref string) (string, map[string]string, bool, error) {
	c, ok := f[ref]
	if !ok {
		return "", nil, false, errors.New("no such container")
	}
	return c.id, c.labels, false, nil
}

// A start's lease is keyed by the container Docker resolves, and takes over
// the run or create whose label it carries (#33).
func TestGateResolvesAStartsContainer(t *testing.T) {
	book := lease.New(time.Minute, time.Now, discardLog())
	pol := config.Defaults("/x").PolicyConfig()
	headroom := int64(64 << 30)
	s := &protocol.Snapshot{Host: &protocol.Host{TotalBytes: 64 << 30, Pressure: "normal"},
		Budget: &protocol.Budget{TotalBytes: 64 << 30, HeadroomBytes: &headroom}, Docker: &protocol.Docker{Running: true}, Tart: &protocol.Tart{}}
	created := gateCheckOn(book, pol, nil, "")(&protocol.CheckRequest{Worktree: "w", Kind: "container", Op: "create", Command: "docker create pg", CostBytes: 4 << 30, Labelled: true}, s)
	insp := fakeInspector{"db": {"full-id", map[string]string{protocol.LeaseLabel: created.LeaseID}}}
	started := gateCheckOn(book, pol, insp, "/s.sock")(&protocol.CheckRequest{Worktree: "w", Kind: "container", Op: "start", Command: "docker start db", Target: "db", Engine: "unix:///s.sock"}, s)
	ls := book.List()
	if len(ls) != 1 || ls[0].ID != started.LeaseID || ls[0].Bytes != 4<<30 {
		t.Fatalf("leases = %+v, want the start's, with the create's 4 GiB", ls)
	}
	// Docker cannot say: the start's lease stands alone.
	gateCheckOn(book, pol, insp, "/s.sock")(&protocol.CheckRequest{Worktree: "w", Kind: "container", Op: "start", Command: "docker start nope", Target: "nope", Engine: "unix:///s.sock"}, s)
	if len(book.List()) != 2 {
		t.Fatalf("leases = %+v", book.List())
	}
}

// Only a plain docker call on the default engine is looked up in Docker.
func TestGateLooksUpOnlyDockerStarts(t *testing.T) {
	book := lease.New(time.Minute, time.Now, discardLog())
	pol := config.Defaults("/x").PolicyConfig()
	headroom := int64(64 << 30)
	s := &protocol.Snapshot{Host: &protocol.Host{TotalBytes: 64 << 30, Pressure: "normal"},
		Budget: &protocol.Budget{TotalBytes: 64 << 30, HeadroomBytes: &headroom}, Docker: &protocol.Docker{Running: true}, Tart: &protocol.Tart{}}
	created := gateCheckOn(book, pol, nil, "")(&protocol.CheckRequest{Worktree: "w", Kind: "container", Op: "create", Command: "docker create pg", CostBytes: 4 << 30, Labelled: true}, s)
	insp := fakeInspector{"db": {"full-id", map[string]string{protocol.LeaseLabel: created.LeaseID}}}
	gateCheckOn(book, pol, insp, "/s.sock")(&protocol.CheckRequest{Worktree: "w", Kind: "container", Op: "start", Command: "podman start db", Target: "db"}, s)
	if len(book.List()) != 2 {
		t.Fatalf("podman's start took over docker's create: %+v", book.List())
	}
}

type flipInspector struct{ running *bool }

func (f flipInspector) Inspect(context.Context, string) (string, map[string]string, bool, error) {
	return "C", nil, *f.running, nil
}

// docker restart db, docker stop db, docker start db within a moment: each
// check asks Docker afresh, so the start sees db stopped and takes a lease.
func TestGateAsksDockerEveryCheck(t *testing.T) {
	book := lease.New(time.Minute, time.Now, discardLog())
	pol := config.Defaults("/x").PolicyConfig()
	headroom := int64(64 << 30)
	s := &protocol.Snapshot{Host: &protocol.Host{TotalBytes: 64 << 30, Pressure: "normal"},
		Budget: &protocol.Budget{TotalBytes: 64 << 30, HeadroomBytes: &headroom}, Docker: &protocol.Docker{Running: true}, Tart: &protocol.Tart{}}
	running := true
	check := gateCheckOn(book, pol, flipInspector{&running}, "/s.sock")
	if d := check(&protocol.CheckRequest{Worktree: "w", Kind: "container", Op: "restart", Command: "docker restart db", Target: "db", Engine: "unix:///s.sock"}, s); d.LeaseID != "" {
		t.Fatalf("restart of a running container leased: %+v", d)
	}
	running = false // docker stop db
	if d := check(&protocol.CheckRequest{Worktree: "w", Kind: "container", Op: "start", Command: "docker start db", Target: "db", Engine: "unix:///s.sock"}, s); d.LeaseID == "" {
		t.Fatalf("start of a stopped container took no lease: %+v", d)
	}
}

// The daemon looks a start up only when the CLI talks to the engine the
// daemon reads: it compares the CLI's endpoint with its own socket.
func TestGateLooksUpOnlyItsOwnEngine(t *testing.T) {
	book := lease.New(time.Minute, time.Now, discardLog())
	pol := config.Defaults("/x").PolicyConfig()
	headroom := int64(64 << 30)
	s := &protocol.Snapshot{Host: &protocol.Host{TotalBytes: 64 << 30, Pressure: "normal"},
		Budget: &protocol.Budget{TotalBytes: 64 << 30, HeadroomBytes: &headroom}, Docker: &protocol.Docker{Running: true}, Tart: &protocol.Tart{}}
	running := true
	check := gateCheckOn(book, pol, flipInspector{&running}, "/Users/dev/.docker/run/docker.sock")
	start := func(endpoint string) protocol.Decision {
		return check(&protocol.CheckRequest{Worktree: "w", Kind: "container", Op: "start", Command: "docker start db", Target: "db", Engine: endpoint}, s)
	}
	if d := start("unix:///Users/dev/.colima/default/docker.sock"); d.LeaseID == "" {
		t.Fatalf("another engine was looked up here: %+v", d)
	}
	if d := start("unix:///Users/dev/.docker/run/docker.sock"); d.LeaseID != "" {
		t.Fatalf("its own engine, db runs: want no lease, got %+v", d)
	}
}

// docker start of a container that runs starts nothing: allowed at once,
// whatever the pressure, with no lease.
func TestStartingARunningContainerIsNeverDenied(t *testing.T) {
	book := lease.New(time.Minute, time.Now, discardLog())
	pol := config.Defaults("/x").PolicyConfig()
	none := int64(0)
	s := &protocol.Snapshot{Host: &protocol.Host{TotalBytes: 64 << 30, Pressure: "critical"},
		Budget: &protocol.Budget{TotalBytes: 64 << 30, HeadroomBytes: &none}, Docker: &protocol.Docker{Running: true}, Tart: &protocol.Tart{}}
	running := true
	d := gateCheckOn(book, pol, flipInspector{&running}, "/s.sock")(&protocol.CheckRequest{Worktree: "w", Kind: "container", Op: "start",
		Command: "docker start db", Target: "db", Engine: "unix:///s.sock"}, s)
	if !d.Allow || d.LeaseID != "" {
		t.Fatalf("decision %+v", d)
	}
}

type mapInspector map[string]struct {
	id      string
	running bool
}

func (m mapInspector) Inspect(_ context.Context, ref string) (string, map[string]string, bool, error) {
	c, ok := m[ref]
	if !ok {
		return "", nil, false, docker.ErrNoSuchContainer
	}
	if c.id == "" {
		return "", nil, false, context.DeadlineExceeded // a lookup that ran out of time
	}
	return c.id, nil, c.running, nil
}

// The gate looks every target of a start up.
func TestGateLooksUpEveryTarget(t *testing.T) {
	book := lease.New(time.Minute, time.Now, discardLog())
	pol := config.Defaults("/x").PolicyConfig()
	headroom := int64(64 << 30)
	s := &protocol.Snapshot{Host: &protocol.Host{TotalBytes: 64 << 30, Pressure: "normal"},
		Budget: &protocol.Budget{TotalBytes: 64 << 30, HeadroomBytes: &headroom}, Docker: &protocol.Docker{Running: true}, Tart: &protocol.Tart{}}
	insp := mapInspector{"a": {"A", true}, "b": {"B", false}, "c": {"C", false}}
	d := gateCheckOn(book, pol, insp, "/s.sock")(&protocol.CheckRequest{Worktree: "w", Kind: "container", Op: "start", Command: "docker start a",
		Target: "a", Targets: []string{"a", "b", "c"}, MultiTarget: true, Engine: "unix:///s.sock"}, s)
	if d.LeaseID == "" || len(book.List()) != 1 || book.List()[0].Bytes != 2*pol.DefaultContainerBytes {
		t.Fatalf("decision %+v, leases %+v: want b and c reserved", d, book.List())
	}
}

type fakeEvents struct {
	calls  int
	cancel context.CancelFunc
}

func (f *fakeEvents) Events(_ context.Context, fn func(action, id string, attrs map[string]string)) error {
	f.calls++
	if f.calls == 1 {
		fn("start", "Q", map[string]string{"name": "quick"})
	}
	if f.calls == 3 {
		f.cancel()
	}
	return errors.New("stream dropped")
}

// The daemon follows Docker's events, and reconnects with a growing wait
// when the stream drops, back to the shortest once one delivered.
func TestFollowEventsReconnectsWithBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeEvents{cancel: cancel}
	var waits []time.Duration
	var got []string
	followEvents(ctx, f, func(action, id, name string, _ map[string]string) { got = append(got, action+" "+id+" "+name) },
		func(_ context.Context, d time.Duration) { waits = append(waits, d) })
	if fmt.Sprint(got) != "[start Q quick]" || f.calls != 3 {
		t.Fatalf("events %v, calls %d", got, f.calls)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("waits = %v", waits)
	}
}

// A first target Docker cannot find does not stop the others' lookup.
func TestGateLooksUpTheOthersWhenTheFirstIsUnknown(t *testing.T) {
	book := lease.New(time.Minute, time.Now, discardLog())
	pol := config.Defaults("/x").PolicyConfig()
	headroom := int64(64 << 30)
	s := &protocol.Snapshot{Host: &protocol.Host{TotalBytes: 64 << 30, Pressure: "normal"},
		Budget: &protocol.Budget{TotalBytes: 64 << 30, HeadroomBytes: &headroom}, Docker: &protocol.Docker{Running: true}, Tart: &protocol.Tart{}}
	insp := mapInspector{"b": {"B", false}, "c": {"C", false}, "slow": {"", false}, "slower": {"", false}}
	gateCheckOn(book, pol, insp, "/s.sock")(&protocol.CheckRequest{Worktree: "w", Kind: "container", Op: "start", Command: "docker start slow",
		Target: "slow", Targets: []string{"slow", "b", "c", "slower"}, MultiTarget: true, Engine: "unix:///s.sock"}, s)
	if l := book.List(); len(l) != 1 || l[0].Bytes != 4*pol.DefaultContainerBytes {
		t.Fatalf("leases %+v: want all four named reserved", l)
	}
}

// podman start db db: one container, though no lookup tells.
func TestGateCountsARepeatedTargetOnce(t *testing.T) {
	var req policy.Request
	lookUpStarts(&req, &protocol.CheckRequest{Target: "db", Targets: []string{"db", "db"}, Engine: "unix:///other.sock"}, nil, "/s.sock")
	if req.Unresolved != 0 {
		t.Fatalf("Unresolved = %d, want 0", req.Unresolved)
	}
}

// Targets Docker says do not exist start nothing: they cost nothing, and
// a start of none that exist takes no lease.
func TestGateChargesNoMissingTarget(t *testing.T) {
	book := lease.New(time.Minute, time.Now, discardLog())
	pol := config.Defaults("/x").PolicyConfig()
	headroom := int64(64 << 30)
	s := &protocol.Snapshot{Host: &protocol.Host{TotalBytes: 64 << 30, Pressure: "normal"},
		Budget: &protocol.Budget{TotalBytes: 64 << 30, HeadroomBytes: &headroom}, Docker: &protocol.Docker{Running: true}, Tart: &protocol.Tart{}}
	insp := mapInspector{"b": {"B", false}}
	check := gateCheckOn(book, pol, insp, "/s.sock")
	check(&protocol.CheckRequest{Worktree: "w", Kind: "container", Op: "start", Command: "docker start typo",
		Target: "typo", Targets: []string{"typo", "b"}, MultiTarget: true, Engine: "unix:///s.sock"}, s)
	if l := book.List(); len(l) != 1 || l[0].Bytes != pol.DefaultContainerBytes {
		t.Fatalf("leases %+v: want b's alone", l)
	}
	if d := check(&protocol.CheckRequest{Worktree: "w", Kind: "container", Op: "start", Command: "docker start nope",
		Target: "nope", Engine: "unix:///s.sock"}, s); !d.Allow || d.LeaseID != "" {
		t.Fatalf("no such container: %+v", d)
	}
}
