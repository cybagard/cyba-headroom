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
// resource is logged as expired. A lease with no key (entry.hasKey) never
// binds, the book keeping nothing a new resource could match: a worktree's
// holds its cost to its timeout. A headroom check sends no key, nor does
// the shim for a call whose resource it cannot name: for example a compose
// call with no project (podman compose without -p, or a file on stdin the
// shim cannot name), or a start whose target follows an option the shim
// does not know. A start is keyed by the IDs of the containers Docker
// resolved that it starts (entry.containerIDs), and by its first target
// as given, a name or an ID, when Docker did not resolve it
// (entry.target). It has neither when its first target was resolved or
// missing and Docker resolved none of the targets it starts (docker start
// a b, a running, b unresolved). It leaves out of its key and cost those
// that run, and those that a worktree's open lease has bound (not as
// entry.held) or lists in its containerIDs (Book.covered). A compose
// project the shim guessed, Compose's config having failed (#84), binds
// only containers its own worktree's reading shows, and when it binds
// none, ends quietly at its timeout and lapses (entry.guessed).
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
	"math"
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
	// previous set, so a resource missing from it does not come back new;
	// so does a reading a container is only missing from (Book.gone),
	// though when it is back it may bind a lease, held (Observe).
	// Empty until a reading: no baseline yet.
	prev map[string]bool
	// based says which sources (container, vm) prev holds a fresh reading
	// of: until then, every resource of that source may have been there
	// already.
	based map[string]bool
	// seen are the containers Docker's events said started or exited
	// (#67), until a reading begun after the event shows them.
	seen map[string]seen
	// missed counts, for each container or VM in prev or bound by an open
	// lease, the fresh readings in a row it was missing from; a die sets it
	// to gone (Book.gone). missedSince is when the first of those readings
	// began: when it died, for Book.markDead.
	missed      map[string]int
	missedSince map[string]time.Time
	// verdicts say, for each resource seen within the lease timeout (a
	// crashed container's within crashKept, if longer), how it started
	// (#33). A container keeps its verdict when it comes back after
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
	// held are those of a stack a compose up found running, held by no
	// lease: bound, so they are gated, but neither the cost nor used counts
	// them, nor does the lease cover a start of them (covered). Their
	// memory counts already: a down does not add it back, and their growth
	// is not what the lease waits for.
	bound, held map[string]bool
	used        uint64
	// found is set once it held a stack, or a start of several let one of
	// its containers go: one it lets go of when it stops (Book.release,
	// entry.letGo) still appeared, so the lease ends quietly.
	found bool
	// dead are a start of several's containers that died while it waited
	// on others, each with its deaths since a reading showed it
	// (Book.markDead).
	dead map[string]deaths
	// project is the compose project a compose lease locked onto with its
	// first container.
	project string
	// macOS is set for a macOS VM: until its VM runs, it holds a slot (R6).
	macOS bool
	// pid is the tart run process, for a tart lease: if it exits before its
	// VM appears, the run failed.
	pid int
	// guessed is set for a compose lease whose project the shim guessed
	// (policy.Request.Guessed, #84). The guess may name another stack, so
	// it stays a guess for the life of the lease: it binds only a container
	// the reading attributes to its own worktree (key), and loses one in
	// that worktree to a lease that is not a guess (keyed). An event, which
	// does not say whose a container is, binds none to another worktree's
	// lease that may be the guess's (tied): the reading decides, as it does
	// for two real keys. It holds no stack at its check (one running
	// says nothing of a hit) and takes no lease over. One it bound shows it
	// hit: a later up of the project in its worktree takes it over as any
	// repeat up does (composeTakes). One that bound nothing missed, its
	// stack was another lease's, or it still pulls: it ends quietly at its
	// timeout and lapses as a real key's does (expire), so a hit after its
	// timeout is gated, as its own worktree's reading shows (lapsedFor
	// goes through key): as for a real key, the lapsed lease vouches for
	// one container per reading (#145). A miss lapses too, holding
	// nothing. A hit whose reading does not attribute it is judged as with
	// no key: binding an unattributed container to a guess could hide
	// another worktree's unchecked stack. An unchecked container of the
	// guessed project in its own worktree binds it, as a real key's would:
	// labels cannot tell a checked container from an unchecked one of one
	// project in one worktree.
	guessed bool
	// The lease's key (#33). labelled: its container carries the lease's ID
	// (protocol.LeaseLabel). containerIDs: a start's containers, as Docker
	// resolved them. name: a run's or create's --name, for a client that
	// sends it without Labelled (the shim labels every run and create it
	// parses; a labelled lease is keyed by its label alone). target: a
	// start's container as given, when Docker could not resolve it, or a
	// tart run's VM. A compose lease's key is its project.
	labelled bool
	// oneoff is set for a compose run's lease: it binds the one one-off
	// container Compose labels as such (hasOneoff once it has), and the
	// services it starts first (depends_on) when no up lease takes them.
	oneoff, hasOneoff bool
	// idle is set for a compose call Compose's dry run said starts
	// nothing: it waits for nothing, nor for what an up it took over
	// waited for (Compose saw that running too), so binding nothing (other
	// leases hold the stack) is no failure.
	idle bool
	// starting are the services of its stack that were down at its check,
	// which its call starts (Book.downServices).
	starting map[string]bool
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
	service string // a compose container's service
	oneoff  bool   // a compose run's one-off container
	// owner is the worktree the last reading that attributed it named: an
	// event says nothing of whose a container is (#89).
	owner string
	lease string // the worktree of the lease it last bound
	// crashed is set when it died without a stop, until a reading begun
	// after its last event shows it again: one begun before the die may
	// list it still. Until then its restart binds as bindsAfterCrash says,
	// and the verdict is kept for crashKept if that outlasts the lease
	// timeout: a restart policy's restart may come later than the timeout.
	crashed bool
	last    time.Time // last in a reading
	event   time.Time // its last start or die event
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
		verdicts: map[string]*verdict{}, prev: map[string]bool{}, based: map[string]bool{}, seen: map[string]seen{}, missed: map[string]int{}, missedSince: map[string]time.Time{}}
}

