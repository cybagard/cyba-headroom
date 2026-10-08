// Package lease reserves the cost of allowed calls until their containers or
// VMs show up and use it (R10, #25), so simultaneous requests cannot
// overcommit.
//
// The shim replaces itself with the real binary on allow, so nothing reports
// back: the book recognises the new resource itself, by a key each lease
// gets when its call is checked (#33). A run or create's container carries
// the lease's ID as a label; a start names its container's ID, which the
// daemon asks Docker for; a compose call names its project, or its project
// directory, which Compose labels each container with; a tart run names its
// VM. A new resource binds the lease whose key it matches, and no other: the
// book never guesses. A bound lease keeps reserving what its resources
// have not used yet, so a container that starts small does not hand its
// reservation back at once. A lease ends when its resources use the full
// cost, when they are gone, or at its timeout; one that never saw its
// resource is logged as expired. A lease with no key (headroom check
// without --name, or Docker not answering at a start) never binds: it holds
// its cost to its timeout.
//
// Manual calls (no worktree) are outside admission control: their lease
// reserves nothing and only marks the call as checked. Their containers
// count in the budget once they appear.
//
// A new resource that binds no lease was started without a check: it is
// ungated (R4, #33).
package lease

import (
	"cmp"
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
	// Empty until a reading: no baseline yet.
	prev map[string]bool
	// based says which sources (container, vm) prev holds a fresh reading
	// of: until then, every resource of that source may have been there
	// already.
	based map[string]bool
	// verdicts say, for each resource seen within the lease timeout, how it
	// started (#33). A container keeps its verdict when it comes back after
	// a tick or two away: its stats failed, a restart policy restarted it,
	// or Docker itself restarted.
	verdicts map[string]*verdict
	// dockerDown is set while a fresh reading shows the Docker engine not
	// running; dockerUp is when it was next seen running. Containers that
	// appear in the dockerSettle after that are a baseline: the engine
	// restarts those with a restart policy itself, a tick or two after its
	// API answers.
	dockerDown bool
	dockerUp   time.Time
	// lapsed are leases that expired before their resource appeared (a
	// slow image pull), kept for another timeout: a resource they name is
	// their call's, so not ungated, though they no longer reserve.
	lapsed []*entry
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
	// The lease's key (#33). labelled: its container carries the lease's ID
	// (protocol.LeaseLabel). containerIDs: a start's containers, as Docker
	// resolved them. name: a run's --name, for a call the shim did not label.
	// target: a start's container as given, when Docker could not resolve
	// it, or a tart run's VM. A compose lease's key is its project.
	labelled bool
	// oneoff is set for a compose run's lease: it binds the one one-off
	// container Compose labels as such (hasOneoff once it has), and the
	// services it starts first (depends_on) when no up lease takes them.
	oneoff, hasOneoff bool
	// took are the leases this one took over at its check: a release
	// (its call did not start) gives them back.
	took, tookLapsed []*entry
	containerIDs     []string
	name             string
	target           string
}

// How a resource started (#33).
const (
	baseline = iota // there before headroom could see it start
	gated           // it bound a lease: its call was checked
	ungated         // it bound none
)

type verdict struct {
	how     int
	u       protocol.Ungated // for ungated: what the snapshot lists
	project string           // a compose container's project, and its directory
	dir     string
	last    time.Time // last in a reading
	present bool      // in the latest reading (or a failed read kept it)
}

// reserved is what the lease still holds back: its cost less what its
// resources already use.
func (e *entry) reserved() uint64 { return e.cost - min(e.used, e.cost) }

// New returns an empty book whose leases last timeout.
func New(timeout time.Duration, now func() time.Time, log *slog.Logger) *Book {
	var r [3]byte
	_, _ = rand.Read(r[:])
	return &Book{timeout: timeout, now: now, log: log, run: hex.EncodeToString(r[:]), alive: processAlive,
		verdicts: map[string]*verdict{}, prev: map[string]bool{}, based: map[string]bool{}}
}

