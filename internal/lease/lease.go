// Package lease reserves the cost of allowed calls until their containers or
// VMs show up and use it (R10, #25), so simultaneous requests cannot
// overcommit.
//
// The shim replaces itself with the real binary on allow, so nothing reports
// back: the book recognises the new resource itself. A resource the book has
// not seen recently is new, and binds the oldest fitting lease, preferring
// the lease's own worktree. A bound lease keeps reserving what its resources
// have not used yet, so a container that starts small does not hand its
// reservation back at once. A lease ends when its resources use the full
// cost, when they are gone, or at its timeout; one that never saw its
// resource is logged as expired.
//
// Manual calls (no worktree) are outside admission control and get no
// lease; their containers count in the budget once they appear.
package lease

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// Book holds the open leases and the snapshot they were last settled on.
type Book struct {
	timeout time.Duration
	now     func() time.Time
	log     *slog.Logger

	mu     sync.Mutex
	open   []*entry // oldest first
	nextID int
	// run tells this book's lease IDs from an earlier daemon run's, so a
	// release for one of those cannot end one of these.
	run string
	// latest is a copy of the snapshot Observe last settled the leases
	// against, observed when. Checks decide on it, so leases and the
	// resources that replaced them are always seen together.
	latest   *protocol.Snapshot
	observed time.Time
	// prev are the resources in the last fresh reading of each source. A
	// resource not among them is new and may bind a lease: one started for
	// the first time, or again after it stopped. A failed read keeps the
	// previous set, so a resource missing from it does not come back new.
	// nil until the first Observe: no baseline yet.
	prev map[string]bool
}

type entry struct {
	protocol.Lease
	cost uint64
	// bound are this lease's own resources, and used what they use now.
	bound map[string]bool
	used  uint64
	// project is the compose project a compose lease locked onto with its
	// first container.
	project string
	// macOS is set for a macOS VM: until its VM runs, it holds a slot (R6).
	macOS bool
}

// reserved is what the lease still holds back: its cost less what its
// resources already use.
func (e *entry) reserved() uint64 { return e.cost - min(e.used, e.cost) }

// New returns an empty book whose leases last timeout.
func New(timeout time.Duration, now func() time.Time, log *slog.Logger) *Book {
	var r [3]byte
	_, _ = rand.Read(r[:])
	return &Book{timeout: timeout, now: now, log: log, run: hex.EncodeToString(r[:])}
}

// stale is how much newer the daemon's snapshot must be than the one leases
// were settled on before checks use it instead: derive (which settles them)
// has stopped running.
const stale = 2 * time.Second

// Check decides r, counting what open leases still reserve, and leases the
// cost of a gated allow. It decides on the snapshot the leases were last
// settled against; current is the daemon's latest, used before the first
// Observe or when it is clearly newer (derive failing). The whole step holds
// the book's lock: concurrent checks run one after another, and the second
// sees the first's lease.
func (b *Book) Check(r policy.Request, current *protocol.Snapshot, c policy.Config) protocol.Decision {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.latest
	if s == nil || (current != nil && current.CollectedAt.After(b.observed.Add(stale))) {
		s = current
	}
	now := b.now()
	b.expire(now) // settling may have stalled: expired leases must not count
	for _, e := range b.open {
		r.LeasedBytes += e.reserved()
		if e.Worktree == r.Worktree {
			r.WorktreeLeasedBytes += e.reserved()
		}
		if e.macOS && len(e.bound) == 0 {
			r.PendingMacOS++ // once bound, its VM counts as running
		}
	}
	d := policy.Decide(r, s, c)
	d.LeasedBytes = r.LeasedBytes
	if !d.Allow || r.Worktree == "" {
		return d // manual calls are not gated, and hold nothing back
	}
	if b.prev == nil && s != nil && (s.Docker != nil || s.Tart != nil) {
		// Before the first Observe, the check's own reading is the baseline.
		b.prev = map[string]bool{}
		for _, r := range resources(s) {
			b.prev[r.key] = true
		}
	}
	b.nextID++
	e := &entry{
		Lease: protocol.Lease{
			ID: fmt.Sprintf("lease-%s-%d", b.run, b.nextID), Worktree: r.Worktree, Kind: r.Kind, Command: Summary(r.Command),
			Created: now, Expires: now.Add(b.timeout),
		},
		cost: d.CostBytes, bound: map[string]bool{}, macOS: r.MacOS,
	}
	b.open = append(b.open, e)
	d.LeaseID = e.ID
	return d
}

