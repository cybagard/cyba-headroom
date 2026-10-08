// Package lease reserves the cost of allowed calls until their containers or
// VMs show up and use it (R10, #25), so simultaneous requests cannot
// overcommit.
//
// The shim replaces itself with the real binary on allow, so nothing reports
// back: the book recognises the new resource itself. Each lease records the
// containers and VMs that existed when it was granted; a new one of its kind
// binds to it, preferring the lease's own worktree. A bound lease keeps
// reserving what its resources have not used yet, so a container that starts
// small does not hand its whole reservation back at once. A lease ends when
// its resources use the full cost, when they are gone, or at its timeout. One
// that never saw its resource is logged as expired.
package lease

import (
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
	// latest is the snapshot Observe last settled the leases against.
	// Checks decide on it, so leases and the resources that replaced them
	// are always seen together.
	latest *protocol.Snapshot
}

type entry struct {
	protocol.Lease
	cost uint64
	// seen are resources that existed, or had their chance to bind, while
	// this lease was open: containers by ID, VMs by name.
	seen map[string]bool
	// bound are this lease's own resources, and used what they use now.
	bound map[string]bool
	used  uint64
}

// reserved is what the lease still holds back: its cost less what its
// resources already use.
func (e *entry) reserved() uint64 { return e.cost - min(e.used, e.cost) }

// New returns an empty book whose leases last timeout.
func New(timeout time.Duration, now func() time.Time, log *slog.Logger) *Book {
	return &Book{timeout: timeout, now: now, log: log}
}

// Check decides r, counting what every open lease still reserves, and leases
// the cost of an allow. It decides on the snapshot the leases were last
// settled against (s only until the first Observe), and the whole step holds
// the book's lock: concurrent checks run one after another, and the second
// sees the first's lease.
func (b *Book) Check(r policy.Request, s *protocol.Snapshot, c policy.Config) protocol.Decision {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.latest != nil {
		s = b.latest
	}
	var leased uint64
	for _, e := range b.open {
		leased += e.reserved()
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
			ID: fmt.Sprintf("lease-%d", b.nextID), Worktree: r.Worktree, Kind: r.Kind, Command: Redact(r.Command),
			Created: at, Expires: at.Add(b.timeout),
		},
		cost: d.CostBytes, seen: map[string]bool{}, bound: map[string]bool{},
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
			out = append(out, resource{key: k, kind: "container", worktree: owner[k], bytes: c.MemoryBytes})
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

// waitsFor is the resource kind a lease binds.
func (e *entry) waitsFor() string {
	if e.Kind == "tart" {
		return "vm"
	}
	return "container" // container and compose
}

// takes reports whether e can bind another resource: compose binds every
// new container of its project, the rest bind one.
func (e *entry) takes() bool { return e.Kind == "compose" || len(e.bound) == 0 }

// Observe binds new resources in s to leases, ends leases whose resources
// use their cost or are gone, and expires those past their timeout. Call it
// on every snapshot before it is published; checks then decide on s.
func (b *Book) Observe(s *protocol.Snapshot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.latest = s
	res := resources(s)
	present := map[string]uint64{}
	for _, r := range res {
		present[r.key] = r.bytes
		if !b.boundAnywhere(r.key) {
			if e := b.match(r); e != nil {
				e.bound[r.key] = true
			}
		}
	}
	now := b.now()
	b.open = slices.DeleteFunc(b.open, func(e *entry) bool {
		e.used = 0
		alive := 0
		for k := range e.bound {
			if bytes, ok := present[k]; ok {
				e.used += bytes
				alive++
			}
		}
		switch {
		case len(e.bound) > 0 && (alive == 0 || e.used >= e.cost):
			return true // its resources use the cost, or are gone
		case now.Before(e.Expires):
			for _, r := range res {
				e.seen[r.key] = true // it had its chance to bind
			}
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

func (b *Book) boundAnywhere(key string) bool {
	return slices.ContainsFunc(b.open, func(e *entry) bool { return e.bound[key] })
}

// match finds the lease r binds to: the oldest open lease of its kind that
// has not seen r and can take it. A lease of r's own worktree comes first,
// then a manual call's lease (a command typed in a worktree's directory),
// and an unattributed r may bind any such lease.
func (b *Book) match(r resource) *entry {
	var manual, other *entry
	for _, e := range b.open {
		if e.seen[r.key] || e.waitsFor() != r.kind || !e.takes() {
			continue
		}
		switch {
		case r.worktree != "" && e.Worktree == r.worktree:
			return e
		case e.Worktree == "" && manual == nil:
			manual = e
		case r.worktree == "" && other == nil:
			other = e
		}
	}
	if manual != nil {
		return manual
	}
	return other
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

// valueFlags take a value that may carry a secret: -e TOKEN=x.
var valueFlags = map[string]bool{"-e": true, "--env": true, "--build-arg": true, "--secret": true}

// Redact drops what a command may carry in the clear before it is kept in a
// lease, the snapshot and the log: every KEY=value becomes KEY=…, including
// after -e, --env, --build-arg and --secret. Long commands are cut.
func Redact(cmd string) string {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		flag, value, isFlagValue := strings.Cut(f, "=")
		switch {
		case isFlagValue && valueFlags[flag]: // --env=TOKEN=x
			if k, _, ok := strings.Cut(value, "="); ok {
				fields[i] = flag + "=" + k + "=…"
			}
		case strings.Contains(f, "="):
			fields[i] = flag + "=…"
		}
	}
	out := strings.Join(fields, " ")
	if r := []rune(out); len(r) > 300 {
		out = string(r[:299]) + "…"
	}
	return out
}
