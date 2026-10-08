// Package lease reserves the cost of allowed calls until their containers or
// VMs show up (R10, #25), so simultaneous requests cannot overcommit.
//
// The shim replaces itself with the real binary on allow, so nothing reports
// back. The book recognises the new resource itself: each lease records the
// containers and VMs that existed when it was granted, and the first new one
// of its kind settles it, preferring the lease's own worktree. A lease that
// sees nothing expires, and is logged.
package lease

import (
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// Book holds the open leases.
type Book struct {
	timeout time.Duration
	now     func() time.Time
	log     *slog.Logger

	mu     sync.Mutex
	open   []*entry // oldest first
	nextID int
}

type entry struct {
	protocol.Lease
	// seen are the resources that existed (or were already accounted for)
	// while this lease was open: containers by ID, VMs by name.
	seen map[string]bool
}

// New returns an empty book whose leases last timeout.
func New(timeout time.Duration, now func() time.Time, log *slog.Logger) *Book {
	return &Book{timeout: timeout, now: now, log: log}
}

// Check decides r against s, counting every open lease, and leases the cost
// of an allow. The whole step holds the book's lock, so concurrent checks run
// one after another: the second sees the first's lease.
func (b *Book) Check(r policy.Request, s *protocol.Snapshot, c policy.Config) protocol.Decision {
	b.mu.Lock()
	defer b.mu.Unlock()
	var leased uint64
	for _, e := range b.open {
		leased += e.Bytes
	}
	r.LeasedBytes = leased
	d := policy.Decide(r, s, c)
	d.LeasedBytes = leased
	if !d.Allow {
		return d
	}
	b.nextID++
	at := b.now()
	e := &entry{
		Lease: protocol.Lease{
			ID: fmt.Sprintf("lease-%d", b.nextID), Worktree: r.Worktree, Kind: r.Kind, Command: r.Command,
			Bytes: d.CostBytes, Created: at, Expires: at.Add(b.timeout),
		},
		seen: map[string]bool{},
	}
	for _, res := range resources(s) {
		e.seen[res.key] = true
	}
	b.open = append(b.open, e)
	d.LeaseID = e.ID
	return d
}

// resource is a container or VM in a snapshot.
type resource struct {
	key      string // "container:<id>" or "vm:<name>"
	kind     string // container or vm
	worktree string // "" when unattributed
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
			out = append(out, resource{key: k, kind: "container", worktree: owner[k]})
		}
	}
	if s.Tart != nil {
		for _, vm := range s.Tart.VMs {
			k := "vm:" + vm.Name
			out = append(out, resource{key: k, kind: "vm", worktree: owner[k]})
		}
	}
	return out
}

// kind is the resource kind a lease waits for.
func (e *entry) waitsFor() string {
	if e.Kind == "tart" {
		return "vm"
	}
	return "container" // container and compose
}

// Observe settles leases whose resource has appeared in s and expires the
// rest that are too old. Call it with every published snapshot.
func (b *Book) Observe(s *protocol.Snapshot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.open = slices.DeleteFunc(b.open, func(e *entry) bool {
		if now.Before(e.Expires) {
			return false
		}
		b.log.Warn("lease expired: its container or VM never appeared", "lease", e.ID, "worktree", e.Worktree,
			"command", e.Command, "bytes", e.Bytes, "age", now.Sub(e.Created).Round(time.Second))
		return true
	})
	res := resources(s)
	for _, r := range res {
		if e := b.match(r); e != nil {
			b.open = slices.DeleteFunc(b.open, func(x *entry) bool { return x == e })
		}
	}
	// Whatever is there now has had its chance to settle a lease.
	for _, e := range b.open {
		for _, r := range res {
			e.seen[r.key] = true
		}
	}
}

// match finds the lease r settles: the oldest open lease of its kind that has
// not seen r, from r's own worktree first; an unattributed r may settle any
// such lease.
func (b *Book) match(r resource) *entry {
	var fallback *entry
	for _, e := range b.open {
		if e.seen[r.key] || e.waitsFor() != r.kind {
			continue
		}
		if r.worktree != "" && e.Worktree == r.worktree {
			return e
		}
		if r.worktree == "" && fallback == nil {
			fallback = e
		}
	}
	return fallback
}

// List returns the open leases, oldest first.
func (b *Book) List() []protocol.Lease {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]protocol.Lease, len(b.open))
	for i, e := range b.open {
		out[i] = e.Lease
	}
	return out
}