// resource is a container or VM in a snapshot.
type resource struct {
	key      string // "container:<id>" or "vm:<name>"
	kind     string // container, compose (a compose project's container) or vm
	project  string // the compose project, for compose
	worktree string // "" when unattributed
	// bytes is what the budget counts for it: a container's memory, a VM's
	// configured memory.
	bytes uint64
}

func resources(s *protocol.Snapshot) []resource {
	owner := map[string]string{}
	if a := s.Attribution; a != nil {
		for _, w := range a.Worktrees {
			for _, c := range w.Containers {
				owner["container:"+c.ID] = w.ID
			}
			for _, vm := range w.TartVMs {
				owner["vm:"+vm.Name] = w.ID
			}
		}
	}
	var out []resource
	if s.Docker != nil {
		for _, c := range s.Docker.Containers {
			k := "container:" + c.ID
			r := resource{key: k, kind: "container", worktree: owner[k], bytes: c.MemoryBytes}
			if p := c.Labels[protocol.ComposeProjectLabel]; p != "" {
				r.kind, r.project = "compose", p
			}
			out = append(out, r)
		}
	}
	if s.Tart != nil {
		for _, vm := range s.Tart.VMs {
			k := "vm:" + vm.Name
			out = append(out, resource{key: k, kind: "vm", worktree: owner[k], bytes: vm.MemoryBytes})
		}
	}
	return out
}

// waitsFor is the resource kind a lease binds: a VM for tart, a compose
// container for compose, a plain container otherwise.
func (e *entry) waitsFor() string {
	switch e.Kind {
	case "tart":
		return "vm"
	case "compose":
		return "compose"
	}
	return "container"
}

// takes reports whether e can bind r: a compose lease takes every container
// of the project its first container belonged to; the rest take one.
func (e *entry) takes(r resource) bool {
	if e.Kind == "compose" {
		return e.project == "" || e.project == r.project
	}
	return len(e.bound) == 0
}

// readable reports whether s holds a fresh reading of the source behind
// resources of kind: absence from a failed read means nothing.
func readable(s *protocol.Snapshot, kind string) bool {
	src := "docker"
	if kind == "vm" {
		src = "tart"
	}
	return !s.Sources[src].Stale
}

// Observe binds new resources in s to leases, ends leases whose resources
// use their cost or are gone, and expires those past their timeout. Call it
// on every snapshot before it is published; checks then decide on it.
func (b *Book) Observe(s *protocol.Snapshot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	res := resources(s)
	present := map[string]uint64{}
	for _, r := range res {
		present[r.key] = r.bytes
		if b.prev[r.key] || b.boundAnywhere(r.key) {
			continue
		}
		// Without a baseline (a lease taken before the first reading), any
		// resource may have been there already: only one attributed to the
		// lease's own worktree is taken as its call's.
		if e := b.match(r, now, b.prev == nil); e != nil {
			e.bound[r.key] = true
			if e.Kind == "compose" && e.project == "" {
				e.project = r.project
			}
		}
	}
	next := map[string]bool{}
	for _, r := range res {
		next[r.key] = true
	}
	for k := range b.prev {
		if !readable(s, kindOf(k)) {
			next[k] = true // a failed read: keep what was there
		}
	}
	b.prev = next
	cp := *s // a copy: the daemon keeps writing to s after this
	b.latest, b.observed = &cp, now

	for _, e := range b.open {
		var used uint64
		unsure := false
		for k := range e.bound {
			if bytes, ok := present[k]; ok {
				used += bytes
			} else if !readable(s, e.waitsFor()) {
				unsure = true
			}
		}
		if !unsure {
			e.used = used // a failed read keeps the last known use
		}
	}
	b.open = slices.DeleteFunc(b.open, func(e *entry) bool {
		alive := false
		for k := range e.bound {
			_, ok := present[k]
			alive = alive || ok || !readable(s, e.waitsFor())
		}
		switch {
		case len(e.bound) > 0 && e.used >= e.cost:
			return true // its resources use the cost
		case len(e.bound) > 0 && !alive && e.Kind != "compose":
			// Its container or VM is gone. A compose project's first
			// container may be a one-shot; the services come after it.
			return true
		}
		return false
	})
	b.expire(now)
}

