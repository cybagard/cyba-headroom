package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// fakeSource returns whatever its collect func returns.
type fakeSource struct {
	name    string
	collect func(ctx context.Context) (Reading, error)
}

func (f fakeSource) Name() string                                 { return f.name }
func (f fakeSource) Collect(ctx context.Context) (Reading, error) { return f.collect(ctx) }

// recorder notes which reading each source contributed to each snapshot,
// standing in for the typed sections real collectors write.
type recorder struct {
	mu  sync.Mutex
	got map[uint64]map[string]int // tick number -> source -> reading value
	n   uint64
}

type valueReading struct {
	rec    *recorder
	source string
	v      int
}

func (r valueReading) Apply(*protocol.Snapshot) {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	if r.rec.got[r.rec.n] == nil {
		r.rec.got[r.rec.n] = map[string]int{}
	}
	r.rec.got[r.rec.n][r.source] = r.v
}

func (r *recorder) tick(ctx context.Context, d *Daemon) map[string]int {
	r.mu.Lock()
	r.n++
	n := r.n
	r.mu.Unlock()
	d.Tick(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got[n]
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newDaemon(t *testing.T, timeout time.Duration, srcs ...Source) *Daemon {
	t.Helper()
	d, err := New(srcs, timeout, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSnapshotBeforeFirstTick(t *testing.T) {
	d := newDaemon(t, time.Second)
	s := d.Snapshot()
	if s.Seq != 0 || s.Sources == nil {
		t.Fatalf("got %+v", s)
	}
}

func TestDuplicateSourceRejected(t *testing.T) {
	s := fakeSource{name: "docker"}
	if _, err := New([]Source{s, s}, time.Second, quietLog()); err == nil {
		t.Fatal("want error for duplicate source")
	}
}

func TestFailingSourceKeepsLastGoodReading(t *testing.T) {
	rec := &recorder{got: map[uint64]map[string]int{}}
	calls := 0
	flaky := fakeSource{name: "flaky", collect: func(context.Context) (Reading, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("socket gone")
		}
		return valueReading{rec, "flaky", calls}, nil
	}}
	steady := fakeSource{name: "steady", collect: func(context.Context) (Reading, error) {
		return valueReading{rec, "steady", 7}, nil
	}}
	d := newDaemon(t, time.Second, flaky, steady)
	ctx := context.Background()

	rec.tick(ctx, d)
	firstAt := d.Snapshot().Sources["flaky"].At

	got := rec.tick(ctx, d)
	s := d.Snapshot()
	if got["flaky"] != 1 || got["steady"] != 7 {
		t.Fatalf("readings after failure = %v, want flaky=1 (last good) steady=7", got)
	}
	st := s.Sources["flaky"]
	if !st.Stale || st.Err != "socket gone" || !st.At.Equal(firstAt) {
		t.Fatalf("flaky status = %+v", st)
	}
	if s.Sources["steady"].Stale || s.Seq != 2 {
		t.Fatalf("steady=%+v seq=%d", s.Sources["steady"], s.Seq)
	}

	got = rec.tick(ctx, d)
	if got["flaky"] != 3 || d.Snapshot().Sources["flaky"].Stale {
		t.Fatalf("recovery: readings=%v status=%+v", got, d.Snapshot().Sources["flaky"])
	}
}

func TestSlowSourceTimesOutWithoutDelayingOthers(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	// Ignores its context, like a hung exec without CommandContext.
	hung := fakeSource{name: "hung", collect: func(context.Context) (Reading, error) {
		<-release
		return nil, errors.New("unreachable")
	}}
	rec := &recorder{got: map[uint64]map[string]int{}}
	fast := fakeSource{name: "fast", collect: func(context.Context) (Reading, error) {
		return valueReading{rec, "fast", 1}, nil
	}}
	d := newDaemon(t, 50*time.Millisecond, hung, fast)

	start := time.Now()
	got := rec.tick(context.Background(), d)
	if el := time.Since(start); el > time.Second {
		t.Fatalf("tick took %s, want ~source timeout", el)
	}
	s := d.Snapshot()
	if st := s.Sources["hung"]; !st.Stale || st.Err == "" || !st.At.IsZero() {
		t.Fatalf("hung status = %+v", st)
	}
	if got["fast"] != 1 || s.Sources["fast"].Stale {
		t.Fatalf("fast reading=%v status=%+v", got, s.Sources["fast"])
	}

	// The hung call is still running: the next tick skips it instead of
	// stacking another, and keeps reporting how long it has been slow.
	d.Tick(context.Background())
	st := d.Snapshot().Sources["hung"]
	if st.Err != errStillRunning.Error() || st.Took < 50*time.Millisecond {
		t.Fatalf("second tick status = %+v, want err %q and Took >= timeout", st, errStillRunning)
	}
}

func TestPanickingSourceIsContained(t *testing.T) {
	boom := fakeSource{name: "boom", collect: func(context.Context) (Reading, error) { panic("nil map") }}
	d := newDaemon(t, time.Second, boom)
	d.Tick(context.Background())
	if st := d.Snapshot().Sources["boom"]; !st.Stale || st.Err != "panic: nil map" {
		t.Fatalf("status = %+v", st)
	}
	// A panic must also release the busy flag.
	d.Tick(context.Background())
	if st := d.Snapshot().Sources["boom"]; st.Err != "panic: nil map" {
		t.Fatalf("second tick status = %+v", st)
	}
}

func TestNilReadingIsAnError(t *testing.T) {
	empty := fakeSource{name: "empty", collect: func(context.Context) (Reading, error) { return nil, nil }}
	d := newDaemon(t, time.Second, empty)
	d.Tick(context.Background())
	if st := d.Snapshot().Sources["empty"]; !st.Stale {
		t.Fatalf("status = %+v", st)
	}
}

func TestRunTicksImmediatelyAndStops(t *testing.T) {
	d := newDaemon(t, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx, time.Hour); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for d.Snapshot().Seq == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no tick before the first interval")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestCancelledTickPublishesNothing(t *testing.T) {
	// Blocks until its context ends, as a well-behaved source does.
	waits := fakeSource{name: "waits", collect: func(ctx context.Context) (Reading, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	d := newDaemon(t, time.Second, waits)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.Tick(ctx)
	if d.Snapshot().Seq != 0 {
		t.Fatal("tick with an already-cancelled context published a snapshot")
	}

	// Cancelled mid-tick (shutdown): no snapshot of sources falsely "timed out".
	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	d.Tick(ctx)
	if s := d.Snapshot(); s.Seq != 0 {
		t.Fatalf("tick cancelled mid-way published %+v", s)
	}
}

func TestRepeatedFailureWarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	down := fakeSource{name: "down", collect: func(context.Context) (Reading, error) {
		return nil, errors.New("unsupported")
	}}
	d, err := New([]Source{down}, time.Second, log)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		d.Tick(context.Background())
	}
	if n := strings.Count(buf.String(), "source failed"); n != 1 {
		t.Fatalf("logged %d warnings for the same error, want 1:\n%s", n, buf.String())
	}
}

// sectionReading fills the Host section, like a real collector.
type sectionReading struct{ total uint64 }

func (r sectionReading) Apply(s *protocol.Snapshot) { s.Host = &protocol.Host{TotalBytes: r.total} }

func TestDeriveSeesEveryReadingAndIsPublished(t *testing.T) {
	src := fakeSource{name: "host", collect: func(context.Context) (Reading, error) {
		return sectionReading{total: 64}, nil
	}}
	d := newDaemon(t, time.Second, src)
	d.SetDerive(func(s *protocol.Snapshot) {
		if s.Host == nil {
			t.Error("derive ran before readings were applied")
			return
		}
		s.Budget = &protocol.Budget{TotalBytes: s.Host.TotalBytes}
	})
	d.Tick(context.Background())
	if b := d.Snapshot().Budget; b == nil || b.TotalBytes != 64 {
		t.Fatalf("budget = %+v, want total 64", b)
	}
}
