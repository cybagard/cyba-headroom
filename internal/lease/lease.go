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
	// target and name are what the call starts (shim.Call), so the lease
	// binds its own resource when several appear at once (#33).
	target, name string
	// labelled is set when its container carries the lease's ID
	// (protocol.LeaseLabel): it binds that container and no other.
	labelled bool
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
	project string           // a compose container's project
	last    time.Time        // last in a reading
	present bool             // in the latest reading (or a failed read kept it)
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

// catchAll is how long a manual lease that does not know what it starts
// may bind any unattributed container: briefly, so it cannot hide an
// ungated one for its whole timeout.
const catchAll = 15 * time.Second

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
	if s != nil && len(b.based) < 2 {
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
		target: r.Target, name: r.Name, labelled: r.Labelled,
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
	lease    string // the lease ID its LeaseLabel carries
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
			r := resource{key: k, name: c.Name, image: c.Image, kind: "container", worktree: owner[k], bytes: c.MemoryBytes,
				lease: c.Labels[protocol.LeaseLabel]}
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
	var fresh []resource // new this tick, and bound to no lease yet
	for _, r := range res {
		present[r.key] = r.bytes
		if !b.prev[r.key] && !b.boundAnywhere(r.key) {
			fresh = append(fresh, r)
		}
	}
	// Resources a call names bind first, so another one appearing in the
	// same tick cannot take their lease; then the rest, by kind and
	// worktree. Without a baseline (a lease taken before the first
	// reading), any resource may have been there already: only one of the
	// lease's own worktree is taken as its call's.
	// A container that carries the ID of an open lease of a labelled call
	// went through the shim: it binds that lease. Any other label value
	// counts for nothing: forged, inherited from a committed image, or of
	// a lease long gone (the container is matched as any other).
	var unbound []resource
	for _, r := range fresh {
		e, open := b.labelledLease(r)
		if e == nil {
			unbound = append(unbound, r)
			continue
		}
		// A docker start after its run or create (open or lapsed) that
		// bound nothing: the start's lease takes the container, and the
		// labelled one ends, having started nothing still running.
		if st := b.startNaming(e, r); st != nil {
			st.bind(r)
			if open {
				b.open = slices.DeleteFunc(b.open, func(o *entry) bool { return o == e })
				b.log.Debug("lease ended: its container was started by a later call", "lease", e.ID, "command", e.Command)
			}
		} else if open {
			e.bind(r)
		} // else its lease lapsed while the image pulled: checked, reserving nothing
		b.judge(r, gated, now)
	}
	// Exact names first, across every new resource, then looser ones (an
	// ID prefix, an image), as Docker resolves a name before an ID prefix.
	exact := func(r resource, now time.Time, ownOnly bool) *entry { return b.named(r, now, ownOnly, false) }
	loose := func(r resource, now time.Time, ownOnly bool) *entry { return b.named(r, now, ownOnly, true) }
	for pass, find := range []func(resource, time.Time, bool) *entry{exact, loose, b.match} {
		var left []resource
		for _, r := range unbound {
			if v := b.verdicts[r.key]; pass == 2 && v != nil && r.kind != "vm" {
				// Back after a tick or two away, with its verdict: only a
				// lease that names it takes it, never a guess.
				left = append(left, r)
				continue
			}
			if e := find(r, now, !b.based[source(r.kind)]); e != nil {
				e.bind(r)
				b.judge(r, gated, now)
			} else {
				left = append(left, r)
			}
		}
		unbound = left
	}
	// Compose projects running containers whose call was checked: a later
	// service (depends_on, a slow healthcheck) belongs to the same call.
	gatedProject := map[string]bool{}
	for _, r := range res {
		if v := b.verdicts[r.key]; r.kind == "compose" && v != nil && v.how == gated {
			gatedProject[r.project] = true
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
		case r.kind == "compose" && gatedProject[r.project] || b.lapsedFor(r):
			b.judge(r, gated, now)
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
			b.lapsed = append(b.lapsed, e)
		}
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
		for _, e := range unlabelled(b.open) {
			if now.Before(e.Expires) != live || e.waitsFor() != r.kind || !e.takes(r) || e.namesOther(r) {
				continue
			}
			switch {
			case e.Worktree == "":
				// A manual call's, made anywhere (also in a worktree's
				// directory): last, after every worktree's own lease. One
				// that knows its target takes only that (named).
				// One that does not, only an unattributed resource.
				if manual == nil && !ownOnly && e.target == "" && e.name == "" && r.worktree == "" && now.Sub(e.Created) < catchAll {
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

// labelledLease is the lease r's label names, if it is a labelled call's
// (a run or create, so a container; one a compose label marks too) that
// has bound nothing: an open one, or a lapsed one after a slow pull,
// which is spent.
func (b *Book) labelledLease(r resource) (e *entry, open bool) {
	if r.lease == "" || source(r.kind) != "container" {
		return nil, false
	}
	is := func(e *entry) bool { return e.ID == r.lease && e.labelled && len(e.bound) == 0 }
	if i := slices.IndexFunc(b.open, is); i >= 0 {
		return b.open[i], true
	}
	if i := slices.IndexFunc(b.lapsed, is); i >= 0 {
		e = b.lapsed[i]
		b.lapsed = slices.Delete(b.lapsed, i, i+1)
		return e, false
	}
	return nil, false
}

// startNaming is an open docker start or restart lease of e's worktree,
// taken after e, that names r: the start runs what e's run or create made.
// One taken before e is not (docker start db || docker run --name db).
func (b *Book) startNaming(e *entry, r resource) *entry {
	for _, st := range unlabelled(b.open) {
		if isStart(st.Command) && st.Worktree == e.Worktree && st.Created.After(e.Created) &&
			st.waitsFor() == "container" && st.takes(r) && st.names(r, false) {
			return st
		}
	}
	return nil
}

// unlabelled leaves out the leases of labelled calls: their container
// carries their ID, so only labelledLease binds it, never a guess.
func unlabelled(es []*entry) []*entry {
	var out []*entry
	for _, e := range es {
		if !e.labelled {
			out = append(out, e)
		}
	}
	return out
}

// lapsedFor reports whether a lapsed lease is r's, and spends it: one its
// call names, or else one of r's own worktree.
func (b *Book) lapsedFor(r resource) bool {
	lapsed := unlabelled(b.lapsed)
	i := slices.IndexFunc(lapsed, func(e *entry) bool { return e.waitsFor() == r.kind && e.takes(r) && e.names(r, true) })
	if i < 0 {
		i = slices.IndexFunc(lapsed, func(e *entry) bool {
			return e.waitsFor() == r.kind && e.takes(r) && r.worktree != "" && e.Worktree == r.worktree && !e.namesOther(r)
		})
	}
	if i < 0 {
		return false
	}
	spent := lapsed[i]
	b.lapsed = slices.DeleteFunc(b.lapsed, func(e *entry) bool { return e == spent })
	return true
}

// judge records how r started.
func (b *Book) judge(r resource, how int, now time.Time) *verdict {
	v := &verdict{how: how, project: r.project, last: now, present: true,
		u: protocol.Ungated{Key: r.key, Name: r.name, Kind: r.kind, Worktree: r.worktree, Since: now}}
	b.verdicts[r.key] = v
	return v
}

// named finds the open lease whose call names r, live ones first: it is
// r's, whatever the worktrees say. Without a baseline (ownOnly) only an
// exact --name, or a lease of r's own worktree, names it.
func (b *Book) named(r resource, now time.Time, ownOnly, loose bool) *entry {
	for _, live := range []bool{true, false} {
		var other *entry
		for _, e := range unlabelled(b.open) {
			if now.Before(e.Expires) != live || e.waitsFor() != r.kind || !e.takes(r) || !e.names(r, loose) {
				continue
			}
			own := r.worktree != "" && e.Worktree == r.worktree
			exact := e.name != "" && e.name == r.name
			switch {
			case own || exact:
				return e // r's own worktree's lease before another's
			case !ownOnly && other == nil:
				other = e
			}
		}
		if other != nil {
			return other
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
// (start), its VM or its compose project; loose also takes a start's ID
// prefix and a run's image.
func (e *entry) names(r resource, loose bool) bool {
	switch {
	case r.kind == "vm":
		return e.target != "" && e.target == r.name
	case r.kind == "compose":
		return e.project != "" && e.project == r.project // -p, or its first container's
	case e.name != "":
		return e.name == r.name
	case e.target == "":
		return false
	}
	if e.target == r.name {
		return true
	}
	if !loose {
		return false
	}
	if isStart(e.Command) {
		// docker start takes a container's ID, or a unique prefix of it.
		id := strings.TrimPrefix(r.key, "container:")
		return strings.HasPrefix(id, e.target)
	}
	// An image is no name: only in the lease's own worktree, or where the
	// worktree is unknown, does it say the container is the call's.
	ownSide := e.Worktree == "" || r.worktree == "" || e.Worktree == r.worktree
	return ownSide && sameImage(e.target, r.image)
}

// isStart reports whether a call summary is a docker or podman start or
// restart, whose target is a container rather than an image.
func isStart(command string) bool { return isOp(command, "start") || isOp(command, "restart") }

// isOp reports whether a call summary ("docker container create db") is
// of subcommand op.
func isOp(command, op string) bool {
	w := strings.Fields(command)
	if len(w) > 2 && w[1] == "container" {
		w = w[1:]
	}
	return len(w) > 1 && w[1] == op
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
		for _, p := range []string{"docker.io/", "index.docker.io/", "localhost/", "library/"} {
			s = strings.TrimPrefix(s, p) // podman lists local images as localhost/
		}
		if i := strings.LastIndex(s, "/"); !strings.Contains(s[i+1:], ":") && !strings.Contains(s, "@") {
			s += ":latest"
		}
		return s
	}
	if a == "" || b == "" {
		return false
	}
	// The same repository and tag under another registry (podman's
	// unqualified search: myimg runs as quay.io/org/myimg).
	base := func(s string) string { return s[strings.LastIndex(s, "/")+1:] }
	return norm(a) == norm(b) || base(norm(a)) == base(norm(b))
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
