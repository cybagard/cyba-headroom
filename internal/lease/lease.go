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
// Manual calls (no worktree) are outside admission control: their lease
// reserves nothing and only marks the call as checked. Their containers
// count in the budget once they appear.
//
// A new resource that binds no lease was started without a check: it is
// ungated (R4, #33).
package lease

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"syscall"
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
	// alive reports whether a process runs (a tart lease's tart run).
	alive func(pid int) bool
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
	// ungated are new resources that bound no lease, by key (#33).
	ungated map[string]protocol.Ungated
	// seen is when each resource was last in a reading. One seen within the
	// lease timeout is not flagged when it comes back: its stats failed for
	// a tick, or a restart policy brought it back after a crash.
	seen map[string]time.Time
	// dockerDown is set while a fresh reading shows the Docker engine not
	// running: its first reading after that is a baseline, since the
	// engine restarts containers with a restart policy itself.
	dockerDown bool
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
	// pid is the tart run process, for a tart lease: if it exits before its
	// VM appears, the run failed.
	pid int
	// target and name are what the call starts (shim.Call), so the lease
	// binds its own resource when several appear at once (#33).
	target, name string
}

// reserved is what the lease still holds back: its cost less what its
// resources already use.
func (e *entry) reserved() uint64 { return e.cost - min(e.used, e.cost) }

// New returns an empty book whose leases last timeout.
func New(timeout time.Duration, now func() time.Time, log *slog.Logger) *Book {
	var r [3]byte
	_, _ = rand.Read(r[:])
	return &Book{timeout: timeout, now: now, log: log, run: hex.EncodeToString(r[:]), alive: processAlive,
		ungated: map[string]protocol.Ungated{}, seen: map[string]time.Time{}}
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
	if !d.Allow {
		return d
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
		cost: d.CostBytes, bound: map[string]bool{}, macOS: r.MacOS, pid: r.PID,
		target: r.Target, name: r.Name,
	}
	if r.Kind == "compose" {
		e.project = r.Target // -p, when given: the lease waits for that project
	}
	if r.Worktree == "" {
		// Manual calls are not gated and hold nothing back: the lease
		// only marks the call as checked, so its container is not ungated.
		e.cost, e.macOS = 0, false
	}
	b.open = append(b.open, e)
	d.LeaseID = e.ID
	return d
}

