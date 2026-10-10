package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
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

// fakeEvents scripts Docker's events streams: each call plays the next
// stream, and the last call ends the context.
type fakeEvents struct {
	streams []fakeStream
	since   []int64 // each call's
	cancel  context.CancelFunc
}

type fakeStream struct {
	events []fakeEvent
	err    error // else "stream dropped"
}

type fakeEvent struct {
	action, id, name string
	at               int64 // Unix ns, 0: Docker gave none
}

func (f *fakeEvents) Events(_ context.Context, since int64, fn func(action, id string, timeNano int64, attrs map[string]string)) error {
	f.since = append(f.since, since)
	st := f.streams[len(f.since)-1]
	for _, e := range st.events {
		fn(e.action, e.id, e.at, map[string]string{"name": e.name})
	}
	if len(f.since) == len(f.streams) {
		f.cancel()
	}
	if st.err != nil {
		return st.err
	}
	return errors.New("stream dropped")
}

// follow runs followEvents over streams, returning what it delivered,
// each call's since, and its waits.
func follow(streams ...fakeStream) (got []string, since []int64, waits []time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeEvents{streams: streams, cancel: cancel}
	followEvents(ctx, f, func(action, id, name string, _ map[string]string) {
		got = append(got, action+" "+id+" "+name)
	}, func(_ context.Context, d time.Duration) { waits = append(waits, d) })
	return got, f.since, waits
}

// The daemon follows Docker's events, and reconnects with a growing wait
// when the stream drops, back to the shortest once one delivered.
func TestFollowEventsReconnectsWithBackoff(t *testing.T) {
	got, since, waits := follow(fakeStream{events: []fakeEvent{{"start", "Q", "quick", 0}}}, fakeStream{}, fakeStream{})
	if fmt.Sprint(got) != "[start Q quick]" || len(since) != 3 {
		t.Fatalf("events %v, calls %d", got, len(since))
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("waits = %v", waits)
	}
}

