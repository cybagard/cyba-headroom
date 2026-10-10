package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cybagard/cyba-headroom/internal/source/docker"
)

// Eventer streams Docker's container starts and exits: the Docker source.
type Eventer interface {
	Events(ctx context.Context, since, until int64, fn func(action, id string, timeNano int64, attrs map[string]string)) error
	Clock(ctx context.Context) (vmNano int64, err error)
}

// A reconnect's replay is dated by the Docker VM's clock, read with an RTT
// of at most twice maxOffsetErr, the best of clockReads (#170).
const (
	maxOffsetErr = 125 * time.Millisecond
	clockReads   = 3
)

// replayTimeout bounds a reconnect's clock reads and replay (#170).
var replayTimeout = 5 * time.Second

// followEvents feeds Docker's container events to fn until ctx ends (#67).
// A stream that drops (Docker quit or restarting) is opened again after a
// wait that doubles up to maxWait, and starts at a second again once a
// stream delivered: meanwhile leases bind from readings alone.
//
// A stream opened again asks for the events from a second before the
// newest delivered, which Docker replays, so one dated just before it but
// not yet sent comes too (#146). Docker sends events out of time order, so
// an event is dropped only when it was delivered already: those delivered
// from that second on are pinned, by their time, container and action,
// when the stream opens, so a newer event the replay brings first forgets
// none of them, and one the stream sends twice is in the window (#165).
// Docker refusing the since, it is dropped.
//
// The replay is bounded by the Docker VM's time, read as it opens, and
// each event it brings reaches fn dated when it happened on the host's
// clock: its time plus the VM clock's offset, no earlier than a second
// before the stream dropped and no later than now. The live stream that
// follows asks from a second before the replay's end, and the book dates
// each live event when it comes (a zero at): the VM's clock can lag the
// host's (#170). When the VM's clock cannot be read to within
// maxOffsetErr, has stepped back past the since, or the replay fails, the
// stream opens as before: one stream, every event dated when it comes.
func followEvents(ctx context.Context, src Eventer, fn func(at time.Time, action, id, name string, labels map[string]string), now func() time.Time, log *slog.Logger, wait func(context.Context, time.Duration)) {
	const maxWait = 30 * time.Second
	sec := int64(time.Second)
	backoff := time.Second
	var newest int64 // the newest delivered event's time
	var w window
	// dropAt is when a live stream (until 0) known to have opened, because
	// it delivered an event, last ended. A bounded replay never moves it,
	// nor does a refused or failed connect (#170).
	var dropAt time.Time
	delivered := false
	// stream delivers the events from since to until (0: on), each dated
	// by date, that were not delivered yet.
	stream := func(ctx context.Context, since, until int64, date func(timeNano int64) time.Time) error {
		pinned := w.pinned(since)
		return src.Events(ctx, since, until, func(action, id string, timeNano int64, attrs map[string]string) {
			if timeNano > 0 {
				k := eventKey{timeNano, id, action}
				if pinned[k] || w.has(k) {
					return // replayed
				}
				newest = max(newest, timeNano)
				w.add(k, newest)
			}
			delivered = true
			at := date(timeNano)
			if !at.IsZero() {
				log.Info("docker event replayed", "action", action, "id", id, "at", at)
			}
			fn(at, action, id, attrs["name"], attrs)
		})
	}
	live := func(int64) time.Time { return time.Time{} }
	// replay delivers the events from a second before the newest to the
	// VM's time, dated, and returns that time; ok is false if it did not.
	replay := func() (until int64, ok bool) {
		rctx, cancel := context.WithTimeout(ctx, replayTimeout)
		defer cancel()
		offset, until, err := vmOffset(rctx, src, now)
		if err != nil {
			log.Info("docker events replayed as before", "reason", err)
			return 0, false
		}
		since := newest - sec
		if until < since {
			log.Warn("docker VM clock stepped back", "since", time.Unix(0, since), "vm", time.Unix(0, until))
			return 0, false
		}
		log.Info("docker events replay", "offset", time.Duration(offset), "since", time.Unix(0, since), "until", time.Unix(0, until))
		from := dropAt.Add(-time.Second)
		if err := stream(rctx, since, until, func(timeNano int64) time.Time {
			if timeNano == 0 {
				return time.Time{}
			}
			at := time.Unix(0, timeNano+offset)
			if at.Before(from) {
				at = from
			}
			if n := now(); at.After(n) {
				at = n
			}
			return at
		}); err != nil {
			log.Info("docker events replayed as before", "reason", err)
			return 0, false
		}
		return until, true
	}
	for ctx.Err() == nil {
		delivered = false
		since := max(newest-sec, 0)
		if newest > 0 {
			if until, ok := replay(); ok {
				since = max(until, newest) - sec
			}
			if ctx.Err() != nil {
				return
			}
		}
		replayed := delivered
		delivered = false
		err := stream(ctx, since, 0, live)
		if delivered {
			dropAt = now()
		}
		delivered = delivered || replayed
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, docker.ErrBadSince) {
			newest = 0 // the window stays: a later replay can bring its events
			continue
		}
		if delivered {
			backoff = time.Second
		}
		wait(ctx, backoff)
		backoff = min(2*backoff, maxWait)
	}
}

