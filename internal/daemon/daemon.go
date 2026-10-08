// Package daemon is the headroom collector daemon (R1): it polls every source
// on a fixed interval, holds the latest snapshot in memory and serves it to
// clients over a Unix socket.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// Source is one thing the daemon reads: Docker, Tart, LM Studio, Orca, host.
type Source interface {
	// Name identifies the source in the snapshot. It must be unique.
	Name() string
	// Collect reads the source once. It should return promptly when ctx ends.
	Collect(ctx context.Context) (Reading, error)
}

// Reading is one source's result. Apply writes it into its own section of the
// snapshot being built; it must not touch other sources' sections.
type Reading interface {
	Apply(*protocol.Snapshot)
}

// errStillRunning marks a source whose previous Collect has not returned.
var errStillRunning = errors.New("previous collection still running")

type sourceState struct {
	src  Source
	busy atomic.Bool
	// last and status are only touched by Tick, which is serialised.
	last   Reading
	status protocol.SourceStatus
}

// Daemon holds the sources and the latest published snapshot.
type Daemon struct {
	sources []*sourceState
	timeout time.Duration
	log     *slog.Logger
	now     func() time.Time
	derive  func(*protocol.Snapshot)
	publish func(*protocol.Snapshot)
	check   CheckFunc
	release func(id string) bool

	tickMu sync.Mutex
	seq    uint64
	snap   atomic.Pointer[protocol.Snapshot]
}

// New returns a daemon over sources, giving each Collect at most sourceTimeout.
func New(sources []Source, sourceTimeout time.Duration, log *slog.Logger) (*Daemon, error) {
	if sourceTimeout <= 0 {
		return nil, errors.New("daemon: source timeout must be > 0")
	}
	d := &Daemon{timeout: sourceTimeout, log: log, now: time.Now}
	seen := map[string]bool{}
	for _, s := range sources {
		if seen[s.Name()] {
			return nil, fmt.Errorf("daemon: duplicate source %q", s.Name())
		}
		seen[s.Name()] = true
		d.sources = append(d.sources, &sourceState{src: s})
	}
	d.snap.Store(&protocol.Snapshot{Sources: map[string]protocol.SourceStatus{}})
	return d, nil
}

// SetDerive sets f to run on each new snapshot after every source's reading
// is applied and before it is published, to add sections computed from the
// others (the budget, R2). f may set sections but must not modify the
// readings it is given. Call it before Run or Tick.
func (d *Daemon) SetDerive(f func(*protocol.Snapshot)) { d.derive = f }

// OnPublish sets f to receive each snapshot right after it is published,
// e.g. to record it. f runs on the tick's goroutine, so it must not block,
// and must not modify the snapshot. Call it before Run or Tick.
func (d *Daemon) OnPublish(f func(*protocol.Snapshot)) { d.publish = f }

// CheckFunc decides a CheckRequest against a snapshot.
type CheckFunc func(*protocol.CheckRequest, *protocol.Snapshot) protocol.Decision

// SetCheck sets f to answer OpCheck from the latest snapshot (#24). Call it
// before Serve, like the other hooks.
func (d *Daemon) SetCheck(f CheckFunc) { d.check = f }

// SetRelease sets f to answer OpRelease: end lease id, reporting whether it
// was open (#28). Call it before Serve.
func (d *Daemon) SetRelease(f func(id string) bool) { d.release = f }

// Snapshot returns the latest published snapshot. Callers must not modify it.
func (d *Daemon) Snapshot() *protocol.Snapshot { return d.snap.Load() }

// Run ticks immediately, then every interval until ctx ends.
func (d *Daemon) Run(ctx context.Context, interval time.Duration) {
	d.Tick(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.Tick(ctx)
		}
	}
}

// Tick collects every source concurrently and publishes a new snapshot. A
// source that fails, times out or panics keeps its last good reading and is
// marked stale; it never delays the others beyond the source timeout. A tick
// whose ctx ends (shutdown) publishes nothing.
func (d *Daemon) Tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	d.tickMu.Lock()
	defer d.tickMu.Unlock()

	tctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	start := d.now()

	type result struct {
		r    Reading
		err  error
		took time.Duration
	}
	// A nil channel marks a source skipped because its last call is still running.
	pending := make([]chan result, len(d.sources))
	for i, s := range d.sources {
		if !s.busy.CompareAndSwap(false, true) {
			continue
		}
		ch := make(chan result, 1)
		pending[i] = ch
		go func() {
			defer s.busy.Store(false)
			r, err := safeCollect(tctx, s.src)
			ch <- result{r, err, d.now().Sub(start)}
		}()
	}

	results := make([]result, len(d.sources))
	for i, ch := range pending {
		if ch == nil {
			continue
		}
		select {
		case results[i] = <-ch:
		case <-tctx.Done():
			select {
			case results[i] = <-ch: // finished right at the deadline
			default:
				results[i] = result{err: fmt.Errorf("timed out after %s", d.timeout), took: d.now().Sub(start)}
			}
		}
	}
	if ctx.Err() != nil {
		return
	}

	next := &protocol.Snapshot{Sources: make(map[string]protocol.SourceStatus, len(d.sources))}
	for i, s := range d.sources {
		res := results[i]
		switch {
		case pending[i] == nil:
			// Keep Took from the call still running, so slowness stays visible.
			s.status.Err = errStillRunning.Error()
			s.status.Stale = true
			d.log.Debug("source skipped", "source", s.src.Name(), "err", errStillRunning)
		case res.err != nil:
			// Warn when a source starts failing or its error changes, not every tick.
			if !s.status.Stale || s.status.Err != res.err.Error() {
				d.log.Warn("source failed", "source", s.src.Name(), "err", res.err, "took", res.took)
			}
			s.status.Took = res.took
			s.status.Err = res.err.Error()
			s.status.Stale = true
		default:
			s.last = res.r
			s.status = protocol.SourceStatus{At: d.now(), Took: res.took}
		}
		if s.last != nil {
			s.last.Apply(next)
		}
		next.Sources[s.src.Name()] = s.status
	}
	if d.derive != nil {
		next = d.safeDerive(next)
	}
	d.seq++
	next.Seq = d.seq
	next.CollectedAt = d.now()
	d.snap.Store(next)
	if d.publish != nil {
		d.publish(next)
	}
}

// safeDerive runs derive on a copy of s and returns the copy, or s itself if
// derive panicked: then the snapshot is published with the sources' readings
// but none of the derived sections, never half of them. derive only adds
// sections, so a shallow copy keeps the readings it was given intact.
func (d *Daemon) safeDerive(s *protocol.Snapshot) (out *protocol.Snapshot) {
	derived := *s
	defer func() {
		if p := recover(); p != nil {
			d.log.Error("derive failed", "panic", fmt.Sprint(p))
			out = s
		}
	}()
	d.derive(&derived)
	return &derived
}

// safeCollect turns a panic or a nil reading into an error.
func safeCollect(ctx context.Context, src Source) (r Reading, err error) {
	defer func() {
		if p := recover(); p != nil {
			r, err = nil, fmt.Errorf("panic: %v", p)
		}
	}()
	r, err = src.Collect(ctx)
	if err == nil && r == nil {
		err = errors.New("returned no reading")
	}
	return r, err
}