// dockerSettle is how long after the Docker engine comes back its
// restart-policy containers count as a baseline.
const dockerSettle = 30 * time.Second

// crashKept is how long a crashed container's verdict lasts at least,
// counted from the later of the last reading that listed it and its last
// start or die event: the engine's restart backoff (Docker caps each delay
// at a minute; Podman restarts at once) plus a reading and the start. Each
// death in a crash loop starts it again (#134).
const crashKept = 2 * time.Minute

// stale is how much newer the daemon's snapshot must be than the one leases
// were settled on before checks use it instead: derive (which settles them)
// has stopped running.
const stale = 2 * time.Second

// composeTakes reports whether the lease of compose call r takes o over:
// an open lease of the same project in its own worktree, neither a compose
// run's (its one-off container is new). Another worktree's keeps its
// lease, against its own cap; a manual call reserves nothing. A guess takes
// nothing over, and is taken over once it hit (entry.guessed).
func composeTakes(r policy.Request, o *entry) bool {
	return r.Kind == "compose" && r.Op != "run" && r.Target != "" && !r.Guessed && o.Kind == "compose" && !o.oneoff && (!o.guessed || len(o.bound) > 0) && o.project == r.Target && o.Worktree == r.Worktree
}

// downServices are the services of compose project r.Target in
// r.Worktree, one-offs aside, whose containers the book last saw stop, die
// or go, and that none of its containers runs: an up starts them again
// (#112).
func (b *Book) downServices(r policy.Request) map[string]bool {
	up, down := map[string]bool{}, map[string]bool{}
	for k, v := range b.verdicts {
		if v.project != r.Target || v.oneoff || cmp.Or(v.owner, v.lease) != r.Worktree {
			continue
		}
		if svc := cmp.Or(v.service, k); b.runs(k, v) {
			up[svc] = true
		} else {
			down[svc] = true
		}
	}
	for svc := range up {
		delete(down, svc)
	}
	return down
}

// runs reports whether k, a container with verdict v, runs as far as the
// book knows: Docker's last event says so, else the latest reading shows
// it, or it is only missing (Book.gone).
func (b *Book) runs(k string, v *verdict) bool {
	if sn, ok := b.seen[k]; ok {
		return !sn.gone
	}
	return v.present || b.missed[k] > 0 && !b.gone(k)
}

// deadUse is what o's containers that died since the reading use showed
// them used then: o.used still counts it.
func (b *Book) deadUse(o *entry, use map[string]uint64) uint64 {
	var n uint64
	for k := range o.bound {
		if v := b.verdicts[k]; !o.held[k] && v != nil && !b.runs(k, v) {
			n += use[k]
		}
	}
	return n
}

// takeover is what a compose up's lease carries over from a lease it takes
// while services of its stack are down (Book.carry).
type takeover struct{ carry, used uint64 }

// carry is what o, a lease a compose up takes over while services of its
// stack are down, still reserves for its services that run, and what their
// containers use (use, by key), as the takeover in Check says. Its cost is
// split equally among its services: those of its containers, held ones
// and long gone ones aside, and those its own call started that none of
// them is (entry.starting). ok is false when nothing is down or none of o's
// services runs.
func (b *Book) carry(o *entry, down map[string]bool, use map[string]uint64) (t takeover, ok bool) {
	if len(down) == 0 {
		return t, false
	}
	services, running := map[string]bool{}, map[string][]string{}
	for k := range o.bound {
		if o.held[k] {
			continue
		}
		v := b.verdicts[k]
		if v == nil && b.gone(k) {
			continue // long gone: its service is unknown, and runs in another
		}
		svc := k
		if v != nil {
			svc = cmp.Or(v.service, k)
		}
		services[svc] = true
		if v == nil || b.runs(k, v) {
			running[svc] = append(running[svc], k)
		}
	}
	for svc := range o.starting {
		services[svc] = true // its call started it, and it has not come
	}
	if len(running) == 0 {
		return t, false
	}
	share := o.cost / uint64(len(services))
	for _, ks := range running {
		var used uint64
		for _, k := range ks {
			used += use[k]
		}
		t.carry += share - min(used, share)
		t.used += used
	}
	t.carry = min(t.carry, o.cost-min(t.used, o.cost))
	return t, true
}

// startTakes reports whether a start of starts takes o over: the lease of
// the run or create that made one of them (its label), which has not run.
func startTakes(r policy.Request, starts []policy.Start, o *entry) bool {
	return slices.ContainsFunc(starts, func(t policy.Start) bool { return t.TakesOver != "" && t.TakesOver == o.ID }) && takesCreate(r, o)
}

