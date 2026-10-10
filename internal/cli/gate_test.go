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

// follow runs followEvents over streams at now, returning what it
// delivered (with each event's time as Unix ns), each call's since, and
// its waits.
func follow(now time.Time, streams ...fakeStream) (got []string, since []int64, waits []time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeEvents{streams: streams, cancel: cancel}
	followEvents(ctx, f, func(at time.Time, action, id, name string, _ map[string]string) {
		got = append(got, fmt.Sprint(action, " ", id, " ", name, " ", at.UnixNano()))
	}, func() time.Time { return now }, func(_ context.Context, d time.Duration) { waits = append(waits, d) })
	return got, f.since, waits
}

// farOff is a now later than every event's time in these tests.
var farOff = time.Unix(0, 1e18)

// The daemon follows Docker's events, and reconnects with a growing wait
// when the stream drops, back to the shortest once one delivered.
func TestFollowEventsReconnectsWithBackoff(t *testing.T) {
	got, since, waits := follow(farOff, fakeStream{events: []fakeEvent{{"start", "Q", "quick", 0}}}, fakeStream{}, fakeStream{})
	if fmt.Sprint(got) != "[start Q quick 1000000000000000000]" || len(since) != 3 {
		t.Fatalf("events %v, calls %d", got, len(since))
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("waits = %v", waits)
	}
}