// A reconnect's replay sends again what was delivered: each event is
// delivered once, in order. Another action at that nanosecond is a new
// event (#146).
func TestFollowEventsReplaysFromTheLastDelivered(t *testing.T) {
	got, since, _ := follow(
		fakeStream{events: []fakeEvent{{"start", "A", "a", 5*sec + 100}, {"kill", "A", "a", 5*sec + 200}}},
		fakeStream{events: []fakeEvent{{"kill", "A", "a", 5*sec + 200}, {"die", "A", "a", 5*sec + 200}, {"start", "B", "b", 5*sec + 300}}},
		fakeStream{})
	want := []string{"start A a", "kill A a", "die A a", "start B b"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	if fmt.Sprint(since) != fmt.Sprint([]int64{0, 4*sec + 200, 4*sec + 300}) {
		t.Errorf("since = %v, want none, then a second before each stream's newest", since)
	}
}

// An event Docker gave no time is delivered, and moves no since (#146).
func TestFollowEventsAnEventWithNoTimeMovesNoSince(t *testing.T) {
	got, since, _ := follow(
		fakeStream{events: []fakeEvent{{"start", "A", "a", 8 * sec}, {"die", "A", "a", 11 * sec}, {"start", "B", "b", 0}}},
		fakeStream{})
	if want := []string{"start A a", "die A a", "start B b"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	if fmt.Sprint(since) != fmt.Sprint([]int64{0, 10 * sec}) {
		t.Errorf("since = %v, want a second before the newest time Docker gave", since)
	}
}

// A stream that replays only what was delivered delivered nothing: the
// wait keeps growing (#146).
func TestFollowEventsAReplayedEventAloneKeepsTheBackoff(t *testing.T) {
	_, _, waits := follow(
		fakeStream{events: []fakeEvent{{"start", "A", "a", 100}}},
		fakeStream{events: []fakeEvent{{"start", "A", "a", 100}}},
		fakeStream{})
	if fmt.Sprint(waits) != "[1s 2s]" {
		t.Fatalf("waits = %v, want [1s 2s]", waits)
	}
}

// Docker refusing the since (it restarted, say): followEvents asks again
// at once without one, and delivers what comes, older or not (#146).
func TestFollowEventsDropsASinceDockerRefuses(t *testing.T) {
	got, since, waits := follow(
		fakeStream{events: []fakeEvent{{"start", "A", "a", 5 * sec}}},
		fakeStream{err: docker.ErrBadSince},
		fakeStream{events: []fakeEvent{{"start", "B", "b", 3 * sec}}},
		fakeStream{})
	if fmt.Sprint(since) != fmt.Sprint([]int64{0, 4 * sec, 0, 2 * sec}) {
		t.Errorf("since = %v, want the refused one dropped", since)
	}
	if want := []string{"start A a", "start B b"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	if fmt.Sprint(waits) != "[1s 1s]" {
		t.Errorf("waits = %v, want none after the refusal", waits)
	}
}

// sec is a second in Unix ns: a reconnect asks Docker for the events from
// a second before the newest delivered (#146).
const sec = int64(time.Second)

// Docker sends live events out of time order: it dates an event before it
// takes its publish lock, and sends each from its own goroutine. Every
// event is delivered, however old (#146).
func TestFollowEventsDeliversALiveEventOutOfOrder(t *testing.T) {
	got, _, _ := follow(
		fakeStream{events: []fakeEvent{{"start", "X", "x", 100}, {"kill", "Y", "y", 300}, {"start", "Z", "z", 250}, {"die", "Y", "y", 400}}},
		fakeStream{})
	if want := []string{"start X x", "kill Y y", "start Z z", "die Y y"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// The replay comes from Docker's ring in the order it was logged, not
// its time order: an event in the gap older than one before it is
// delivered (#146).
func TestFollowEventsDeliversAReplayOutOfOrder(t *testing.T) {
	got, _, _ := follow(
		fakeStream{events: []fakeEvent{{"start", "A", "a", 100}}},
		fakeStream{events: []fakeEvent{{"start", "A", "a", 100}, {"start", "B", "b", 150}, {"die", "C", "c", 140}}},
		fakeStream{})
	if want := []string{"start A a", "start B b", "die C c"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// The Docker VM's clock stepping back (a resync, or a VM restart while the
// daemon stays up) drops no live event (#146).
func TestFollowEventsDeliversEventsAfterTheClockSteppedBack(t *testing.T) {
	got, _, _ := follow(
		fakeStream{events: []fakeEvent{{"start", "A", "a", 1000}}},
		fakeStream{events: []fakeEvent{{"start", "B", "b", 900}, {"die", "B", "b", 950}}},
		fakeStream{})
	if want := []string{"start A a", "start B b", "die B b"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// A start Docker sends after a later-dated kill still binds its lease, so
// its die frees the cost, with no "never appeared" (#146).
func TestFollowEventsAStartOutOfOrderEndsItsLease(t *testing.T) {
	var log strings.Builder
	now := time.Unix(0, 1e18)
	book := lease.New(2*time.Minute, func() time.Time { return now }, slog.New(slog.NewTextHandler(&log, nil)))
	headroom := int64(64 << 30)
	s := &protocol.Snapshot{Host: &protocol.Host{TotalBytes: 64 << 30, Pressure: "normal"},
		Budget: &protocol.Budget{TotalBytes: 64 << 30, HeadroomBytes: &headroom}, Docker: &protocol.Docker{Running: true}, Tart: &protocol.Tart{}}
	book.Observe(s)
	d := book.Check(policy.Request{Worktree: "w", Kind: "container", Command: "docker run x", CostBytes: 2 << 30, Labelled: true}, s, config.Defaults("/x").PolicyConfig())
	ctx, cancel := context.WithCancel(context.Background())
	lab := map[string]string{protocol.LeaseLabel: d.LeaseID}
	f := &fakeEvents{streams: []fakeStream{{}}, cancel: cancel}
	f.streams[0].events = []fakeEvent{{"kill", "Y", "y", 300}, {"start", "Q", "quick", 250}, {"die", "Q", "quick", 400}}
	followEvents(ctx, f, func(action, id, name string, _ map[string]string) {
		if id == "Q" {
			book.ContainerEvent(action, id, name, lab)
		}
	}, func(context.Context, time.Duration) {})
	if l := book.List(); len(l) != 0 {
		t.Errorf("leases = %+v after the die, want none", l)
	}
	now = now.Add(3 * time.Minute)
	book.Observe(s)
	if strings.Contains(log.String(), "never appeared") {
		t.Errorf("log: %s", log.String())
	}
}

// A reconnect asks Docker for the events from a second before the newest
// delivered, to nanosecond precision, so an event dated just before it but
// not yet sent comes too; what was delivered already is not again (#146).
func TestFollowEventsReplaysASecondBeforeTheNewest(t *testing.T) {
	got, since, _ := follow(
		fakeStream{events: []fakeEvent{{"start", "A", "a", 5*sec + 1}, {"kill", "A", "a", 7*sec + 3}, {"start", "B", "b", 6*sec + 2}}},
		fakeStream{events: []fakeEvent{{"die", "C", "c", 6*sec + 9}, {"kill", "A", "a", 7*sec + 3}}},
		fakeStream{})
	want := []string{"start A a", "kill A a", "start B b", "die C c"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	if fmt.Sprint(since) != fmt.Sprint([]int64{0, 6*sec + 3, 6*sec + 3}) {
		t.Errorf("since = %v, want none, then a second before the newest delivered", since)
	}
}

// followEvents remembers what it delivered as far back as a reconnect
// replays, a second before the newest, and forgets what is older (#146).
func TestFollowEventsRemembersASecondBeforeTheNewest(t *testing.T) {
	got, _, _ := follow(
		fakeStream{events: []fakeEvent{{"start", "A", "out", 5 * sec}, {"start", "B", "in", 5*sec + 1}, {"start", "C", "newest", 6*sec + 1}}},
		fakeStream{events: []fakeEvent{{"start", "A", "out", 5 * sec}, {"start", "B", "in", 5*sec + 1}}},
		fakeStream{})
	want := []string{"start A out", "start B in", "start C newest", "start A out"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %q, want %q: the one just inside the second dropped, the one just outside forgotten", got, want)
	}
}

// densely is Docker's ring around a reconnect that delivers a gap late:
// 196 kills already in it when the daemon connected (T+200 to 395 ms),
// then x1's start and die and 58 kills live (T+1000 to 1059 ms), then 10
// more. The second stream replays the gap, and the third the ring's last
// 256 logged. More than 256 events were delivered within the second
// before the newest (#146, review round 3's F3-1).
func densely(first ...fakeEvent) []fakeStream {
	const T, ms = 100 * sec, sec / 1000
	var gap, live, more []fakeEvent
	for i := range int64(196) {
		gap = append(gap, fakeEvent{"kill", fmt.Sprint("G", i), "", T + (200+i)*ms})
	}
	live = append(live, first...)
	for i := int64(len(first)); i < 60; i++ {
		live = append(live, fakeEvent{"kill", fmt.Sprint("A", i), "", T + (1000+i)*ms})
	}
	for i := range int64(10) {
		more = append(more, fakeEvent{"kill", fmt.Sprint("B", i), "", T + (1100+i)*ms})
	}
	return []fakeStream{{events: live}, {events: slices.Concat(gap, live, more)}, {events: slices.Concat(gap[10:], live, more)}, {}}
}

// A gap delivered late, with more than 256 events in the second before
// the newest: no event is delivered twice (#146, review round 3's F3-1).
func TestFollowEventsDeliversNoEventTwiceAfterADenseReplay(t *testing.T) {
	got, _, _ := follow(densely()...)
	n := map[string]int{}
	for _, g := range got {
		n[g]++
	}
	for g, c := range n {
		if c > 1 {
			t.Errorf("%q delivered %d times", g, c)
		}
	}
	if len(n) != 266 {
		t.Errorf("delivered %d events, want 266", len(n))
	}
}

// docker run --name x ran x1 and it exited; docker start x is checked
// while the stream is down, and the reconnect replays a dense second:
// x1's old start and die do not reach the book again, so the start's
// lease holds x1 when it runs (#146, review round 3's D1).
func TestFollowEventsADenseReplayKeepsAPendingStart(t *testing.T) {
	var log strings.Builder
	t0 := time.Unix(1000, 0)
	now := t0
	book := lease.New(2*time.Minute, func() time.Time { return now }, slog.New(slog.NewTextHandler(&log, nil)))
	snap := func(began time.Time, running bool) *protocol.Snapshot {
		headroom := int64(8 << 30)
		s := &protocol.Snapshot{Host: &protocol.Host{TotalBytes: 64 << 30, Pressure: "normal"},
			Budget:      &protocol.Budget{TotalBytes: 64 << 30, HeadroomBytes: &headroom},
			Docker:      &protocol.Docker{Running: true},
			Tart:        &protocol.Tart{Installed: true},
			Sources:     map[string]protocol.SourceStatus{"docker": {At: began.Add(3 * time.Second), Took: time.Second, Began: began}},
			CollectedAt: now}
		if running {
			s.Docker.Containers = []protocol.Container{{ID: "x1", Name: "x", Image: "alpine", MemoryBytes: 1 << 29}}
		}
		return s
	}
	cfg := policy.Config{PressureGuard: "critical", DefaultContainerBytes: 1 << 30, DefaultTartBytes: 4 << 30}
	book.Observe(snap(t0, false))
	book.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker run alpine", CostBytes: 1 << 30, Target: "alpine", Name: "x"}, snap(t0, false), cfg)
	streams := densely(fakeEvent{"start", "x1", "x", 100*sec + sec}, fakeEvent{"die", "x1", "x", 100*sec + sec + 1})
	ctx, cancel := context.WithCancel(context.Background())
	waited := 0
	followEvents(ctx, &fakeEvents{streams: streams, cancel: cancel}, func(action, id, name string, _ map[string]string) {
		if id == "x1" {
			now = now.Add(time.Second)
			book.ContainerEvent(action, id, name, nil)
		}
	}, func(context.Context, time.Duration) {
		now = now.Add(time.Second)
		if waited++; waited == 1 {
			book.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start x", CostBytes: 1 << 30, Target: "x"}, snap(t0, false), cfg)
		}
	})
	now = now.Add(time.Second)
	book.ContainerEvent("start", "x1", "x", nil) // docker start x
	now = now.Add(2 * time.Second)
	book.Observe(snap(now.Add(-time.Second), true))
	if ls := book.List(); len(ls) != 1 {
		t.Errorf("leases = %+v, want docker start x holding x1", ls)
	}
	now = now.Add(3 * time.Minute)
	book.Observe(snap(now.Add(-time.Second), true))
	if strings.Contains(log.String(), "ungated") || strings.Contains(log.String(), "never appeared") {
		t.Errorf("warned: %s", log.String())
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

// docker start " b": the CLI trims it and starts b, so it costs.
func TestGateChargesAPaddedTarget(t *testing.T) {
	book := lease.New(time.Minute, time.Now, discardLog())
	pol := config.Defaults("/x").PolicyConfig()
	headroom := int64(64 << 30)
	s := &protocol.Snapshot{Host: &protocol.Host{TotalBytes: 64 << 30, Pressure: "normal"},
		Budget: &protocol.Budget{TotalBytes: 64 << 30, HeadroomBytes: &headroom}, Docker: &protocol.Docker{Running: true}, Tart: &protocol.Tart{}}
	insp := mapInspector{"b": {"B", false}}
	if d := gateCheckOn(book, pol, insp, "/s.sock")(&protocol.CheckRequest{Worktree: "w", Kind: "container", Op: "start", Command: "docker start",
		Target: " b", Engine: "unix:///s.sock"}, s); d.LeaseID == "" {
		t.Fatalf("decision %+v: want a lease", d)
	}
}

// A compose project the shim guessed reaches the book as a guess: it takes
// no lease over (#84).
func TestGatePassesAGuessedProjectOn(t *testing.T) {
	book := lease.New(time.Minute, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	check := gateCheckOn(book, config.Defaults("/x").PolicyConfig(), nil, "")
	headroom := int64(64 << 30)
	s := &protocol.Snapshot{Budget: &protocol.Budget{HeadroomBytes: &headroom}}
	up := protocol.CheckRequest{Worktree: "w", Kind: "compose", Op: "up", Command: "docker compose up", Target: "app"}
	check(&up, s)
	up.Guessed = true
	check(&up, s)
	if l := book.List(); len(l) != 2 {
		t.Fatalf("leases = %+v, want both: the guess took the up's over", l)
	}
}

// The Docker VM's clock 1.5 s behind the host's (after host sleep, until
// its time sync catches up): a compose run's one-off dies 2 ms after a
// reading began, and the die is dated now, not by the VM's clock before
// the reading, so the run's next one-off is not warned ungated (#146,
// review round 2's Q4 and L2).
func TestFollowEventsALiveDieFromALaggingVMIsDatedNow(t *testing.T) {
	var log strings.Builder
	t0 := time.Unix(1000, 0)
	now := t0
	book := lease.New(2*time.Minute, func() time.Time { return now }, slog.New(slog.NewTextHandler(&log, nil)))
	lab := map[string]string{protocol.ComposeProjectLabel: "p", protocol.ComposeOneoffLabel: "True"}
	snap := func(began time.Time, oneoff string) *protocol.Snapshot {
		headroom := int64(8 << 30)
		s := &protocol.Snapshot{Host: &protocol.Host{TotalBytes: 64 << 30, Pressure: "normal"},
			Budget:      &protocol.Budget{TotalBytes: 64 << 30, HeadroomBytes: &headroom},
			Docker:      &protocol.Docker{Running: true},
			Tart:        &protocol.Tart{Installed: true},
			Sources:     map[string]protocol.SourceStatus{"docker": {At: began.Add(3 * time.Second), Took: time.Second, Began: began}},
			CollectedAt: now}
		s.Docker.Containers = []protocol.Container{{ID: oneoff, Name: oneoff, MemoryBytes: 1 << 28, Labels: lab}}
		return s
	}
	composeRun := policy.Request{Worktree: "w1", Kind: "compose", Op: "run", Command: "docker compose run", CostBytes: 1 << 30, Target: "p"}
	cfg := policy.Config{PressureGuard: "critical", DefaultContainerBytes: 1 << 30, DefaultTartBytes: 4 << 30}
	empty := snap(t0, "")
	empty.Docker.Containers = nil
	book.Observe(empty)
	book.Check(composeRun, empty, cfg)
	now = t0.Add(2 * time.Second)
	book.ContainerEvent("start", "r1", "r1", lab)
	now = t0.Add(5 * time.Second)
	book.Check(composeRun, empty, cfg)
	now = t0.Add(6002 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeEvents{streams: []fakeStream{{events: []fakeEvent{{"die", "r1", "r1", now.Add(-1500 * time.Millisecond).UnixNano()}}}}, cancel: cancel}
	followEvents(ctx, f, func(action, id, name string, _ map[string]string) {
		book.ContainerEvent(action, id, name, lab)
	}, func(context.Context, time.Duration) {})
	now = t0.Add(7 * time.Second)
	book.Observe(snap(t0.Add(6*time.Second), "r1"))
	now = t0.Add(16 * time.Second)
	book.Observe(snap(t0.Add(15*time.Second), "r2"))
	if strings.Contains(log.String(), "ungated") {
		t.Errorf("the run's own one-off was warned ungated: %s", log.String())
	}
}