// takesCreate reports whether a start may take over o, a run's or
// create's lease its target names: one that bound nothing yet, and a
// worktree's only for a worktree's call (a manual call reserves nothing).
// Another worktree's too: its label is only on the container its own
// create made (protocol.LeaseOf, and the shim refuses one a call sets),
// and this start holds the larger cost for it.
func takesCreate(r policy.Request, o *entry) bool {
	return o.labelled && len(o.bound) == 0 && (r.Worktree != "" || o.Worktree == "")
}

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
	// docker start db <db's ID>: one container.
	starts = slices.CompactFunc(slices.SortedStableFunc(slices.Values(starts), func(x, y policy.Start) int { return strings.Compare(x.ID, y.ID) }),
		func(x, y policy.Start) bool { return x.ID == y.ID })
	// Named but not resolved: each still starts. The first, unresolved,
	// is a start of several only when it names others.
	unresolved := r.Unresolved
	if r.ContainerID == "" && r.Target != "" && (len(r.Others) > 0 || r.Unresolved > 0) {
		unresolved++
	}
	// Docker said what each target is: one it resolved, or one that does
	// not exist (it starts nothing).
	known := (r.ContainerID != "" || r.FirstMissing) && unresolved == 0 && !r.MultiTarget
	if known && !slices.ContainsFunc(starts, func(t policy.Start) bool { return !t.Running }) {
		// A start or restart of containers Docker says run, or that do not
		// exist: it starts nothing new, and they already count. Allowed,
		// whatever the pressure, and no lease.
		why := "it already runs"
		if len(starts) == 0 {
			why = "no such container"
		}
		return protocol.Decision{Allow: true, Message: fmt.Sprintf("headroom: allowed `%s` (%s)", Summary(r.Command), why)}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.latest
	if s == nil || (current != nil && current.CollectedAt.After(b.observed.Add(stale))) {
		s = current
	}
	now := b.now()
	b.expire(now) // settling may have stalled: expired leases must not count
	// The containers this start's lease is keyed by and costs: not those
	// that run, nor those a worktree's open lease has bound (not as
	// entry.held) or lists in its containerIDs (covered; docker stop &&
	// docker start): that lease still covers them, and its worktree is
	// charged for them. A manual call's lease is charged nothing, so it
	// covers nothing.
	// The leases that cover them wait for them again, but only once this
	// start is allowed (renew): a denied one, or a BUDGET_WAIT polling it,
	// must not keep them open.
	var covering []*entry
	starts = slices.DeleteFunc(slices.Clone(starts), func(t policy.Start) bool {
		if t.Running {
			return true
		}
		cs := b.covered(t.ID)
		covering = append(covering, cs...)
		return len(cs) > 0
	})
	if known && len(starts) == 0 {
		// Allowed, and no new lease. Not when others went unresolved:
		// they are no lease's.
		b.renew(covering, now)
		return protocol.Decision{Allow: true, Message: fmt.Sprintf("headroom: allowed `%s` (its lease holds it)", Summary(r.Command))}
	}
	// The project's running services in the caller's worktree: a compose
	// up's lease holds them (below). Not a guess's (entry.guessed).
	var stack []resource
	if r.Kind == "compose" && r.Op != "run" && r.Target != "" && !r.Guessed && r.Worktree != "" && r.OnEngine && s != nil {
		for _, x := range resources(s) {
			// Not one Docker's events said exited since (compose stop).
			if x.kind == "compose" && !x.oneoff && x.project == r.Target && x.worktree == r.Worktree && !b.seen[x.key].gone {
				stack = append(stack, x)
			}
		}
	}
	// Compose's own dry run said the call creates, recreates and starts
	// nothing (compose up -d of a stack that runs as configured).
	idle := r.Kind == "compose" && r.Op != "run" && r.Idle
	// What this call's lease takes over is part of what it will hold (see
	// the takeovers below): the check weighs that in its cost, not twice.
	// An idle up adds nothing: what it takes over stays counted as it is.
	est := cmp.Or(r.CostBytes, c.DefaultContainerBytes) // one container's, or the stack's
	var down map[string]bool
	use := map[string]uint64{}
	if r.Kind == "compose" && r.Op != "run" && !idle {
		down = b.downServices(r)
		if s != nil {
			for _, x := range resources(s) {
				use[x.key] = x.bytes
			}
		}
	}
	var reservedTaken, carried, surplus uint64
	composeTook := false
	carries := map[*entry]takeover{}
	for _, e := range b.open {
		switch {
		case !idle && composeTakes(r, e):
			if t, ok := b.carry(e, down, use); ok {
				carries[e], carried = t, carried+t.carry
			} else {
				reservedTaken += e.reserved()
			}
			composeTook = true
			continue
		case startTakes(r, starts, e):
			surplus += min(max(est, e.cost)-est, math.MaxUint64-surplus) // that container's cost: the larger
			continue
		}
		r.LeasedBytes += e.reserved()
		if e.Worktree == r.Worktree {
			r.WorktreeLeasedBytes += e.reserved()
		}
		if e.macOS && len(e.bound) == 0 {
			r.PendingMacOS++ // once bound, its VM counts as running
		}
	}
	for _, e := range b.lapsed {
		if startTakes(r, starts, e) {
			surplus += min(max(est, e.cost)-est, math.MaxUint64-surplus) // released when it lapsed: counted afresh
		}
	}
	n := len(starts) + unresolved
	switch {
	case idle:
		// Decided at a byte (0 means the default): the reading may be
		// seconds old, so the pressure guard still holds.
		r.CostBytes = 1
	case composeTook:
		r.CostBytes = max(est, reservedTaken) + carried
	case n > 1 || surplus > 0:
		// docker start a b c: each costs what one would, or what the run
		// or create that made it reserved, if more.
		r.CostBytes = math.MaxUint64
		if est <= (math.MaxUint64-surplus)/uint64(max(n, 1)) {
			r.CostBytes = uint64(max(n, 1))*est + surplus
		}
	}
	d := policy.Decide(r, s, c)
	d.LeasedBytes = r.LeasedBytes
	if !d.Allow {
		return d
	}
	b.renew(covering, now)
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
		cost: d.CostBytes, bound: map[string]bool{}, held: map[string]bool{}, dead: map[string]deaths{}, macOS: r.MacOS, pid: r.PID,
		labelled: r.Labelled, name: r.Name, target: r.Target, guessed: r.Kind == "compose" && r.Guessed,
	}
	if idle {
		e.cost, e.idle = 0, true // it starts nothing new
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
		// lease takes that one over, with its containers and its cost:
		// what they use stays counted, so a start of one stopped is
		// covered. Those it held stay held, whoever the reading says runs
		// them: one gone since (Book.gone) is no longer held, so Compose
		// starting it binds it afresh, and a plain start of it is checked.
		var reserved, used uint64
		b.open = slices.DeleteFunc(b.open, func(o *entry) bool {
			if !composeTakes(r, o) {
				return false
			}
			e.took = append(e.took, o)
			if t, ok := carries[o]; ok {
				used += t.used // weighed in the check's cost
			} else {
				// Not the last use of one that died since: this call
				// starts it again.
				reserved, used = reserved+o.reserved(), used+o.used-min(b.deadUse(o, use), o.used)
			}
			for k := range o.bound {
				e.bound[k], e.held[k] = true, o.held[k]
			}
			e.found = e.found || o.found
			b.log.Debug("lease ended: a later compose call took its project over", "lease", o.ID, "by", e.ID)
			return true
		})
		if len(down) > 0 {
			e.starting = down
		}
		if len(e.took) > 0 {
			// The same stack again, with a fresh timeout: this call was
			// admitted, and its containers may be a pull away. With none
			// of its services down, the call recreates the stack or
			// starts what the old leases still wait for: it holds the
			// larger of its estimate and what they still reserved,
			// bounded however often it repeats. With some down (#112),
			// it starts those, by its estimate, and holds beside it what
			// each old lease still reserves for its services that run:
			// each one's equal share of that lease's cost (one number
			// says no more), less what its containers use (Book.carry).
			// A service it starts again is counted once, by its
			// estimate; one it also recreates while warming is counted
			// twice until the lease ends, at most what the old lease
			// reserved for it. Either way, as decided, and on top of
			// what their containers use.
			e.cost, e.used = max(e.cost, reserved)+used, used
			if idle {
				// It adds nothing: what it took over ends when that would
				// have, however often the up repeats. And what that waited
				// for came: Compose saw it running.
				for _, o := range e.took {
					if o.Expires.Before(e.Expires) {
						e.Expires = o.Expires
					}
				}
			}
		}
		// compose up -d of a stack that already runs, once its lease
		// ended: its running containers are this lease's, so it never logs
		// "never appeared" when the up starts nothing new. They are held
		// (entry.held). Only those held by no lease.
		for _, x := range stack {
			if !e.bound[x.key] && !b.boundAnywhere(x.key) {
				e.bind(x)
				e.held[x.key], e.found = true, true
				b.boundBy(x, e)
			}
		}
	}
	if r.ContainerID != "" {
		// Resolved, a is keyed by its ID, or not at all when it runs or an
		// open lease holds it (docker start a b: the lease is for b).
		e.target = ""
	}
	for _, t := range starts {
		if t.TakesOver == "" {
			continue
		}
		// A start of a container a run or create made that has not run
		// yet: that lease ends here, and this one holds the larger cost
		// for that container (decided above).
		for _, list := range []*[]*entry{&b.open, &b.lapsed} {
			*list = slices.DeleteFunc(*list, func(o *entry) bool {
				if o.ID != t.TakesOver || !takesCreate(r, o) {
					return false
				}
				if list == &b.open {
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
	key   string // "container:<id>" or "vm:<name>"
	name  string
	id    string // a container's ID
	lease string // the lease ID its LeaseLabel carries
	// mark is the LeaseLabel it carries, counted or not (protocol.LeaseOf),
	// once Book.unmark keeps it only for a live lease's: the shim made it,
	// or it is forged, and it is no compose lease's service. One of a lease
	// that is over is inherited from an image committed from a gated
	// container.
	mark     string
	dir      string // a compose container's project directory
	kind     string // container, compose (a compose project's container) or vm
	project  string // the compose project, for compose
	service  string // its compose service
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
			r := containerResource(c.ID, c.Name, c.Labels)
			r.worktree, r.bytes = owner[r.key], c.MemoryBytes
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

// containerResource is the container id: its labels say which keys apply.
func containerResource(id, name string, labels map[string]string) resource {
	r := resource{key: "container:" + id, name: name, id: id, kind: "container", lease: protocol.LeaseOf(labels),
		mark: labels[protocol.LeaseLabel]}
	if p := labels[protocol.ComposeProjectLabel]; p != "" {
		r.kind, r.project, r.dir = "compose", p, labels[protocol.ComposeWorkingDirLabel]
		r.oneoff, r.service = labels[protocol.ComposeOneoffLabel] == "True", labels[protocol.ComposeServiceLabel]
	}
	return r
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
	// The reading is the word on what runs, for events before it began.
	began, current := readingBegan(s)
	for k, v := range b.seen {
		if current && v.at.Before(began) {
			delete(b.seen, k)
		}
	}
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
	// A start of several's container that died, back in a reading begun
	// after: a lease checked while it was dead that it binds takes it, else
	// it is the start's again (Book.markDead). A start event dated its
	// restart even when no reading missed it, or when a reading begun
	// before the event showed it: prev holds it then.
	for _, r := range res {
		from, d := b.deadIn(r.key)
		if from == nil || b.seen[r.key].at.After(began) || !readable(s, kindOf(r.key)) {
			continue
		}
		delete(from.dead, r.key)
		evented := !d.last().ran.IsZero()
		if !evented {
			d.last().ran = now // no event saw it: by this reading
		}
		r = b.unmark(r)
		if e := b.binder(r); (evented || !b.prev[r.key]) && e != nil && d.takenBy(e) {
			from.letGo(r.key)
			e.bind(r)
			b.judge(r, gated, now)
			b.boundBy(r, e)
		}
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
			if r, ok := byID[id]; ok && !bound[r.key] && keyed(b.open, b.unmark(r), true) == e {
				e.bind(r)
				b.judge(r, gated, now)
				b.boundBy(r, e)
				bound[r.key] = true
			}
		}
	}
	var fresh []resource // new this tick, or back after a reading it was missing from (not gone: a die), and bound to no lease yet
	for _, r := range res {
		if sn := b.seen[r.key]; sn.gone && sn.at.After(began) && sn.bound {
			// This life's start event bound it, and it died since the
			// reading began: gone, not new to a lease checked since. One
			// no event bound, whatever verdict an earlier life left, is
			// the reading's to bind or judge (#131).
			continue
		}
		if (!b.prev[r.key] || b.missed[r.key] > 0 && !b.gone(r.key)) && !bound[r.key] {
			fresh = append(fresh, b.unmark(r))
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
		if e := b.binder(r); e != nil {
			e.bind(r)
			if b.prev[r.key] {
				// Back after one reading it was missing from: with no
				// events, a stop the call started again, or a stats blip
				// across its check (#87). Held, so it binds without its
				// use counting as what the lease waits for, and it keeps
				// its verdict. Without events a stop reads like that blip,
				// so an up after a stop reserves its cost on top of what
				// the container uses, until the lease ends (its timeout at
				// the latest): an error toward a deny. With events, the
				// stop's die makes it gone, and its start binds it as new
				// (#131).
				e.held[r.key], e.found = true, true
			} else {
				b.judge(r, gated, now)
			}
			b.boundBy(r, e)
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
		v.owner = cmp.Or(r.worktree, v.owner)
		v.crashed = v.crashed && b.seen[r.key].at.After(began) // see verdict.crashed
	}
	for k, v := range b.verdicts {
		keep, since := b.timeout, v.last
		if v.crashed {
			// A crash loop's lives may be too short for any reading to list:
			// it is absent only once its events stop too.
			keep = max(keep, crashKept)
			if v.event.After(since) {
				since = v.event
			}
		}
		if !v.present && now.Sub(since) >= keep {
			delete(b.verdicts, k)
		}
	}
	b.lapsed = slices.DeleteFunc(b.lapsed, func(e *entry) bool { return !now.Before(e.Expires.Add(b.timeout)) })
	// What was there, in the last reading or bound by an open lease, and is
	// missing from this one: gone only after goneAfter readings (Book.gone).
	missed, since := map[string]int{}, map[string]time.Time{}
	count := func(k string) {
		n := b.missed[k]
		switch _, ok := present[k]; {
		case b.seen[k].at.After(began):
			// It started or died during the reading, which may not show
			// it: the event stands.
		case ok:
			n = 0
		case readable(s, kindOf(k)):
			n = min(n+1, goneAfter(k)) // a failed read keeps the count
		}
		missed[k] = n
		switch at, ok := b.missedSince[k]; {
		case n == 0:
		case b.missed[k] > 0 && ok:
			since[k] = at
		case !began.IsZero():
			since[k] = began
		default:
			since[k] = now
		}
	}
	for k := range b.prev {
		count(k)
	}
	for _, e := range b.open {
		for k := range e.bound {
			count(k)
		}
	}
	b.missed, b.missedSince = missed, since
	next := map[string]bool{}
	for _, r := range res {
		next[r.key] = true
	}
	for k := range b.prev {
		if !readable(s, kindOf(k)) || !b.gone(k) {
			next[k] = true // a failed read, or only missing: keep what was there
		}
	}
	b.prev = next
	b.markBased(s)
	cp := *s // a copy: the daemon keeps writing to s after this
	b.latest, b.observed = &cp, now

	for _, e := range b.open {
		for k := range e.held {
			if b.gone(k) {
				b.release(k)
			}
		}
	}
	for _, e := range b.open {
		var used uint64
		unsure := false
		for k := range e.bound {
			if e.held[k] {
				continue // it ran before the check: not what the lease waits for
			}
			if bytes, ok := present[k]; ok {
				used += bytes
			} else if !readable(s, e.waitsFor()) || !b.gone(k) {
				unsure = true
			}
		}
		if !unsure {
			e.used = used // a failed read, or one it is only missing from, keeps the last known use
		}
	}
	b.open = slices.DeleteFunc(b.open, func(e *entry) bool {
		alive := false
		for k := range e.bound {
			alive = alive || !b.gone(k)
		}
		switch {
		case e.Kind == "tart" && len(e.bound) == 0 && e.pid > 0 && !b.alive(e.pid):
			b.log.Info("lease ended: its tart run exited before its VM appeared", "lease", e.ID, "worktree", e.Worktree,
				"command", e.Command)
			return true
		case len(e.bound) > 0 && e.used >= e.cost:
			return true // its resources use the cost
		case !alive && e.over():
			return true
		}
		return false
	})
	for _, e := range b.open {
		for k := range e.bound {
			if b.gone(k) {
				b.markDead(k, cmp.Or(b.missedSince[k], now))
			}
		}
	}
	b.expire(now)
}

// binder is the open lease r, new in a reading, binds, or nil.
func (b *Book) binder(r resource) *entry {
	es := b.open
	if v := b.verdicts[r.key]; v != nil && v.crashed {
		// Back after a crash and a reading: as for its start event.
		es = slices.DeleteFunc(slices.Clone(es), func(e *entry) bool { return !b.bindsAfterCrash(r, e) })
	}
	e := keyed(es, r, b.based[source(r.kind)])
	if e != nil && e.oneoff && !r.oneoff && b.verdicts[r.key] != nil {
		// A service back after a tick away (a crash loop) is no new
		// dependency of a compose run: it keeps its verdict.
		return nil
	}
	return e
}

// over reports whether e ends once its containers or VM are gone: not a
// compose project's, whose first container may be a one-shot with the
// services after it, nor a start's before each of its containers came.
func (e *entry) over() bool {
	return len(e.bound) > 0 && (e.Kind != "compose" || e.oneoff && e.hasOneoff) && len(e.bound) >= e.starts()
}

// starts is how many containers a start's lease waits for: 0 for any
// other lease.
func (e *entry) starts() int {
	n := len(e.containerIDs)
	if n > 0 && e.target != "" {
		n++ // and the first, by its name
	}
	return n
}

// boundIDs counts the containers e bound by their ID.
func (e *entry) boundIDs() int {
	n := 0
	for _, id := range e.containerIDs {
		if e.bound["container:"+id] {
			n++
		}
	}
	return n
}

// seen is what Docker's events last said of a container, and when.
type seen struct {
	at   time.Time
	gone bool
	// stopped: by docker stop or kill, or compose stop or kill (a stop or
	// kill event), not a restart policy's restart or a crash.
	stopped bool
	// bound: its start event bound it to a lease, in this life (a start
	// resets it; a stop, kill or its die keeps it).
	bound bool
	// died: a die since its last start. A second die means a start the
	// events lost, which began a life no event bound: that die drops
	// bound (#131).
	died bool
}

// readingBegan is when s's Docker reading began: an event after it may be
// missing from it. fresh is false when the reading is the last good one,
// kept after a failed attempt: it says nothing new of any event.
func readingBegan(s *protocol.Snapshot) (began time.Time, fresh bool) {
	st, ok := s.Sources["docker"]
	switch {
	case !ok:
		return s.CollectedAt, true // no status: a reading stamped as a whole
	case st.Stale || st.Began.IsZero():
		return time.Time{}, false
	}
	return st.Began, true
}

// ContainerEvent takes Docker's word that a container started or exited
// (#67): one that lives between two readings still binds the lease its
// call took, and its exit ends that lease at once. An event only binds:
// whether an unbound container was gated is the next reading's to judge.
func (b *Book) ContainerEvent(action, id, name string, labels map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := containerResource(id, name, labels)
	now := b.now()
	switch action {
	case "stop":
		last := b.seen[r.key]
		b.seen[r.key] = seen{at: now, gone: true, stopped: true, bound: last.bound, died: last.died}
	case "kill":
		if sig := labels["signal"]; sig == "9" || sig == "15" {
			// SIGKILL or SIGTERM: it stops (its die says it is gone). Any
			// other signal (HUP: a reload) leaves it running.
			v := b.seen[r.key]
			v.at, v.stopped = now, true
			b.seen[r.key] = v
		}
	case "start":
		if v := b.verdicts[r.key]; v != nil {
			v.event = now
		}
		last := b.seen[r.key]
		stopped := last.stopped
		b.seen[r.key] = seen{at: now}
		delete(b.missed, r.key) // it runs again
		from, d := b.deadIn(r.key)
		if from == nil && b.boundAnywhere(r.key) {
			return
		}
		if from != nil && d.last().ran.IsZero() {
			// It runs again: the mark keeps when, for the reading to
			// settle whose it is if this event does not (Book.markDead).
			d.last().ran = now
		}
		r = b.unmark(r)
		e := keyed(b.open, r, b.based[source(r.kind)])
		// After a crash: Docker said so, or the verdict kept it across the
		// readings since.
		crashed := last.gone && !stopped || b.verdicts[r.key] != nil && b.verdicts[r.key].crashed
		checkedSinceCrash := e != nil && last.gone && e.Created.After(last.at)
		switch {
		case e == nil:
			return
		case from != nil && !d.takenBy(e):
			// Not checked since it died: its start's restart, a restart
			// policy's (Book.markDead).
			return
		case crashed && !b.bindsAfterCrash(r, e):
			// It was gated before the crash, so it is not flagged.
			return
		case b.prev[r.key] && !stopped && !checkedSinceCrash && !e.labelled && !slices.Contains(e.containerIDs, id):
			// A container the last reading held, started again without
			// a stop (a restart policy's restart): no name's or project's.
			// After docker or compose stop it is a start like any, and so
			// after a crash it is for a call checked since (compose up).
			return
		case e.oneoff && !r.oneoff && b.verdicts[r.key] != nil:
			return // a crash-looping service is no new dependency of a compose run
		case tied(b.open, r, e):
			// Two worktrees' leases match it equally, and an event says
			// nothing of whose it is: the reading's attribution decides.
			return
		}
		if from != nil {
			from.letGo(r.key)
		}
		e.bind(r)
		b.judge(r, gated, now)
		b.boundBy(r, e)
		b.seen[r.key] = seen{at: now, bound: true}
	case "die":
		last := b.seen[r.key]
		b.seen[r.key] = seen{at: now, gone: true, stopped: last.stopped, bound: last.bound && !last.died, died: true}
		if v := b.verdicts[r.key]; v != nil {
			v.crashed = !b.seen[r.key].stopped
			v.event = now
		}
		b.missed[r.key] = goneAfter(r.key) // gone at once
		b.release(r.key)
		b.open = slices.DeleteFunc(b.open, func(e *entry) bool {
			if !e.bound[r.key] || !e.over() {
				return false
			}
			for k := range e.bound {
				if !b.gone(k) {
					return false // it runs, or is only missing
				}
			}
			b.log.Debug("lease ended: its container exited", "lease", e.ID, "worktree", e.Worktree)
			return true
		})
		b.markDead(r.key, now)
	}
}

// expire drops leases past their timeout: quietly if they bound their
// resource, logged if it never appeared (R10).
func (b *Book) expire(now time.Time) {
	b.open = slices.DeleteFunc(b.open, func(e *entry) bool {
		switch {
		case now.Before(e.Expires):
			return false
		case len(e.bound) > 0 || e.found:
			b.log.Debug("lease ended at its timeout", "lease", e.ID, "worktree", e.Worktree, "command", e.Command)
			return true
		case e.idle:
			b.log.Debug("lease of an up that started nothing ended at its timeout", "lease", e.ID, "worktree", e.Worktree)
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
		case e.guessed:
			// The guess missed, its stack was another lease's, or it still
			// pulls (entry.guessed): it held its cost to the timeout, and
			// lapses quietly.
			b.log.Debug("lease with a guessed key ended at its timeout", "lease", e.ID, "worktree", e.Worktree, "command", e.Command)
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

// covered returns each worktree's open lease that holds container id, or
// waits for it: two docker start db at once, the first's lease covers it.
// Not one that holds it as entry.held: it reserves nothing for that one.
// It renews none of them: Check does, once the start is allowed (renew).
func (b *Book) covered(id string) []*entry {
	k := "container:" + id
	var out []*entry
	for _, e := range b.open {
		if e.Worktree != "" && (e.bound[k] && !e.held[k] || slices.Contains(e.containerIDs, id)) {
			out = append(out, e)
		}
	}
	return out
}

// renew starts the timeout of each lease that covers an allowed start
// afresh: it waits for that container again.
func (b *Book) renew(covering []*entry, now time.Time) {
	for _, e := range covering {
		e.Expires = later(e.Expires, now.Add(b.timeout))
	}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (b *Book) boundAnywhere(key string) bool {
	return slices.ContainsFunc(b.open, func(e *entry) bool { return e.bound[key] })
}

// gone reports whether k, a container or VM in the last reading or bound by
// an open lease, is gone: Docker said it died, or it was missing from
// goneAfter fresh readings in a row (#87). A container missing from one is
// missing, not gone: its stats failed, or a restart backoff spans the
// reading. Only one gone lets go of its held state, ends its lease, and
// leaves prev, so that when it is back it is new. One only missing, back,
// binds a lease held (Observe): it may never have stopped.
func (b *Book) gone(k string) bool { return b.missed[k] >= goneAfter(k) }

// goneAfter is how many fresh readings in a row k must be missing from to
// be gone: two for a container, one for a VM, as before (#87's causes,
// failed stats and a restart backoff, are Docker's).
func goneAfter(k string) int {
	if kindOf(k) == "vm" {
		return 1
	}
	return 2
}

// bindsAfterCrash reports whether e may bind r, back after a crash (a
// restart policy's restart): a lease keyed by r's label or ID, or a
// project's only in r's owner's worktree. Another worktree may use the
// name (#89), so a container of no known worktree binds none.
func (b *Book) bindsAfterCrash(r resource, e *entry) bool {
	o := b.owner(r)
	return e.labelled || slices.Contains(e.containerIDs, r.id) || e.Kind != "compose" || o != "" && o == e.Worktree
}

// owner is the worktree r is in: as this reading attributes it, else as
// the last reading that attributed it did, else that of the lease it last
// bound; "" if none says.
func (b *Book) owner(r resource) string {
	if v := b.verdicts[r.key]; v != nil {
		return cmp.Or(r.worktree, v.owner, v.lease)
	}
	return r.worktree
}

// boundBy records that r bound e, if e's key says whose r is: its label or
// its project. A start by ID names the container, not whose it is (#89).
func (b *Book) boundBy(r resource, e *entry) {
	if v := b.verdicts[r.key]; v != nil && (e.labelled || e.Kind == "compose" && !slices.Contains(e.containerIDs, r.id)) {
		v.lease = e.Worktree
	}
}

// release lets go of a held container that is gone: it was never this
// lease's to start, so a start of it is a new call's.
func (b *Book) release(key string) {
	for _, e := range b.open {
		if e.held[key] {
			delete(e.bound, key)
			delete(e.held, key)
		}
	}
}

// death is when a start of several's container died and, once it runs
// again, when it ran (Book.markDead).
type death struct{ at, ran time.Time }

// deaths are a container's deaths since a reading last showed it, oldest
// first: it may die and run again more than once between two readings.
type deaths []*death

// last is the latest death.
func (ds deaths) last() *death { return ds[len(ds)-1] }

// takenBy reports whether e, the lease that binds the container as it runs
// again, takes it from its start: only a lease checked since it died and by
// when it ran again, in any of its deaths, can have started it.
func (ds deaths) takenBy(e *entry) bool {
	return slices.ContainsFunc(ds, func(d *death) bool { return !e.Created.Before(d.at) && !e.Created.After(d.ran) })
}

// markDead marks key, a container that is gone, dead in each open start of
// several still waiting on others (#111). It stays bound: the lease still
// covers its own start of it (docker stop && docker start), and a restart
// policy's restart is still its. When it runs again, its start event or the
// reading that shows it keeps when; dead again after that, before a
// reading, is another death. The first reading begun after it runs ends
// the mark: the lease that reading binds it to takes it if
// deaths.takenBy, else it stays the start's (entry.letGo): the start
// covers starting each once. A start event with a lease deaths.takenBy
// takes it at once. Leases it was all of ended first: they end quietly, as before.
func (b *Book) markDead(key string, now time.Time) {
	for _, e := range b.open {
		if e.starts() < 2 || !e.bound[key] || !slices.ContainsFunc(e.containerIDs, func(id string) bool { return "container:"+id == key }) {
			continue
		}
		if ds := e.dead[key]; len(ds) == 0 || !ds.last().ran.IsZero() {
			e.dead[key] = append(ds, &death{at: now}) // dead again after it ran: another
		}
	}
}

// deadIn returns the open lease key is dead in (Book.markDead), and its
// deaths.
func (b *Book) deadIn(key string) (*entry, deaths) {
	for _, e := range b.open {
		if ds, ok := e.dead[key]; ok {
			return e, ds
		}
	}
	return nil, nil
}

// letGo lets go of key, a container of e's that died, for a lease checked
// since to bind.
func (e *entry) letGo(key string) {
	delete(e.bound, key)
	delete(e.dead, key)
	e.containerIDs = slices.DeleteFunc(e.containerIDs, func(id string) bool { return "container:"+id == key })
	e.found = true
}

// live reports whether id is an open or lapsed lease's.
func (b *Book) live(id string) bool {
	is := func(e *entry) bool { return e.ID == id }
	return id != "" && (slices.ContainsFunc(b.open, is) || slices.ContainsFunc(b.lapsed, is))
}

// unmark drops the mark of a resource whose label names no live lease
// (resource.mark).
func (b *Book) unmark(r resource) resource {
	if !b.live(r.mark) {
		r.mark = ""
	}
	return r
}

// rank is how sure e's key for r is: a label, then a start's container ID,
// then a compose project's name (or a start's first target, by its name,
// not resolved), then a name, or a compose run's lease for a service
// (after any up's).
func rank(e *entry, r resource) int {
	switch {
	case e.labelled:
		return 0
	case slices.Contains(e.containerIDs, r.id):
		return 1 // docker start of a stopped service: its own lease
	case len(e.containerIDs) > 0, e.Kind == "compose" && (!e.oneoff || r.oneoff):
		return 2
	}
	return 3
}

// tied reports whether another worktree's lease in es keys r as surely as
// e does: only r's attribution can tell them apart.
func tied(es []*entry, r resource, e *entry) bool {
	return slices.ContainsFunc(es, func(o *entry) bool {
		// A manual call's lease reserves nothing: the worktree's wins, as
		// in keyed. A guess keys only its worktree's (entry.guessed), and
		// an event does not say whose r is: it may be the guess's.
		a := r
		if a.worktree == "" {
			a.worktree = o.Worktree
		}
		return o != e && o.Worktree != "" && o.Worktree != e.Worktree && o.key(a, true) && rank(o, r) == rank(e, r)
	})
}

// keyed is the lease in es whose key r matches. The surest key wins: a
// container's label names its lease outright, before a start's container ID
// or a compose project's name, before a name (docker start db || docker
// run --name db: the run's label beats the start's name). Without a
// baseline (based false), only a label or a container ID counts.
func keyed(es []*entry, r resource, based bool) *entry {
	// Among equal keys, the lease of the worktree r is attributed to: two
	// worktrees may bring up one project.
	// Then a key the shim named before a guess (entry.guessed).
	better := func(e, best *entry) bool {
		if rank(e, r) != rank(best, r) {
			return rank(e, r) < rank(best, r)
		}
		if r.worktree != "" {
			if (e.Worktree == r.worktree) != (best.Worktree == r.worktree) {
				return e.Worktree == r.worktree
			}
			return !e.guessed && best.guessed
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
		case !based || e.project == "" || e.project != r.project || r.mark != "":
			// mark: a shim run dressed as Compose's (its config hash and
			// project) must not take a project's lease by its name.
			return false
		case e.guessed && (r.worktree == "" || r.worktree != e.Worktree):
			// Only its own worktree's, as the reading attributes it (an
			// event's is unknown): entry.guessed.
			return false
		case r.oneoff:
			return e.oneoff && !e.hasOneoff // a compose run's own, one each
		}
		// A service: an up's, or a compose run's dependency, which starts
		// before its one-off container.
		return !e.oneoff || !e.hasOneoff
	case e.Kind == "container" && len(e.containerIDs) > 0:
		if slices.Contains(e.containerIDs, r.id) {
			return !e.bound[r.key] // docker start a b c: each once
		}
		// The first, named but not resolved in time.
		return based && e.target != "" && (e.target == r.name || e.target == r.id) && len(e.bound) == e.boundIDs()
	case e.Kind != "container" || len(e.bound) > 0:
		return false
	case e.name != "":
		return based && e.name == r.name // a client's --name, sent without Labelled
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
	v := &verdict{how: how, project: r.project, dir: r.dir, service: r.service, oneoff: r.oneoff, last: now, present: true,
		u: protocol.Ungated{Key: r.key, Name: r.name, Kind: r.kind, Worktree: r.worktree, Since: now}}
	if old := b.verdicts[r.key]; old != nil {
		v.owner, v.lease = old.owner, old.lease // an event's r is unattributed
	}
	v.owner = cmp.Or(r.worktree, v.owner)
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