// A reconnect's replay sends again what was delivered: each event is
// delivered once, in order, with its time. Another action at that
// nanosecond is a new event (#146).
func TestFollowEventsReplaysFromTheLastDelivered(t *testing.T) {
	got, since, _ := follow(farOff,
		fakeStream{events: []fakeEvent{{"start", "A", "a", 5*sec + 100}, {"kill", "A", "a", 5*sec + 200}}},
		fakeStream{events: []fakeEvent{{"kill", "A", "a", 5*sec + 200}, {"die", "A", "a", 5*sec + 200}, {"start", "B", "b", 5*sec + 300}}},
		fakeStream{})
	want := []string{fmt.Sprint("start A a ", 5*sec+100), fmt.Sprint("kill A a ", 5*sec+200), fmt.Sprint("die A a ", 5*sec+200), fmt.Sprint("start B b ", 5*sec+300)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	if fmt.Sprint(since) != fmt.Sprint([]int64{0, 4*sec + 200, 4*sec + 300}) {
		t.Errorf("since = %v, want none, then a second before each stream's newest", since)
	}
}

// An event more than a second old, a replay from a gap, is dated when
// Docker says it happened; any other at now (Docker's VM clock may run
// ahead), as is one Docker gave no time, which moves no since (#146).
func TestFollowEventsDatesEventsNoLaterThanNow(t *testing.T) {
	now := time.Unix(0, 10*sec)
	got, since, _ := follow(now,
		fakeStream{events: []fakeEvent{{"start", "A", "a", 8 * sec}, {"die", "A", "a", 11 * sec}, {"start", "B", "b", 0}}},
		fakeStream{})
	if want := []string{fmt.Sprint("start A a ", 8*sec), fmt.Sprint("die A a ", 10*sec), fmt.Sprint("start B b ", 10*sec)}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	if fmt.Sprint(since) != fmt.Sprint([]int64{0, 10 * sec}) {
		t.Errorf("since = %v, want a second before the newest time Docker gave", since)
	}
}

// A stream that replays only what was delivered delivered nothing: the
// wait keeps growing (#146).
func TestFollowEventsAReplayedEventAloneKeepsTheBackoff(t *testing.T) {
	_, _, waits := follow(farOff,
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
	got, since, waits := follow(farOff,
		fakeStream{events: []fakeEvent{{"start", "A", "a", 5 * sec}}},
		fakeStream{err: docker.ErrBadSince},
		fakeStream{events: []fakeEvent{{"start", "B", "b", 3 * sec}}},
		fakeStream{})
	if fmt.Sprint(since) != fmt.Sprint([]int64{0, 4 * sec, 0, 2 * sec}) {
		t.Errorf("since = %v, want the refused one dropped", since)
	}
	if want := []string{fmt.Sprint("start A a ", 5*sec), fmt.Sprint("start B b ", 3*sec)}; fmt.Sprint(got) != fmt.Sprint(want) {
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
	got, _, _ := follow(farOff,
		fakeStream{events: []fakeEvent{{"start", "X", "x", 100}, {"kill", "Y", "y", 300}, {"start", "Z", "z", 250}, {"die", "Y", "y", 400}}},
		fakeStream{})
	if want := []string{"start X x 100", "kill Y y 300", "start Z z 250", "die Y y 400"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// The replay comes from Docker's ring in the order it was logged, not
// its time order: an event in the gap older than one before it is
// delivered (#146).
func TestFollowEventsDeliversAReplayOutOfOrder(t *testing.T) {
	got, _, _ := follow(farOff,
		fakeStream{events: []fakeEvent{{"start", "A", "a", 100}}},
		fakeStream{events: []fakeEvent{{"start", "A", "a", 100}, {"start", "B", "b", 150}, {"die", "C", "c", 140}}},
		fakeStream{})
	if want := []string{"start A a 100", "start B b 150", "die C c 140"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// The Docker VM's clock stepping back (a resync, or a VM restart while the
// daemon stays up) drops no live event (#146).
func TestFollowEventsDeliversEventsAfterTheClockSteppedBack(t *testing.T) {
	got, _, _ := follow(farOff,
		fakeStream{events: []fakeEvent{{"start", "A", "a", 1000}}},
		fakeStream{events: []fakeEvent{{"start", "B", "b", 900}, {"die", "B", "b", 950}}},
		fakeStream{})
	if want := []string{"start A a 1000", "start B b 900", "die B b 950"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// A live event is dated now, though Docker's VM clock, a few ms behind,
// dates it just before: else a die just after a reading began would be
// dated before it, and its container's next run warned ungated (#146).
func TestFollowEventsDatesALiveEventNow(t *testing.T) {
	now := time.Unix(0, 10*sec)
	got, _, _ := follow(now,
		fakeStream{events: []fakeEvent{{"die", "A", "a", 10*sec - 2e6}, {"die", "B", "b", 9 * sec}}},
		fakeStream{})
	if want := []string{fmt.Sprint("die A a ", 10*sec), fmt.Sprint("die B b ", 10*sec)}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %q, want both dated now", got)
	}
}

// A start Docker sends after a later-dated kill still binds its lease, so
// its die frees the cost, with no "never appeared" (#146).
func TestFollowEventsAStartOutOfOrderEndsItsLease(t *testing.T) {
	var log strings.Builder
	now := farOff
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
	followEvents(ctx, f, func(at time.Time, action, id, name string, _ map[string]string) {
		if id == "Q" {
			book.ContainerEventAt(at, action, id, name, lab)
		}
	}, func() time.Time { return now }, func(context.Context, time.Duration) {})
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
	got, since, _ := follow(farOff,
		fakeStream{events: []fakeEvent{{"start", "A", "a", 5*sec + 1}, {"kill", "A", "a", 7*sec + 3}, {"start", "B", "b", 6*sec + 2}}},
		fakeStream{events: []fakeEvent{{"die", "C", "c", 6*sec + 9}, {"kill", "A", "a", 7*sec + 3}}},
		fakeStream{})
	want := []string{fmt.Sprint("start A a ", 5*sec+1), fmt.Sprint("kill A a ", 7*sec+3), fmt.Sprint("start B b ", 6*sec+2), fmt.Sprint("die C c ", 6*sec+9)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	if fmt.Sprint(since) != fmt.Sprint([]int64{0, 6*sec + 3, 6*sec + 3}) {
		t.Errorf("since = %v, want none, then a second before the newest delivered", since)
	}
}

// followEvents remembers only as many delivered events as Docker keeps to
// replay: the oldest beyond them is delivered again (#146).
func TestFollowEventsRemembersTheLast256(t *testing.T) {
	var first fakeStream
	for i := range int64(257) {
		first.events = append(first.events, fakeEvent{"start", "A", "a", 5*sec + i})
	}
	got, _, _ := follow(farOff, first,
		fakeStream{events: []fakeEvent{{"start", "A", "a", 5 * sec}, {"start", "A", "a", 5*sec + 256}}},
		fakeStream{})
	if len(got) != 258 || got[257] != fmt.Sprint("start A a ", 5*sec) {
		t.Fatalf("delivered %d, the last %q: want the oldest of 257 again, and only it", len(got), got[len(got)-1])
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