// dockerSettle is how long after the Docker engine comes back its
// restart-policy containers count as a baseline.
const dockerSettle = 30 * time.Second

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
	starts := r.Others
	if r.ContainerID != "" {
		starts = append([]policy.Start{{ID: r.ContainerID, TakesOver: r.TakesOver, Running: r.Running}}, r.Others...)
	}
	if len(starts) > 0 && !r.MultiTarget && !slices.ContainsFunc(starts, func(t policy.Start) bool { return !t.Running }) {
		// A start or restart of containers Docker says run: it starts
		// nothing new, and they already count. Allowed, whatever the
		// pressure, and no lease.
		return protocol.Decision{Allow: true, Message: fmt.Sprintf("headroom: allowed `%s` (it already runs)", Summary(r.Command))}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.latest
	if s == nil || (current != nil && current.CollectedAt.After(b.observed.Add(stale))) {
		s = current
	}
	now := b.now()
	b.expire(now) // settling may have stalled: expired leases must not count
	// The containers this start starts: not those that run, nor those an
	// open lease holds (docker stop && docker start): that lease still
	// covers them, and its worktree is charged for them.
	starts = slices.DeleteFunc(slices.Clone(starts), func(t policy.Start) bool {
		return t.Running || b.boundAnywhere("container:"+t.ID)
	})
	if r.ContainerID != "" && len(starts) == 0 && !r.MultiTarget {
		// Allowed, and no new lease. Not when others went unresolved:
		// they are no lease's.
		return protocol.Decision{Allow: true, Message: fmt.Sprintf("headroom: allowed `%s` (its lease holds it)", Summary(r.Command))}
	}
	for _, e := range b.open {
		r.LeasedBytes += e.reserved()
		if e.Worktree == r.Worktree {
			r.WorktreeLeasedBytes += e.reserved()
		}
		if e.macOS && len(e.bound) == 0 {
			r.PendingMacOS++ // once bound, its VM counts as running
		}
	}
	if len(starts) > 1 {
		// docker start a b c: each costs what one would.
		r.CostBytes = uint64(len(starts)) * cmp.Or(r.CostBytes, c.DefaultContainerBytes)
	}
	d := policy.Decide(r, s, c)
	d.LeasedBytes = r.LeasedBytes
	if !d.Allow {
		return d
	}
	if s != nil && b.latest == nil {
		// Before the first Observe, the check's own reading is the
		// baseline, of each source it has one of.
		b.baseline(s)
	}
	b.nextID++
	e := &entry{
		Lease: protocol.Lease{
			ID: fmt.Sprintf("lease-%s-%d", b.run, b.nextID), Worktree: r.Worktree, Kind: r.Kind, Command: Summary(r.Command),
			Created: now, Expires: now.Add(b.timeout),
		},
		cost: d.CostBytes, bound: map[string]bool{}, macOS: r.MacOS, pid: r.PID,
		labelled: r.Labelled, name: r.Name, target: r.Target,
	}
	for _, t := range starts {
		e.containerIDs = append(e.containerIDs, t.ID)
	}
	if r.Kind == "compose" {
		// Its key: the project, -p or as docker compose config names it.
		// Compose labels each container with it.
		e.target, e.project, e.oneoff = "", r.Target, r.Op == "run"
		// compose up again for the project an open lease of this worktree
		// already waits for or holds (compose stop, then up): this call's
		// lease takes that one over, with its containers and its cost.
		var reserved, used uint64
		b.open = slices.DeleteFunc(b.open, func(o *entry) bool {
			if o.Kind != "compose" || e.oneoff || o.oneoff || e.project == "" || o.project != e.project || o.Worktree != r.Worktree {
				// Only its own worktree's, and not a compose run's (its
				// one-off container is new): another's keeps its lease,
				// against its own cap; a manual call reserves nothing.
				return false
			}
			e.took = append(e.took, o)
			reserved, used = reserved+o.reserved(), used+o.used
			for k := range o.bound {
				e.bound[k] = true
			}
			b.log.Debug("lease ended: a later compose call took its project over", "lease", o.ID, "by", e.ID)
			return true
		})
		if len(e.took) > 0 {
			// The same stack again: it holds the larger of this call's
			// estimate and what the old leases still reserved, on top of
			// what their containers use, never below what was held and
			// bounded however often it repeats, with a fresh timeout: this
			// call was admitted, and its containers may be a pull away.
			e.cost, e.used = max(e.cost, reserved)+used, used
		}
	}
	if r.ContainerID != "" && !slices.ContainsFunc(starts, func(t policy.Start) bool { return t.ID == r.ContainerID }) {
		// docker start a b with a running, or held by an open lease: the
		// lease is for the others, so it is not keyed by a's name.
		e.target = ""
	}
	per := d.CostBytes / uint64(max(1, len(starts)))
	for _, t := range starts {
		if t.TakesOver == "" {
			continue
		}
		// A start of a container a run or create made that has not run
		// yet: that lease ends here, and this one keeps the larger cost
		// for that container.
		for _, list := range []*[]*entry{&b.open, &b.lapsed} {
			*list = slices.DeleteFunc(*list, func(o *entry) bool {
				if o.ID != t.TakesOver || !o.labelled || len(o.bound) > 0 || r.Worktree == "" && o.Worktree != "" {
					return false
				}
				if list == &b.open {
					e.cost += max(per, o.cost) - per
					e.took = append(e.took, o)
				} else {
					e.tookLapsed = append(e.tookLapsed, o)
				}
				b.log.Debug("lease ended: its container was started by a later call", "lease", o.ID, "by", e.ID)
				return true
			})
		}
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
	id       string // a container's ID
	lease    string // the lease ID its LeaseLabel carries
	dir      string // a compose container's project directory
	kind     string // container, compose (a compose project's container) or vm
	project  string // the compose project, for compose
	oneoff   bool   // a compose run's one-off container
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
			r := resource{key: k, name: c.Name, id: c.ID, kind: "container", worktree: owner[k], bytes: c.MemoryBytes,
				lease: c.Labels[protocol.LeaseLabel]}
			if p := c.Labels[protocol.ComposeProjectLabel]; p != "" {
				r.kind, r.project, r.dir = "compose", p, c.Labels[protocol.ComposeWorkingDirLabel]
				r.oneoff = c.Labels["com.docker.compose.oneoff"] == "True"
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

// takes reports whether e can bind r: a container or VM lease takes one,
// a macOS one a macOS VM (compose leases take their project's: see key).
func (e *entry) takes(r resource) bool {
	if e.macOS && r.os != "darwin" && r.os != "" {
		return false // a macOS lease's slot is freed by a macOS VM only ("": unknown, counted as one)
	}
	return len(e.bound) == 0
}

// readable reports whether s holds a fresh reading of the source behind
// resources of kind: absence from a failed read means nothing.
func readable(s *protocol.Snapshot, kind string) bool {
	src := "docker"
	if source(kind) == "vm" {
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
	// containers itself: those appearing soon after are a baseline.
	if d := s.Docker; d != nil && readable(s, "container") {
		if b.dockerDown && d.Running {
			b.dockerUp = now
		}
		b.dockerDown = !d.Running
	}
	dockerSettling := !b.dockerUp.IsZero() && now.Sub(b.dockerUp) < dockerSettle
	present := map[string]uint64{}
	for _, r := range res {
		present[r.key] = r.bytes
	}
	// A start keyed by its container's ID binds it whenever it is there,
	// new or not: docker stop && docker start within a tick never leaves
	// the snapshot. The key is exact, so nothing is guessed.
	bound := map[string]bool{}
	for _, e := range b.open {
		for k := range e.bound {
			bound[k] = true
		}
	}
	byID := map[string]resource{}
	for _, r := range res {
		if r.id != "" {
			byID[r.id] = r
		}
	}
	for _, e := range b.open {
		for _, id := range e.containerIDs {
			// A container whose label names an open lease is that lease's:
			// the label outranks an ID.
			if r, ok := byID[id]; ok && !bound[r.key] && keyed(b.open, r, true) == e {
				e.bind(r)
				b.judge(r, gated, now)
				bound[r.key] = true
			}
		}
	}
	var fresh []resource // new this tick, and bound to no lease yet
	for _, r := range res {
		if !b.prev[r.key] && !b.boundAnywhere(r.key) {
			fresh = append(fresh, r)
		}
	}
	// Compose services before one-off containers: a compose run's
	// dependencies are its lease's until its one-off binds, and Docker lists
	// the newest first, whatever started first.
	slices.SortStableFunc(fresh, func(x, y resource) int {
		switch {
		case !x.oneoff && y.oneoff:
			return -1
		case x.oneoff && !y.oneoff:
			return 1
		}
		return 0
	})
	// Each new resource binds the open lease whose key it matches. Before a
	// source's first reading, only a key nothing else can match (a label,
	// a container ID) binds: anything else may have been there already.
	var unbound []resource
	for _, r := range fresh {
		e := keyed(b.open, r, b.based[source(r.kind)])
		if e != nil && e.oneoff && !r.oneoff && b.verdicts[r.key] != nil {
			// A service back after a tick away (a crash loop) is no new
			// dependency of a compose run: it keeps its verdict.
			e = nil
		}
		if e != nil {
			e.bind(r)
			b.judge(r, gated, now)
		} else {
			unbound = append(unbound, r)
		}
	}
	// Compose projects running containers whose call was checked: a later
	// service (depends_on, a slow healthcheck) belongs to the same call.
	// A project is its name and its directory: another stack may share
	// the name.
	gatedProject := map[string]bool{}
	for _, r := range res {
		// A compose run's one-off vouches for no service of its project.
		if v := b.verdicts[r.key]; r.kind == "compose" && !r.oneoff && v != nil && v.how == gated {
			gatedProject[r.project+"\x00"+r.dir] = true
		}
	}
	for _, r := range unbound {
		v := b.verdicts[r.key]
		switch {
		case v != nil && r.kind != "vm":
			// A container back after a tick or two away keeps its verdict.
			// A VM run again is a new run.
		case !b.based[source(r.kind)] || dockerSettling && r.kind != "vm":
			b.judge(r, baseline, now) // it may have been there before
		case r.kind == "compose" && gatedProject[r.project+"\x00"+r.dir] || b.lapsedFor(r):
			b.judge(r, gated, now) // a later service, or its lease lapsed (a slow pull)
		default:
			b.judge(r, ungated, now)
			b.log.Warn("ungated: a container or VM appeared without a check (socket or SDK use, a login shell, an agent not launched through headroom run, or the daemon down)",
				"name", r.name, "kind", r.kind, "worktree", r.worktree)
		}
	}
	for _, v := range b.verdicts {
		v.present = v.present && !readable(s, kindOf(v.u.Key)) // a failed read keeps it
	}
	for _, r := range res {
		v := b.verdicts[r.key]
		if v == nil {
			v = b.judge(r, baseline, now) // there before the first reading
		}
		v.last, v.present = now, true
		v.u.Worktree = r.worktree // attribution can change, or come late
	}
	for k, v := range b.verdicts {
		if !v.present && now.Sub(v.last) >= b.timeout {
			delete(b.verdicts, k)
		}
	}
	b.lapsed = slices.DeleteFunc(b.lapsed, func(e *entry) bool { return !now.Before(e.Expires.Add(b.timeout)) })
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
	b.markBased(s)
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
		case len(e.bound) > 0 && !alive && (e.Kind != "compose" || e.oneoff && e.hasOneoff) && len(e.bound) >= len(e.containerIDs):
			// Its container or VM is gone. A compose project's first
			// container may be a one-shot; the services come after it, as
			// may a start's other containers.
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
			return true
		case e.Worktree == "":
			// A manual call's: it reserved nothing, and many start nothing
			// that lives long enough to be seen (docker run --rm).
			b.log.Debug("manual lease ended at its timeout", "lease", e.ID, "command", e.Command)
		case !e.hasKey():
			// It had nothing to see appear (docker start a b, a running):
			// it held its cost to the timeout, as meant.
			b.log.Debug("lease with no key ended at its timeout", "lease", e.ID, "worktree", e.Worktree, "command", e.Command)
			return true
		default:
			b.log.Warn("lease expired: its container or VM never appeared", "lease", e.ID, "worktree", e.Worktree,
				"command", e.Command, "bytes", e.cost, "age", now.Sub(e.Created).Round(time.Second))
		}
		b.lapsed = append(b.lapsed, e) // its resource may still come (a slow pull)
		return true
	})
}

// baseline adds the resources of each source s has a fresh reading of,
// and no baseline yet, to prev.
func (b *Book) baseline(s *protocol.Snapshot) {
	for _, r := range resources(s) {
		if !b.based[source(r.kind)] && readable(s, r.kind) {
			b.prev[r.key] = true
		}
	}
	b.markBased(s)
}

// markBased records which sources s has a fresh reading of.
func (b *Book) markBased(s *protocol.Snapshot) {
	if s.Docker != nil && readable(s, "container") {
		b.based["container"] = true
	}
	if s.Tart != nil && readable(s, "vm") {
		b.based["vm"] = true
	}
}

// source is the source a resource kind comes from: vm (Tart) or
// container (Docker, compose included).
func source(kind string) string {
	if kind == "vm" {
		return "vm"
	}
	return "container"
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

// keyed is the lease in es whose key r matches. The surest key wins: a
// container's label names its lease outright, before a start's container ID
// or a compose project's name, before a name (docker start db || docker
// run --name db: the run's label beats the start's name). Without a
// baseline (based false), only a label or a container ID counts.
func keyed(es []*entry, r resource, based bool) *entry {
	rank := func(e *entry) int {
		switch {
		case e.labelled:
			return 0
		case len(e.containerIDs) > 0, e.Kind == "compose" && (!e.oneoff || r.oneoff):
			return 1
		}
		return 2 // a name; or a compose run's lease for a service, after any up's
	}
	// Among equal keys, the lease of the worktree r is attributed to: two
	// worktrees may bring up one project.
	better := func(e, best *entry) bool {
		if rank(e) != rank(best) {
			return rank(e) < rank(best)
		}
		if r.worktree != "" {
			return e.Worktree == r.worktree && best.Worktree != r.worktree
		}
		// Unattributed: a worktree's lease, which reserves, before a
		// manual one, which does not.
		return e.Worktree != "" && best.Worktree == ""
	}
	var best *entry
	for _, e := range es {
		if e.key(r, based) && (best == nil || better(e, best)) {
			best = e
		}
	}
	return best
}

// key reports whether r is the resource e's call started.
func (e *entry) key(r resource, based bool) bool {
	switch {
	case r.kind == "vm":
		// A tart run's VM: the one its tart run process runs, whatever the
		// shim guessed its OS to be, else by its name.
		if e.Kind != "tart" || len(e.bound) > 0 {
			return false
		}
		return e.pid > 0 && e.pid == r.runPID || based && e.takes(r) && e.target != "" && e.target == r.name
	case e.labelled:
		// A run's or create's container carries its lease's ID; Compose may
		// have labelled it a project's too (a --label on run).
		return r.lease == e.ID && len(e.bound) == 0
	case e.Kind == "compose" && r.kind == "compose":
		switch {
		case !based || e.project == "" || e.project != r.project:
			return false
		case r.oneoff:
			return e.oneoff && !e.hasOneoff // a compose run's own, one each
		}
		// A service: an up's, or a compose run's dependency, which starts
		// before its one-off container.
		return !e.oneoff || !e.hasOneoff
	case e.Kind == "container" && len(e.containerIDs) > 0:
		return slices.Contains(e.containerIDs, r.id) && !e.bound[r.key] // docker start a b c: each once
	case e.Kind != "container" || len(e.bound) > 0:
		return false
	case e.name != "":
		return based && e.name == r.name // a run checked with headroom check --name
	case e.target != "":
		return based && (e.target == r.name || e.target == r.id) // a start Docker could not resolve
	}
	return false
}

// hasKey reports whether e has anything a resource could match.
func (e *entry) hasKey() bool {
	return e.labelled || len(e.containerIDs) > 0 || e.name != "" || e.target != "" || e.project != "" || e.pid > 0
}

// lapsedFor reports whether a lapsed lease is r's (its key matches), and
// spends it.
func (b *Book) lapsedFor(r resource) bool {
	e := keyed(b.lapsed, r, true)
	if e == nil {
		return false
	}
	b.lapsed = slices.DeleteFunc(b.lapsed, func(o *entry) bool { return o == e })
	return true
}

// judge records how r started.
func (b *Book) judge(r resource, how int, now time.Time) *verdict {
	v := &verdict{how: how, project: r.project, dir: r.dir, last: now, present: true,
		u: protocol.Ungated{Key: r.key, Name: r.name, Kind: r.kind, Worktree: r.worktree, Since: now}}
	b.verdicts[r.key] = v
	return v
}

// bind makes r one of e's resources.
func (e *entry) bind(r resource) {
	e.bound[r.key] = true
	if r.oneoff {
		e.hasOneoff = true
	}
	if e.Kind == "compose" && e.project == "" {
		e.project = r.project
	}
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
	var out []protocol.Ungated
	for _, v := range b.verdicts {
		if v.how == ungated && v.present {
			out = append(out, v.u)
		}
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
	i := slices.IndexFunc(b.open, func(e *entry) bool { return e.ID == id })
	if i < 0 {
		return false
	}
	e := b.open[i]
	b.open = slices.Delete(b.open, i, i+1)
	// The leases it took over at its check are theirs again: this call
	// started nothing.
	b.open = append(b.open, e.took...)
	slices.SortStableFunc(b.open, func(a, b *entry) int { return a.Created.Compare(b.Created) })
	b.lapsed = append(b.lapsed, e.tookLapsed...)
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
