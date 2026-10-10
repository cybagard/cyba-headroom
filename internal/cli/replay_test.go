package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/lease"
	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/source/docker"
)

// h0 is the host's time when a replay test begins.
var h0 = time.Unix(1_000_000, 0)

// delivered is an event as followEvents gave it to fn.
type delivered struct {
	at         time.Time
	action, id string
}

func (d delivered) String() string {
	at := "live"
	if !d.at.IsZero() {
		at = d.at.Sub(h0).String()
	}
	return d.action + " " + d.id + " @" + at
}

// replay runs followEvents over f on a host clock from h0 that only f's
// Clock and gap move: gap(i) is called in the i-th wait, from 1. It
// returns what was delivered, and the log.
func replay(f *fakeEvents, now *time.Time, gap func(i int)) ([]delivered, string) {
	var log strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.advance = func(d time.Duration) { *now = now.Add(d) }
	var got []delivered
	waits := 0
	followEvents(ctx, f, func(at time.Time, action, id, _ string, _ map[string]string) {
		got = append(got, delivered{at, action, id})
	}, func() time.Time { return *now }, slog.New(slog.NewTextHandler(&log, nil)), func(context.Context, time.Duration) {
		waits++
		if gap != nil {
			gap(waits)
		}
	})
	return got, log.String()
}

// A compose project's db runs. The events stream drops, and in the gap db
// crashes, compose up is checked, a reading without db is taken, and db
// starts again. The Docker VM's clock is 3 s behind the host's. The
// reconnect's replay dates the crash before the up, so the up was checked
// since, and its start binds db: the up then reserves only what db does
// not use yet, rather than count db twice (#170).
func TestAReplayDatesACrashBeforeAnUpCheckedInTheGap(t *testing.T) {
	const gib = 1 << 30
	now := h0
	vm := func(at time.Time) int64 { return at.Add(-3 * time.Second).UnixNano() }
	var log strings.Builder
	book := lease.New(2*time.Minute, func() time.Time { return now }, slog.New(slog.NewTextHandler(&log, nil)))
	cfg := policy.Config{PressureGuard: "critical", DefaultContainerBytes: gib, DefaultTartBytes: 4 * gib}
	lab := map[string]string{protocol.ComposeProjectLabel: "app"}
	read := func(db uint64) *protocol.Snapshot {
		headroom := int64(8 * gib)
		s := &protocol.Snapshot{Host: &protocol.Host{TotalBytes: 64 * gib, Pressure: "normal"},
			Budget:      &protocol.Budget{TotalBytes: 64 * gib, HeadroomBytes: &headroom},
			Orca:        &protocol.Orca{Running: true, Worktrees: []protocol.Worktree{{ID: "w1", Name: "A", Agents: []protocol.Agent{{State: "working"}}}}},
			Docker:      &protocol.Docker{Running: true},
			Tart:        &protocol.Tart{Installed: true},
			Attribution: &protocol.Attribution{Worktrees: []protocol.WorktreeUsage{{ID: "w1"}}},
			Sources:     map[string]protocol.SourceStatus{"docker": {At: now.Add(3 * time.Second), Took: time.Second, Began: now}},
			CollectedAt: now}
		if db > 0 {
			s.Docker.Containers = []protocol.Container{{ID: "db", Name: "db", MemoryBytes: db, Labels: lab}}
			s.Attribution.Worktrees[0].Containers = []protocol.AttributedContainer{{ID: "db", Name: "db"}}
		}
		return s
	}
	s := read(gib)
	book.Observe(s)
	book.Observe(s)
	crash, start := vm(h0.Add(time.Second)), vm(h0.Add(3*time.Second))
	f := &fakeEvents{
		streams: []fakeStream{
			{events: []fakeEvent{{"kill", "y", "y", vm(h0)}}},
			{events: []fakeEvent{{"kill", "y", "y", vm(h0)}, {"die", "db", "db", crash}, {"start", "db", "db", start}}},
			{}},
		vmNow: func() int64 { return vm(now) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	waits := 0
	followEvents(ctx, f, func(at time.Time, action, id, name string, _ map[string]string) {
		book.ContainerEventAt(at, action, id, name, lab)
	}, func() time.Time { return now }, discardLog(), func(context.Context, time.Duration) {
		if waits++; waits > 1 {
			return
		}
		now = h0.Add(2 * time.Second) // db crashed at 1 s
		book.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: gib, Target: "app", OnEngine: true}, s, cfg)
		now = h0.Add(2500 * time.Millisecond)
		book.Observe(read(0))
		now = h0.Add(4 * time.Second) // db started at 3 s; the reconnect
	})
	now = now.Add(5 * time.Second)
	book.Observe(read(gib / 4))
	var r uint64
	for _, l := range book.List() {
		r += l.Bytes
	}
	if r != gib-gib/4 {
		t.Fatalf("reserved %d MiB once db is back, want 768\n%s", r>>20, log.String())
	}
}

