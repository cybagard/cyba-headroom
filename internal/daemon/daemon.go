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
// marked stale; it never delays the others beyond the source timeout.
func (d *Daemon) Tick(ctx context.Context) {
	d.tickMu.Lock()
	defer d.tickMu.Unlock()

	type result struct {
		r    Reading
		err  error
		took time.Duration
	}
	results := make([]result, len(d.sources))
	var wg sync.WaitGroup
	for i, s := range d.sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := d.now()
			r, err := d.collect(ctx, s)
			results[i] = result{r, err, d.now().Sub(start)}
		}()
	}
	wg.Wait()

	next := &protocol.Snapshot{Sources: make(map[string]protocol.SourceStatus, len(d.sources))}
	for i, s := range d.sources {
		res := results[i]
		s.status.Took = res.took
		if res.err != nil {
			s.status.Err = res.err.Error()
			s.status.Stale = true
			d.log.Warn("source failed", "source", s.src.Name(), "err", res.err, "took", res.took)
		} else {
			s.last = res.r
			s.status = protocol.SourceStatus{At: d.now(), Took: res.took}
		}
		if s.last != nil {
			s.last.Apply(next)
		}
		next.Sources[s.src.Name()] = s.status
	}
	d.seq++
	next.Seq = d.seq
	next.CollectedAt = d.now()
	d.snap.Store(next)
}

// collect runs one source under the source timeout. If the source ignores its
// context, collect stops waiting at the deadline; the source is skipped on
// later ticks until that call returns.
func (d *Daemon) collect(ctx context.Context, s *sourceState) (Reading, error) {
	if !s.busy.CompareAndSwap(false, true) {
		return nil, errStillRunning
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	type out struct {
		r   Reading
		err error
	}
	done := make(chan out, 1)
	go func() {
		defer s.busy.Store(false)
		defer func() {
			if p := recover(); p != nil {
				done <- out{err: fmt.Errorf("panic: %v", p)}
			}
		}()
		r, err := s.src.Collect(ctx)
		if err == nil && r == nil {
			err = errors.New("returned no reading")
		}
		done <- out{r, err}
	}()
	select {
	case o := <-done:
		return o.r, o.err
	case <-ctx.Done():
		return nil, fmt.Errorf("timed out after %s", d.timeout)
	}
}
