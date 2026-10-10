package cli

import (
	"context"
	"errors"
	"time"

	"github.com/cybagard/cyba-headroom/internal/source/docker"
)

// Eventer streams Docker's container starts and exits: the Docker source.
type Eventer interface {
	Events(ctx context.Context, since int64, fn func(action, id string, timeNano int64, attrs map[string]string)) error
}

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
// Docker refusing the since, it is dropped. Every event, replayed or live,
// reaches fn when it comes, and the book dates it then: the Docker VM's
// clock can lag the host's.
func followEvents(ctx context.Context, src Eventer, fn func(action, id, name string, labels map[string]string), wait func(context.Context, time.Duration)) {
	const maxWait = 30 * time.Second
	backoff := time.Second
	var newest int64 // the newest delivered event's time
	var w window
	for ctx.Err() == nil {
		delivered := false
		since := max(newest-int64(time.Second), 0)
		pinned := w.pinned(since)
		err := src.Events(ctx, since, func(action, id string, timeNano int64, attrs map[string]string) {
			if timeNano > 0 {
				k := eventKey{timeNano, id, action}
				if pinned[k] || w.has(k) {
					return // replayed
				}
				newest = max(newest, timeNano)
				w.add(k, newest)
			}
			delivered = true
			fn(action, id, attrs["name"], attrs)
		})
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