// The Docker VM's clock is 1.5 s behind the host's. A reconnect's replay
// dates each event it brings by that offset, but never before a second
// before the stream dropped. The live stream that follows asks from a
// second before the replay's end, and dates its events now (#170).
func TestAReplayFromALaggingVMIsDatedByTheOffsetAndALiveDieNow(t *testing.T) {
	now := h0
	vm := func(at time.Time) int64 { return at.Add(-1500 * time.Millisecond).UnixNano() }
	f := &fakeEvents{
		streams: []fakeStream{
			// q, which Docker gave no time, is delivered 3 s on.
			{events: []fakeEvent{{"start", "a", "a", vm(h0)}, {"kill", "q", "q", 0}}},
			{events: []fakeEvent{
				{"start", "a", "a", vm(h0)},
				{"die", "c", "c", vm(h0.Add(-500 * time.Millisecond))}, // published late
				{"die", "a", "a", vm(h0.Add(4 * time.Second))}}},
			{events: []fakeEvent{{"die", "a", "a", vm(h0.Add(4 * time.Second))}, {"die", "b", "b", vm(h0.Add(5 * time.Second))}}}},
		vmNow: func() int64 { return vm(now) },
	}
	var got []delivered
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	followEvents(ctx, f, func(at time.Time, action, id, _ string, _ map[string]string) {
		got = append(got, delivered{at, action, id})
		if id == "q" {
			now = h0.Add(3 * time.Second) // the stream drops at 3 s
		}
	}, func() time.Time { return now }, discardLog(), func(context.Context, time.Duration) {
		now = h0.Add(5 * time.Second) // the reconnect
	})
	want := []string{"start a @live", "kill q @live", "die c @2s", "die a @4s", "die b @live"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("delivered %v, want %v", got, want)
	}
	u := vm(h0.Add(5 * time.Second))
	if wantSince, wantUntil := []int64{0, vm(h0) - sec, u - sec}, []int64{0, u, 0}; fmt.Sprint(f.since, f.until) != fmt.Sprint(wantSince, wantUntil) {
		t.Errorf("calls since %v until %v, want %v %v", f.since, f.until, wantSince, wantUntil)
	}
}

// Each time a replay cannot be dated or bounded, the reconnect is #146's:
// one stream from a second before the newest delivered, every event dated
// when it comes (#170).
func TestAReplayFallsBackAsBefore(t *testing.T) {
	defer func(d time.Duration) { replayTimeout = d }(replayTimeout)
	replayTimeout = 50 * time.Millisecond
	const newest = 1_000_000 * sec
	live := fakeStream{events: []fakeEvent{{"start", "a", "a", newest}}}
	fallback := fakeStream{events: []fakeEvent{{"start", "a", "a", newest}, {"die", "a", "a", newest + 1}}}
	slow := func(f *fakeEvents) { f.rtt = 300 * time.Millisecond }
	for _, tc := range []struct {
		name      string
		vmNow     int64
		clockErr  error
		streams   []fakeStream
		since     []int64
		until     []int64
		clocks    int
		log       string
		configure func(*fakeEvents)
	}{
		{name: "no clock", streams: []fakeStream{live, fallback},
			since: []int64{0, newest - sec}, until: []int64{0, 0}, clocks: 1},
		{name: "a clock error", vmNow: newest + sec, clockErr: errors.New("info: 500"), streams: []fakeStream{live, fallback},
			since: []int64{0, newest - sec}, until: []int64{0, 0}, clocks: 1},
		{name: "every RTT over 250 ms", vmNow: newest + sec, configure: slow, streams: []fakeStream{live, fallback},
			since: []int64{0, newest - sec}, until: []int64{0, 0}, clocks: 3},
		{name: "a clock stepped back", vmNow: newest - 2*sec, streams: []fakeStream{live, fallback},
			since: []int64{0, newest - sec}, until: []int64{0, 0}, clocks: 3, log: "docker VM clock stepped back"},
		{name: "a bounded call that fails", vmNow: newest + sec, streams: []fakeStream{live, {err: errors.New("EOF")}, fallback},
			since: []int64{0, newest - sec, newest - sec}, until: []int64{0, newest + sec, 0}, clocks: 3},
		{name: "a bounded call that times out", vmNow: newest + sec, streams: []fakeStream{live, {block: true}, fallback},
			since: []int64{0, newest - sec, newest - sec}, until: []int64{0, newest + sec, 0}, clocks: 3},
		{name: "a bounded call whose since is refused", vmNow: newest + sec, streams: []fakeStream{live, {err: docker.ErrBadSince}, {err: docker.ErrBadSince}, fallback},
			since: []int64{0, newest - sec, newest - sec, 0}, until: []int64{0, newest + sec, 0, 0}, clocks: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := h0
			f := &fakeEvents{streams: tc.streams, clockErr: tc.clockErr}
			if tc.vmNow != 0 {
				f.vmNow = func() int64 { return tc.vmNow }
			}
			if tc.configure != nil {
				tc.configure(f)
			}
			got, log := replay(f, &now, nil)
			for _, d := range got {
				if !d.at.IsZero() {
					t.Errorf("%v: want every event dated when it comes", d)
				}
			}
			if want := "[start a @live die a @live]"; fmt.Sprint(got) != want {
				t.Errorf("delivered %v, want %s", got, want)
			}
			if fmt.Sprint(f.since, f.until) != fmt.Sprint(tc.since, tc.until) {
				t.Errorf("calls since %v until %v, want %v %v", f.since, f.until, tc.since, tc.until)
			}
			if f.clocks != tc.clocks {
				t.Errorf("Clock calls = %d, want %d", f.clocks, tc.clocks)
			}
			if !strings.Contains(log, tc.log) {
				t.Errorf("log %q, want %q", log, tc.log)
			}
		})
	}
}
