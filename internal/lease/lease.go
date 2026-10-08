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
	"regexp"
	"slices"
	"strconv"
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
	// prev are the resources in latest. A resource not among them is new:
	// it can bind a lease.
	prev map[string]bool
}

type entry struct {
	protocol.Lease
	cost uint64
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
// settled against; fallback is used only before the first Observe. The whole
// step holds the book's lock: concurrent checks run one after another, and
// the second sees the first's lease.
func (b *Book) Check(r policy.Request, fallback *protocol.Snapshot, c policy.Config) protocol.Decision {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.latest
	if s == nil {
		s = fallback
		if b.prev == nil {
			b.prev = keys(resources(s))
		}
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
	command := Redact(r.Command)
	if len(r.Args) > 0 {
		command = RedactArgs(r.Args)
	}
	e := &entry{
		Lease: protocol.Lease{
			ID: fmt.Sprintf("lease-%d", b.nextID), Worktree: r.Worktree, Kind: r.Kind, Command: command,
			Created: at, Expires: at.Add(b.timeout),
		},
		cost: d.CostBytes, bound: map[string]bool{},
	}
	b.open = append(b.open, e)
	d.LeaseID = e.ID
	return d
}

// resource is a container or VM in a snapshot.
type resource struct {
	key      string // "container:<id>" or "vm:<name>"
	kind     string // container, compose (a compose project's container) or vm
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
			kind := "container"
			if c.Labels[composeProjectLabel] != "" {
				kind = "compose"
			}
			out = append(out, resource{key: k, kind: kind, worktree: owner[k], bytes: c.MemoryBytes})
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

// composeProjectLabel marks a container that Docker Compose created.
const composeProjectLabel = "com.docker.compose.project"

func keys(rs []resource) map[string]bool {
	m := make(map[string]bool, len(rs))
	for _, r := range rs {
		m[r.key] = true
	}
	return m
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

// takes reports whether e can bind another resource: compose binds every
// new compose container, the rest bind one.
func (e *entry) takes() bool { return e.Kind == "compose" || len(e.bound) == 0 }

// Observe binds new resources in s to leases, ends leases whose resources
// use their cost, and expires those past their timeout. Call it on every
// snapshot before it is published; checks then decide on s.
func (b *Book) Observe(s *protocol.Snapshot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	res := resources(s)
	present := map[string]uint64{}
	for _, r := range res {
		present[r.key] = r.bytes
		if b.prev != nil && !b.prev[r.key] && !b.boundAnywhere(r.key) {
			if e := b.match(r, now); e != nil {
				e.bound[r.key] = true
			}
		}
	}
	b.latest, b.prev = s, keys(res)
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
		case len(e.bound) > 0 && e.used >= e.cost:
			return true // its resources use the cost
		case len(e.bound) > 0 && alive == 0 && e.Kind != "compose":
			// Its container or VM is gone. A compose project's first
			// container may be a one-shot; the services come after it.
			return true
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

func (b *Book) boundAnywhere(key string) bool {
	return slices.ContainsFunc(b.open, func(e *entry) bool { return e.bound[key] })
}

// match finds the lease a new resource r binds to: the oldest open lease of
// its kind that can take it. A lease of r's own worktree comes first, then a
// manual call's lease (a command typed in a worktree's directory), and an
// unattributed r may bind any such lease. Leases still within their time
// come before expired ones, so a lease whose own call failed cannot take a
// newer lease's resource on its last tick.
func (b *Book) match(r resource, now time.Time) *entry {
	for _, live := range []bool{true, false} {
		var manual, other *entry
		for _, e := range b.open {
			if now.Before(e.Expires) != live || e.waitsFor() != r.kind || !e.takes() {
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
		if other != nil {
			return other
		}
	}
	return nil
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

// credentials matches user:password@ in a URL.
var credentials = regexp.MustCompile(`://[^/@]+@`)

// RedactArgs is Redact for an argv: each argument is redacted whole, so a
// quoted secret with spaces cannot leak its tail, and URLs lose their
// credentials. Arguments with spaces are shown quoted.
func RedactArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		a = credentials.ReplaceAllString(a, "://…@")
		flag, value, hasEq := strings.Cut(a, "=")
		switch {
		case hasEq && valueFlags[flag]: // --env=TOKEN=x
			if k, _, ok := strings.Cut(value, "="); ok {
				a = flag + "=" + k + "=…"
			}
		case hasEq && !strings.Contains(flag, " "):
			a = flag + "=…"
		case i > 0 && valueFlags[args[i-1]] && hasEq:
			a = flag + "=…"
		}
		if strings.ContainsAny(a, " \t") {
			a = strconv.Quote(a)
		}
		out[i] = a
	}
	s := strings.Join(out, " ")
	if r := []rune(s); len(r) > 300 {
		s = string(r[:299]) + "…"
	}
	return s
}

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
