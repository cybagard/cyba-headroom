package cli

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/lease"
	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// The Docker VM's clock steps back more than 1 s between Y, logged before
// the daemon connected, and D, delivered live. The reconnect replays Y,
// then D, in the order Docker logged them: Y moves the newest more than a
// second past D, and D is still not delivered again (#165).
func TestFollowEventsAStepBackDeliversAReplayedEventOnce(t *testing.T) {
	got, _, _ := follow(
		fakeStream{events: []fakeEvent{{"start", "D", "d", 98*sec + 500}}},
		fakeStream{events: []fakeEvent{{"start", "Y", "y", 100 * sec}, {"start", "D", "d", 98*sec + 500}}},
		fakeStream{})
	if want := []string{"start D d", "start Y y"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events = %q, want %q", got, want)
	}
}

// The same without a clock step: Y stamped more than 1 s after D, but
// logged before it (#165).
func TestFollowEventsALateStampDeliversAReplayedEventOnce(t *testing.T) {
	got, _, _ := follow(
		fakeStream{events: []fakeEvent{{"die", "D", "d", 50 * sec}}},
		fakeStream{events: []fakeEvent{{"die", "Y", "y", 51*sec + 1}, {"die", "D", "d", 50 * sec}}},
		fakeStream{})
	if want := []string{"die D d", "die Y y"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events = %q, want %q", got, want)
	}
}

// followThrough runs followEvents over streams into a book in which docker
// run --name x is checked, and checks docker start x during the first
// wait. x1 then runs: the start's lease must hold it, with no warning.
func followThrough(t *testing.T, streams []fakeStream) {
	t.Helper()
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
	ctx, cancel := context.WithCancel(context.Background())
	waited := 0
	var got []string
	followEvents(ctx, &fakeEvents{streams: streams, cancel: cancel}, func(action, id, name string, _ map[string]string) {
		got = append(got, action+" "+id)
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
	t.Logf("delivered %q", got)
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

// docker run --name x ran x1 and it exited, live, after the VM's clock
// stepped back 2 s past Y, logged before the daemon connected. docker
// start x is checked while the stream is down. The reconnect replays Y,
// then x1's start and die: they do not reach the book again, so the
// start's lease holds x1 when it runs (#165).
func TestFollowEventsAStepBackKeepsAPendingStart(t *testing.T) {
	pair := []fakeEvent{{"start", "x1", "x", 98*sec + 500e6}, {"die", "x1", "x", 98*sec + 500e6 + 1}}
	followThrough(t, []fakeStream{
		{events: pair},
		{events: append([]fakeEvent{{"kill", "Y", "y", 100 * sec}}, pair...)},
		{}})
}

// The same with no step back: Y at 98.4 s, before x1 (#165).
func TestFollowEventsAReplayKeepsAPendingStart(t *testing.T) {
	pair := []fakeEvent{{"start", "x1", "x", 98*sec + 500e6}, {"die", "x1", "x", 98*sec + 500e6 + 1}}
	followThrough(t, []fakeStream{
		{events: pair},
		{events: append([]fakeEvent{{"kill", "Y", "y", 98*sec + 400e6}}, pair...)},
		{}})
}