// resource is a container or VM in a snapshot.
type resource struct {
	key      string // "container:<id>" or "vm:<name>"
	name     string
	image    string // a container's image
	kind     string // container, compose (a compose project's container) or vm
	project  string // the compose project, for compose
	worktree string // "" when unattributed
	os       string // a VM's OS
	runPID   int    // a VM's tart run process
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
			r := resource{key: k, name: c.Name, image: c.Image, kind: "container", worktree: owner[k], bytes: c.MemoryBytes}
			if p := c.Labels[protocol.ComposeProjectLabel]; p != "" {
				r.kind, r.project = "compose", p
			}
			out = append(out, r)
		}
	}
	if s.Tart != nil {
		for _, vm := range s.Tart.VMs {
			k := "vm:" + vm.Name
			out = append(out, resource{key: k, name: vm.Name, kind: "vm", worktree: owner[k], bytes: vm.MemoryBytes, os: vm.OS, runPID: vm.RunPID})
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
	if e.macOS && r.os != "darwin" && r.os != "" {
		return false // a macOS lease's slot is freed by a macOS VM only ("": unknown, counted as one)
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
	// The Docker engine back after a fresh reading showed it down restarts
	// containers itself: this reading is their baseline, not ungated.
	dockerBack := false
	if d := s.Docker; d != nil && readable(s, "container") {
		dockerBack = b.dockerDown && d.Running
		b.dockerDown = !d.Running
	}
	present := map[string]uint64{}
	var fresh []resource // new this tick, and bound to no lease yet
	for _, r := range res {
		present[r.key] = r.bytes
		if u, ok := b.ungated[r.key]; ok {
			u.Worktree = r.worktree // attribution can change, or come late
			b.ungated[r.key] = u
		}
		if !b.prev[r.key] && !b.boundAnywhere(r.key) {
			fresh = append(fresh, r)
		}
	}
	// Resources a call names bind first, so another one appearing in the
	// same tick cannot take their lease.
	var rest []resource
	for _, r := range fresh {
		if e := b.named(r, now); e != nil {
			e.bind(r)
		} else {
			rest = append(rest, r)
		}
	}
	// Compose projects already running gated containers: a later service of
	// one (depends_on, a slow healthcheck) belongs to the same call.
	gatedProject := map[string]bool{}
	for _, r := range res {
		if r.kind == "compose" && (b.prev[r.key] || b.boundAnywhere(r.key)) && b.ungated[r.key].Key == "" {
			gatedProject[r.project] = true
		}
	}
	for _, r := range rest {
		// Without a baseline (a lease taken before the first reading), any
		// resource may have been there already: only one attributed to the
		// lease's own worktree is taken as its call's.
		if e := b.match(r, now, b.prev == nil); e != nil {
			e.bind(r)
			continue
		}
		if b.prev == nil || dockerBack && r.kind != "vm" {
			continue // a baseline: it may have been there before
		}
		if t, ok := b.seen[r.key]; ok && now.Sub(t) < b.timeout || r.kind == "compose" && gatedProject[r.project] {
			continue
		}
		if _, ok := b.ungated[r.key]; !ok {
			b.ungated[r.key] = protocol.Ungated{Key: r.key, Name: r.name, Kind: r.kind, Worktree: r.worktree, Since: now}
			b.log.Warn("ungated: a container or VM appeared without a check (socket or SDK use, a login shell, an agent not launched through headroom run, or the daemon down)",
				"name", r.name, "kind", r.kind, "worktree", r.worktree)
		}
	}
	for k := range b.ungated {
		if _, ok := present[k]; !ok && readable(s, kindOf(k)) {
			delete(b.ungated, k)
		}
	}
	for _, r := range res {
		b.seen[r.key] = now
	}
	for k, t := range b.seen {
		if now.Sub(t) >= b.timeout {
			delete(b.seen, k)
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
		case e.Kind == "tart" && len(e.bound) == 0 && e.pid > 0 && !b.alive(e.pid):
			b.log.Info("lease ended: its tart run exited before its VM appeared", "lease", e.ID, "worktree", e.Worktree,
				"command", e.Command)
			return true
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
		case e.Worktree == "":
			// A manual call's: it reserved nothing, and many start nothing
			// that lives long enough to be seen (docker run --rm).
			b.log.Debug("manual lease ended at its timeout", "lease", e.ID, "command", e.Command)
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
	// A VM whose tart run is a lease's own process is that lease's: the
	// shim became tart run, keeping its PID.
	if r.runPID > 0 {
		for _, e := range b.open {
			if e.pid == r.runPID && e.waitsFor() == r.kind && len(e.bound) == 0 {
				return e // its own VM, whatever the shim guessed its OS to be
			}
		}
	}
	for _, live := range []bool{true, false} {
		var other, manual *entry
		for _, e := range b.open {
			if now.Before(e.Expires) != live || e.waitsFor() != r.kind || !e.takes(r) || e.namesOther(r) {
				continue
			}
			switch {
			case e.Worktree == "":
				// A manual call's, made anywhere (also in a worktree's
				// directory): last, after every worktree's own lease. One
				// that knows its target takes only that (named).
				if manual == nil && !ownOnly && e.target == "" && e.name == "" {
					manual = e
				}
			case r.worktree != "" && e.Worktree == r.worktree:
				return e
			case r.worktree == "" && other == nil && !ownOnly:
				other = e
			}
		}
		if other != nil {
			return other
		}
		if manual != nil {
			return manual
		}
	}
	return nil
}

// named finds the open lease whose call names r, live ones first: it is
// r's, whatever the worktrees say.
func (b *Book) named(r resource, now time.Time) *entry {
	for _, live := range []bool{true, false} {
		for _, e := range b.open {
			if now.Before(e.Expires) == live && e.waitsFor() == r.kind && e.takes(r) && e.names(r) {
				return e
			}
		}
	}
	return nil
}

// bind makes r one of e's resources.
func (e *entry) bind(r resource) {
	e.bound[r.key] = true
	if e.Kind == "compose" && e.project == "" {
		e.project = r.project
	}
}

// names reports whether e's call names r: its --name, its container
// (start), its image, or its VM.
func (e *entry) names(r resource) bool {
	switch {
	case r.kind == "vm":
		return e.target != "" && e.target == r.name
	case r.kind == "compose":
		return false // takes already keeps a compose lease to its project
	case e.name != "":
		return e.name == r.name
	case e.target == "":
		return false
	}
	if e.target == r.name {
		return true
	}
	if isStart(e.Command) {
		// docker start takes a container's ID, or a unique prefix of it.
		id := strings.TrimPrefix(r.key, "container:")
		return len(e.target) >= 4 && strings.HasPrefix(id, e.target)
	}
	// An image is no name: only in the lease's own worktree, or where the
	// worktree is unknown, does it say the container is the call's.
	ownSide := e.Worktree == "" || r.worktree == "" || e.Worktree == r.worktree
	return ownSide && sameImage(e.target, r.image)
}

// isStart reports whether a call summary is a docker or podman start or
// restart, whose target is a container rather than an image.
func isStart(command string) bool {
	w := strings.Fields(command)
	if len(w) > 2 && w[1] == "container" {
		w = w[1:]
	}
	return len(w) > 1 && (w[1] == "start" || w[1] == "restart")
}

// namesOther reports whether e's call names a different container: a
// --name is exact. An image is not (mirrors, digests), so it only orders.
func (e *entry) namesOther(r resource) bool {
	return r.kind == "container" && e.name != "" && e.name != r.name
}

// sameImage reports whether two image references name the same image, as
// Docker reads a short one: docker.io/library/ and :latest are implied.
func sameImage(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimPrefix(s, "docker.io/")
		s = strings.TrimPrefix(s, "library/")
		if i := strings.LastIndex(s, "/"); !strings.Contains(s[i+1:], ":") && !strings.Contains(s, "@") {
			s += ":latest"
		}
		return s
	}
	return a != "" && b != "" && norm(a) == norm(b)
}

// processAlive reports whether pid runs: signal 0 checks without sending.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Ungated returns the containers and VMs that appeared without a check
// and still run, oldest first.
func (b *Book) Ungated() []protocol.Ungated {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]protocol.Ungated, 0, len(b.ungated))
	for _, u := range b.ungated {
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b protocol.Ungated) int {
		if c := a.Since.Compare(b.Since); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out
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

// List returns the open leases of worktrees' calls, oldest first, each
// with what it still reserves. Manual calls' leases reserve nothing and
// are left out.
func (b *Book) List() []protocol.Lease {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]protocol.Lease, 0, len(b.open))
	for _, e := range b.open {
		if e.Worktree == "" {
			continue // a manual call's: it reserves nothing
		}
		l := e.Lease
		l.Bytes = e.reserved()
		out = append(out, l)
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