// vmOffset reads the Docker VM's clock clockReads times, and returns the
// read with the shortest RTT: the host's time halfway through it less the
// VM's, and the VM's. It fails if a read does, or every RTT is over twice
// maxOffsetErr (#170).
func vmOffset(ctx context.Context, src Eventer, now func() time.Time) (offset, vmNano int64, err error) {
	best := time.Duration(-1)
	for range clockReads {
		t0 := now()
		v, err := src.Clock(ctx)
		if err != nil {
			return 0, 0, err
		}
		if rtt := now().Sub(t0); best < 0 || rtt < best {
			best, vmNano = rtt, v
			offset = t0.Add(rtt/2).UnixNano() - v
		}
	}
	if best > 2*maxOffsetErr {
		return 0, 0, fmt.Errorf("docker VM clock read in %v at best", best)
	}
	return offset, vmNano, nil
}

// eventKey is a delivered event as a replay sends it again.
type eventKey struct {
	timeNano   int64
	id, action string
}

// window is what followEvents delivered from a second before the newest
// on, as far back as a reconnect replays, in the order it was delivered:
// a ring of n keys from head, and the same keys as a set (#165).
type window struct {
	ring    []eventKey
	head, n int
	kept    map[eventKey]bool
}

// add keeps k if it is from a second before newest on, and forgets the
// oldest delivered while they are older: each key is kept and forgotten
// once. A key delivered after a newer one is forgotten after it.
func (w *window) add(k eventKey, newest int64) {
	from := newest - int64(time.Second)
	if k.timeNano >= from {
		if w.n == len(w.ring) {
			ring := make([]eventKey, max(2*w.n, 16))
			for i := range w.n {
				ring[i] = w.at(i)
			}
			w.ring, w.head = ring, 0
		}
		w.ring[(w.head+w.n)%len(w.ring)] = k
		w.n++
		if w.kept == nil {
			w.kept = map[eventKey]bool{}
		}
		w.kept[k] = true
	}
	for w.n > 0 && w.at(0).timeNano < from {
		delete(w.kept, w.at(0))
		w.ring[w.head] = eventKey{}
		w.head = (w.head + 1) % len(w.ring)
		w.n--
	}
}

// has is whether k is kept.
func (w *window) has(k eventKey) bool { return w.kept[k] }

// at is the i-th key kept.
func (w *window) at(i int) eventKey { return w.ring[(w.head+i)%len(w.ring)] }

// pinned is the keys from since on.
func (w *window) pinned(since int64) map[eventKey]bool {
	p := map[eventKey]bool{}
	for i := range w.n {
		if k := w.at(i); k.timeNano >= since {
			p[k] = true
		}
	}
	return p
}