// expire drops leases past their timeout: quietly if they bound their
// resource, logged if it never appeared (R10).
func (b *Book) expire(now time.Time) {
	b.open = slices.DeleteFunc(b.open, func(e *entry) bool {
		switch {
		case now.Before(e.Expires):
			return false
		case len(e.bound) > 0:
			b.log.Debug("lease ended at its timeout", "lease", e.ID, "worktree", e.Worktree, "command", e.Command)
		default:
			b.log.Warn("lease expired: its container or VM never appeared", "lease", e.ID, "worktree", e.Worktree,
				"command", e.Command, "bytes", e.cost, "age", now.Sub(e.Created).Round(time.Second))
		}
		return true
	})
}

// kindOf is the resource kind a key names, for readable.
func kindOf(key string) string {
	if strings.HasPrefix(key, "vm:") {
		return "vm"
	}
	return "container"
}

func (b *Book) boundAnywhere(key string) bool {
	return slices.ContainsFunc(b.open, func(e *entry) bool { return e.bound[key] })
}

// match finds the lease a new resource r binds to: the oldest open lease of
// its kind that can take it, of r's own worktree, or of any worktree when r
// is unattributed (unless ownOnly). Leases still within their time come before expired ones,
// so a lease whose own call failed cannot take a newer lease's resource on
// its last tick.
func (b *Book) match(r resource, now time.Time, ownOnly bool) *entry {
	for _, live := range []bool{true, false} {
		var other *entry
		for _, e := range b.open {
			if now.Before(e.Expires) != live || e.waitsFor() != r.kind || !e.takes(r) {
				continue
			}
			if r.worktree != "" && e.Worktree == r.worktree {
				return e
			}
			if r.worktree == "" && other == nil && !ownOnly {
				other = e
			}
		}
		if other != nil {
			return other
		}
	}
	return nil
}

// Release ends the open lease id, whose call never started (its exec
// failed); false if no such lease is open.
func (b *Book) Release(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.open)
	b.open = slices.DeleteFunc(b.open, func(e *entry) bool { return e.ID == id })
	if len(b.open) == n {
		return false
	}
	b.log.Debug("lease released: its call did not start", "lease", id)
	return true
}

// List returns the open leases, oldest first, each with what it still
// reserves.
func (b *Book) List() []protocol.Lease {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]protocol.Lease, len(b.open))
	for i, e := range b.open {
		out[i] = e.Lease
		out[i].Bytes = e.reserved()
	}
	return out
}

// Summary names a call without its arguments, for leases, the snapshot and
// the log: its leading words, up to three, stopping at the first flag. That
// is "docker run postgres:17", "docker compose up", "tart run ci-vm". Words
// that could hold a value (with = or @) are left out, so arguments, which
// may carry secrets, are never kept.
func Summary(cmd string) string {
	var words []string
	for _, w := range strings.Fields(cmd) {
		if len(words) == 3 || strings.HasPrefix(w, "-") || strings.ContainsAny(w, "=@") {
			break
		}
		words = append(words, w)
	}
	return strings.Join(words, " ")
}
