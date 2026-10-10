package lease_test

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/lease"
	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// A model of Docker and the agents' calls through the gate, driven at
// random against the book. After each reading it checks what hurts users:
// a checked container flagged ungated, a lease of a call that started
// something warning "never appeared", warming containers of checked calls
// needing more than is reserved, a container started past the shim not
// flagged, and, after each check too, a lease held for no reason.

const target = gib // what each container grows to

const timeout = 2 * time.Minute // the book's (book)

// plenty is the headroom readings report unless memory runs short (ops 14
// and 15): with it, every check must be allowed.
const plenty = int64(1024 * gib)

// lowReadings is how many readings memory stays short after op 14 lowers
// headroom: one, so most calls are checked with plenty.
const lowReadings = 1

// waitReadings is how many readings memory stays short while a BUDGET_WAIT
// call (op 15) polls: it is denied, and asked again after each.
const waitReadings = 3

type mcont struct {
	id, project, service, wt string
	labels                   map[string]string
	cur                      uint64
	running, gone            bool
	firstGated, raw          bool
	startedBy                string    // the lease of the call that last started it, "" if none
	startedAt                time.Time // when its call was checked
	crashed                  time.Time // when it last crashed, zero once it stopped or started
	heldBy                   string    // the up that found it running bound to no lease, or the lease a reading bound it to back after one it missed (backReading); "" once it stopped or started
	boundBy                  string    // the lease that started it, the up open at a guessed hit's start that took it, or the up that took either over
	missing                  bool      // left out of this reading
	missedAt                 int       // the last reading it was left out of
	read                     int       // the last reading it was in
	// deaths are its deaths since a reading showed it, while deadIn, a
	// start of several's lease (model.several), binds it, each with when it
	// ran again: that lease keeps it until a lease checked in one of them
	// binds it (lease.go markDead, deaths.takenBy).
	deaths []mdeath
	deadIn string
	// owned is set once a reading with attribution showed it, or a
	// project's lease bound it: the book knows its worktree (lease.go owner,
	// boundBy). pending is set while a start of it bound no lease at its
	// event: crashed and owned by no worktree, a call's (lease.go
	// bindsAfterCrash), or a restart policy's (model.restart): the next
	// reading binds it as new, if it is new to it (pendingReading). verdict
	// is set while the book has a verdict of it (lease.go judge): a reading
	// showed it or an event bound it, at verdictAt at the latest, and it was
	// not absent for the timeout since; owned goes with it.
	owned, pending, verdict bool
	verdictAt               time.Time
	// guess is the guessed up that started it as its own stack's (a hit),
	// until its first reading, which decides whether the guess binds it, or
	// another call's start, whose lease it is then (lease.go keyed).
	// taken is set when an open up of its project in its worktree took it at
	// its start, before the reading (lease.go keyed). shown is set when the
	// last reading before that start showed it, or only missed it: it is not
	// new to the next, which binds it to no lease (lease.go Observe fresh).
	guess string
	taken bool
	shown bool
	began int // the reading count when it last started
	// miss is set when a guessed up that missed created it: no lease's key
	// names its project (m1, m2). judged is then its verdict as the book's
	// rules make it, kept until it is absent for the timeout (judgedAt, when
	// a reading last showed it), and unbound is set while a start no lease
	// binds waits for the reading that judges it: a gated sibling in that
	// reading vouches for it (lease.go gatedProject). lapsedFor never does:
	// no lease that keys m1 or m2 lapses (a docker start's would warn
	// never appeared).
	miss     bool
	judged   int
	judgedAt time.Time
	unbound  bool
	// byGuess is set once a guessed up started it: the differential compares
	// its binding with the book's (model.differ).
	byGuess bool
	// vcrashed is set while the book's verdict of it says it crashed: it
	// died with no stop, and no reading has shown it since (lease.go
	// verdict.crashed). A reading binds it then only by the crash filter
	// (binder).
	vcrashed bool
}

// mdeath is a death and when it ran again (lease.go death).
type mdeath struct{ at, ran time.Time }

// A miss's container's verdict (mcont.judged).
const (
	unjudged = iota
	judgedGated
	judgedUngated
	judgedBaseline // in a reading, with no verdict to keep (lease.go Observe)
)

// mguess is a guessed up's lease (lease.go entry.guessed): what its
// containers come up as. Whether it bound one is model.confirmed.
type mguess struct {
	name, project, wt string // what it names, and what its stack comes up as
}

type op struct{ kind, a, b int }

type model struct {
	t        *testing.T
	cur      op
	b        *lease.Book
	c        *clock
	log      interface{ String() string }
	conts    []*mcont
	n        int
	flick    bool              // attribution lost this tick
	leases   map[string]string // lease → the call that took it
	starts   map[string]int    // lease → containers its call started
	guesses  map[string]*mguess
	outcomes map[string]int // how often each guessed outcome ran (coverage)
	trace    []string
	last     time.Time // the last allowed call
	ticks    int
	headroom int64          // what the readings say is free
	low      int            // readings left before headroom is plenty again
	open     map[int]bool   // the open bugs whose cases run
	several  map[string]int // a docker start's lease → the containers it waits for (lease.go entry.starts)
	readAt   time.Time      // when the last reading was taken
	diff     bool           // HEADROOM_MODEL_DIFF=1: compare guessed bookkeeping with the book's after each op (differ)
}

// openBugs are the cases that expose a bug still open, by its issue. They
// run only when HEADROOM_MODEL_OPEN is 1 (all) or lists the issue
// (HEADROOM_MODEL_OPEN=87,89). Fixing the issue removes its entry.
var openBugs = map[int]string{}

// openIssues are the issues HEADROOM_MODEL_OPEN turns on.
func openIssues() map[int]bool {
	out := map[int]bool{}
	for _, f := range strings.Split(os.Getenv("HEADROOM_MODEL_OPEN"), ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		switch {
		case err != nil:
		case n == 1:
			for i := range openBugs {
				out[i] = true
			}
		default:
			out[n] = true
		}
	}
	return out
}

// on reports whether the cases of issue run: fixed, or turned on.
func (m *model) on(issue int) bool {
	_, open := openBugs[issue]
	return !open || m.open[issue]
}

func (m *model) step(f string, a ...any) { m.trace = append(m.trace, fmt.Sprintf(f, a...)) }

type failure struct{ kind, msg string }

func (m *model) fail(f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	kind, _, _ := strings.Cut(msg, ":")
	panic(failure{kind, fmt.Sprintf("%s\ntrace:\n  %s\nleases: %+v", msg, strings.Join(m.trace, "\n  "), m.b.List())})
}

func (m *model) snapshot() *protocol.Snapshot {
	s := snap()
	s.Docker.Containers = nil
	// Each worktree's directory, as Compose labels its stacks with, so the
	// book's matcher finds the attribution below again (lease.go Observe,
	// attribution.AttributeLeased); none while attribution is lost.
	for i := range s.Orca.Worktrees {
		if !m.flick {
			s.Orca.Worktrees[i].Path = "/src/" + s.Orca.Worktrees[i].ID
		}
	}
	for _, x := range m.conts {
		if !x.running || x.missing {
			continue
		}
		wt := x.wt
		if m.flick {
			wt = ""
		}
		addContainer(s, protocol.Container{ID: x.id, Name: x.id, MemoryBytes: x.cur, Labels: x.labels}, wt)
	}
	s.CollectedAt = m.c.t
	s.Budget.HeadroomBytes = i64(m.headroom)
	return read(s, m.c.t)
}

func (m *model) event(action string, x *mcont) { m.b.ContainerEvent(action, x.id, x.id, x.labels) }

func (m *model) start(x *mcont, by string) {
	if by != "covered" && !m.bound(x) {
		x.boundBy = by
	}
	crashed := m.crashedToBook(x) // before it starts: the book's word for its start event
	x.shown = m.inLast(x)
	x.running, x.cur, x.startedBy, x.startedAt, x.raw, x.crashed, x.heldBy, x.missedAt = true, 0, by, m.c.t, by == "", time.Time{}, "", 0
	x.began, x.guess, x.taken, x.unbound, x.pending = m.ticks, "", false, false, false
	if by != "" {
		m.starts[by]++
	}
	m.event("start", x)
	m.ranAgain(x, crashed)
}

// crashedToBook reports whether the book takes a start of x for one after
// a crash: it crashed, and no reading has dropped the crash's event since,
// or a verdict of it keeps it (lease.go ContainerEvent, verdict.crashed).
func (m *model) crashedToBook(x *mcont) bool {
	return !x.crashed.IsZero() && (x.crashed.After(m.readAt) || x.verdict)
}

// inLast reports whether the last reading showed x, or only missed it
// after the one before showed it: it is still there to the book (lease.go
// prev).
func (m *model) inLast(x *mcont) bool {
	return m.ticks > 0 && (x.read == m.ticks || x.missedAt == m.ticks && x.read == m.ticks-1)
}

func (m *model) newCont(project, service, wt string, labels map[string]string) *mcont {
	m.n++
	x := &mcont{id: fmt.Sprintf("c%d", m.n), project: project, service: service, wt: wt, labels: labels}
	m.conts = append(m.conts, x)
	return x
}

// of is a worktree's containers of a compose project.
func (m *model) of(project, wt string) []*mcont {
	var out []*mcont
	for _, x := range m.conts {
		if x.project == project && x.wt == wt && !x.gone {
			out = append(out, x)
		}
	}
	return out
}

// check asks the book about r. A denied call starts nothing; with plenty
// of headroom, a denial is a failure.
func (m *model) check(r policy.Request) protocol.Decision {
	d := m.b.Check(r, m.snapshot(), cfg)
	if !d.Allow && m.headroom == plenty {
		m.fail("%s denied: %+v", r.Command, d)
	}
	if d.Allow {
		m.last = m.c.t
		if d.LeaseID != "" {
			m.leases[d.LeaseID] = r.Command
		}
	} else {
		m.step("%s: %s denied", r.Worktree, r.Command)
	}
	m.heldForNothing()
	return d
}

// stack is a compose project in a worktree's directory.
type stack struct{ project, wt string }

// stacks are the projects w1 and w2 bring up: both use the name app
// (#109).
var stacks = []stack{{"p1", "w1"}, {"p2", "w2"}, {"app", "w1"}, {"app", "w2"}}

// nameTaken reports whether x's compose project label names a stack of
// another worktree (#89).
func nameTaken(x *mcont) bool {
	name := x.labels[protocol.ComposeProjectLabel]
	return name != "" && slices.ContainsFunc(stacks, func(st stack) bool { return st.project == name && st.wt != x.wt })
}

// stackAt is the stack op argument a names.
func (m *model) stackAt(a int) stack {
	if !m.on(109) {
		return stacks[a%2]
	}
	return stacks[a%len(stacks)]
}

// Guessed outcomes of a guessed up (#84, #120): kind 2 with b odd, (b/2)%3.
const (
	guessHit       = iota // the guess names the caller's own stack
	guessMiss             // it names nothing: the stack comes up as m1 or m2
	guessMissOther        // it names another worktree's stack: w1 guesses p2
)

// guessOf is what a guessed up of st names, and the project its stack
// comes up as.
func guessOf(st stack, outcome int) (name, project string) {
	miss := "m" + strings.TrimPrefix(st.wt, "w") // a project only misses use
	switch outcome {
	case guessHit:
		return st.project, st.project
	case guessMiss:
		return "nothing", miss
	}
	other := map[string]string{"w1": "p2", "w2": "p1"}[st.wt]
	return other, miss
}

// composeUp is an up of st, or with guess ≥ 0, a guessed up of that
// outcome in st's worktree.
// tied reports whether another worktree's open guess names project, so
// the start event of wt's container of it ties (lease.go tied) and only a
// reading binds it: #109's.
func (m *model) tied(project, wt string) bool {
	if m.on(109) || project == "" {
		return false
	}
	for id, g := range m.guesses {
		if g.name == project && g.wt != wt && m.isOpen(id) {
			return true
		}
	}
	return false
}

// tiedUnread reports whether x is #109's: its start event tied, and no
// reading has shown it since. Gone before one, it binds no lease.
func (m *model) tiedUnread(x *mcont) bool { return x.read <= x.began && m.tied(x.project, x.wt) }

func (m *model) composeUp(st stack, guess int) bool {
	project, wt := st.project, st.wt
	name := project
	if guess >= 0 {
		name, project = guessOf(st, guess)
	}
	have := map[string]*mcont{}
	for _, x := range m.of(project, wt) {
		have[x.service] = x
	}
	if guess < 0 && m.tied(project, wt) && slices.ContainsFunc(m.of(project, wt), func(x *mcont) bool { return !x.running && x.read == m.ticks }) {
		// #109's: stopped since the last reading, which showed it, its
		// start ties and the next reading finds nothing new to bind.
		m.step("%s: compose up %s skipped (#109)", wt, project)
		return true
	}
	var cost uint64 // what the services it starts will use
	for _, sv := range []string{"a", "b"} {
		if x := have[sv]; x == nil || !x.running {
			cost += target
		}
	}
	idle := cost == 0 && guess < 0 // Compose's dry run failed too: a guess is never idle
	cmd := upCommand(project)
	taken := map[string]bool{} // its stack's leases it takes over, those not past their timeout
	for _, l := range m.b.List() {
		if guess >= 0 || l.Worktree != wt || !l.Expires.After(m.c.t) {
			continue // a guess takes nothing over
		}
		// A guess only once it bound something (lease.go composeTakes).
		if g := m.guesses[l.ID]; m.leases[l.ID] == cmd || g != nil && m.confirmed(l.ID) && g.project == project && g.wt == wt {
			taken[l.ID] = true
		}
	}
	r := policy.Request{Worktree: wt, Kind: "compose", Op: "up", Command: cmd,
		CostBytes: cost, Target: project, OnEngine: true, Idle: idle, ComposeDir: "/src/" + wt}
	if guess >= 0 {
		r.Command, r.Target, r.Guessed = "docker compose up -d", name, true
	}
	// An up of the project in this worktree, open now, takes what a hit
	// starts at its event: a guess keys no event, whose worktree is unknown
	// (lease.go entry.key). Unless another worktree's guess names the
	// project: the event ties, and the reading decides (tied).
	var up string
	if guess == guessHit && !m.tied(project, wt) {
		up = m.upOf(project, wt)
	}
	d := m.check(r)
	if !d.Allow {
		return false
	}
	for id := range taken {
		if m.guesses[id] != nil {
			m.outcomes["takeover"]++ // of a guess that hit
		}
	}
	if guess >= 0 {
		m.guesses[d.LeaseID] = &mguess{name: name, project: project, wt: wt}
		m.outcomes[[]string{"hit", "miss", "miss other"}[guess]]++
		m.step("%s: guessed compose up %s, comes up as %s → %s", wt, name, project, d.LeaseID)
	} else {
		m.step("%s: compose up %s (idle %v) → %s", wt, project, idle, d.LeaseID)
	}
	for _, x := range m.conts {
		// Removed ones and another worktree's too: the up binds and holds all
		// it took (lease.go Check).
		if taken[x.boundBy] {
			x.boundBy = d.LeaseID
		}
		if taken[x.heldBy] {
			x.heldBy = d.LeaseID // it stays held
		}
	}
	for _, sv := range []string{"a", "b"} {
		x := have[sv]
		switch {
		case x == nil || !x.running:
		case guess >= 0:
			// A guess holds no running stack (lease.go Check).
		case !m.bound(x) && m.ticks > 0 && x.read == m.ticks && !m.flick:
			x.heldBy = d.LeaseID // the last reading found it running in wt, no lease's
		}
		if x == nil {
			x = m.newCont(project, sv, wt, map[string]string{protocol.ComposeProjectLabel: project,
				protocol.ComposeServiceLabel: sv, protocol.ComposeConfigHashLabel: "h", protocol.ComposeWorkingDirLabel: "/src/" + wt})
			// A miss's container is ungated, as with no key.
			x.firstGated = guess < 0 || guess == guessHit
			x.miss = !x.firstGated
		}
		if !x.running {
			bound := m.bound(x) // a start's lease keeps it (lease.go boundAnywhere)
			up, unowned := up, m.crashedToBook(x) && !x.owned
			if m.bindsNoneAfterCrash(x, up) || unowned {
				up = "" // its start event binds no lease (#168's restart row)
			}
			m.start(x, d.LeaseID)
			x.byGuess = x.byGuess || guess >= 0
			switch {
			case bound:
			case unowned && guess < 0:
				x.boundBy, x.pending = "", true
			case guess == guessHit:
				x.pending = unowned
				// The up's, or not the guess's until a reading says so.
				x.guess, x.taken, x.boundBy = d.LeaseID, up != "", up
			case guess == guessMiss || guess == guessMissOther:
				x.boundBy, x.unbound = "", true // as with no key
			}
			x.owned = x.owned || m.isOpen(x.boundBy) && x.boundBy == d.LeaseID || x.taken // bound by its project
			if m.bound(x) {
				x.verdict, x.verdictAt, x.vcrashed = true, m.c.t, false
			}
			m.step("  started %s", x.id)
		}
	}
	return true
}

func (m *model) stop(x *mcont, crash bool) {
	if !crash {
		m.event("stop", x)
	}
	m.event("die", x)
	x.vcrashed = crash && x.verdict // lease.go ContainerEvent die
	if crash {
		x.crashed = m.c.t
		if x.verdict {
			x.verdictAt = m.c.t // kept from its die (lease.go verdict.event, crashKept)
		}
	} else {
		x.crashed = time.Time{}
	}
	x.running, x.heldBy = false, ""
	if m.several[x.boundBy] >= 2 && m.isOpen(x.boundBy) && (len(x.deaths) == 0 || !x.deaths[len(x.deaths)-1].ran.IsZero()) {
		x.deaths, x.deadIn = append(x.deaths, mdeath{at: m.c.t}), x.boundBy // its start of several's lease keeps it (#111)
	}
}

// ranAgain settles x, a start of several's container that died, as it runs
// again: at its start event, an up of its project in its worktree checked
// since a death takes it (lease.go ContainerEvent); else the start's lease
// keeps it until a reading (letGo). After a crash the book saw (crashed),
// only if it knows x's worktree (lease.go bindsAfterCrash).
func (m *model) ranAgain(x *mcont, crashed bool) {
	if !m.dead(x) {
		return
	}
	if d := &x.deaths[len(x.deaths)-1]; d.ran.IsZero() {
		d.ran = m.c.t
	}
	if !crashed || x.owned {
		m.letGo(x, m.eventBinder(x)) // else no project's lease binds it (lease.go bindsAfterCrash)
	}
}

// dead reports whether x is marked dead in the open start of several's
// lease that binds it (lease.go deadIn).
func (m *model) dead(x *mcont) bool {
	if x.deadIn != x.boundBy || !m.isOpen(x.deadIn) {
		x.deaths = nil // the mark ended with the lease
	}
	return len(x.deaths) > 0
}

// letGo binds x, dead, to lease id if it was checked in one of its deaths
// (takenBy): the start of several's lease lets it go, and waits for one
// container less (lease.go entry.letGo).
func (m *model) letGo(x *mcont, id string) {
	if m.takenBy(x, id) {
		m.several[x.boundBy]--
		x.boundBy, x.deaths, x.owned = id, nil, true
	}
}

// created is when open lease id was checked, and whether it is open.
func (m *model) created(id string) (time.Time, bool) {
	ls := m.b.List()
	if i := slices.IndexFunc(ls, func(l protocol.Lease) bool { return l.ID == id }); i >= 0 {
		return ls[i].Created, true
	}
	return time.Time{}, false
}

// bindsNoneAfterCrash reports whether a start of x binds no lease at its
// event though open lease id keys it: x crashed since the last reading
// showed it, and id was checked before the crash. The book still has x, so
// that is a start without a stop, no call's (lease.go ContainerEvent prev,
// checkedSinceCrash).
func (m *model) bindsNoneAfterCrash(x *mcont, id string) bool {
	created, ok := m.created(id)
	return ok && !x.crashed.IsZero() && m.inLast(x) && !created.After(x.crashed)
}

// takenBy reports whether lease id was checked in one of x's deaths, by
// when it ran again: its call can have started x (lease.go deaths.takenBy).
func (m *model) takenBy(x *mcont, id string) bool {
	created, ok := m.created(id)
	return ok && slices.ContainsFunc(x.deaths, func(d mdeath) bool { return !created.Before(d.at) && !created.After(d.ran) })
}

// held reports whether x is held: an up found it running and bound to no
// lease (lease.go), or a reading bound it when it came back (backReading),
// and that lease is open.
func (m *model) held(x *mcont) bool { return m.isOpen(x.heldBy) }

// bound reports whether a lease binds x, held or not.
func (m *model) bound(x *mcont) bool { return m.held(x) || m.isOpen(x.boundBy) }

func (m *model) isOpen(id string) bool {
	return id != "" && slices.ContainsFunc(m.b.List(), func(l protocol.Lease) bool { return l.ID == id })
}

// restart is Docker's restart policy bringing a crashed container back, as
// a start event and no call. It was checked if its first start was, and its
// call's reservation is long spent. Its event binds it as a call's start
// would (lease.go ContainerEvent): the lease keyed would (eventBinder),
// unless a lease still binds it, that lease was checked before the crash
// and the book still has it (bindsNoneAfterCrash), or the crash filter
// refuses it (bindsAfterCrash). Bound to none, it binds at the next
// reading it is new to (pending).
func (m *model) restart(x *mcont) {
	crashed := m.crashedToBook(x) // before it starts: the book's word for its start event
	bound := m.bound(x)           // a lease still binds it (lease.go boundAnywhere), or its dead mark's (ranAgain)
	id := m.eventBinder(x)
	if bound || m.bindsNoneAfterCrash(x, id) || crashed && !m.bindsAfterCrash(x, id) {
		id = ""
	}
	x.shown = m.inLast(x)
	x.running, x.cur, x.startedBy, x.startedAt, x.crashed, x.missedAt = true, 0, "", m.c.t, time.Time{}, 0
	x.began = m.ticks
	m.event("start", x)
	m.ranAgain(x, crashed)
	if id != "" {
		x.boundBy, x.owned = id, true                           // its project's lease (lease.go boundBy)
		x.verdict, x.verdictAt, x.vcrashed = true, m.c.t, false // a new verdict (lease.go judge)
	}
	x.pending = !bound && id == ""
	x.unbound = x.miss && !m.bound(x)
}

// bindsAfterCrash reports whether open lease id may bind x back after a
// crash: an up only in x's worktree, if the book knows it (lease.go
// bindsAfterCrash, owner).
func (m *model) bindsAfterCrash(x *mcont, id string) bool {
	ls := m.b.List()
	i := slices.IndexFunc(ls, func(l protocol.Lease) bool { return l.ID == id })
	return i >= 0 && x.owned && ls[i].Worktree == x.wt
}

// dockerStart starts xs in one call: docker start a b, where a lease may
// cover a (#88) and not b.
func (m *model) dockerStart(xs ...*mcont) bool {
	x := xs[0]
	wt := x.wt
	if wt == "" {
		wt = "w1"
	}
	r := policy.Request{Worktree: wt, Kind: "container", Command: "docker start " + x.id, Target: x.id,
		ContainerID: x.id, CostBytes: target, OnEngine: true, TakesOver: protocol.LeaseOf(x.labels)}
	for _, y := range xs[1:] {
		r.Command += " " + y.id
		r.Others = append(r.Others, policy.Start{ID: y.id, TakesOver: protocol.LeaseOf(y.labels)})
	}
	d := m.check(r)
	if !d.Allow {
		return false
	}
	m.step("%s: %s → %q (%s)", wt, r.Command, d.LeaseID, d.Message)
	by := d.LeaseID
	if by == "" {
		by = "covered"
	}
	for _, x := range xs {
		if by != "covered" && (!m.bound(x) || m.held(x)) {
			m.several[by]++ // one its lease waits for: not covered (lease.go Check)
		}
	}
	for _, x := range xs {
		bound := m.bound(x)
		m.start(x, by)
		if by != "covered" {
			x.verdict, x.verdictAt, x.vcrashed = true, m.c.t, false // its lease binds it by ID (lease.go judge)
		}
		if by != "covered" && !bound {
			x.judged, x.judgedAt = judgedGated, m.c.t // its lease binds it by ID
		}
	}
	return true
}

func (m *model) dockerRun(wt string) bool {
	d := m.check(policy.Request{Worktree: wt, Kind: "container", Command: "docker run img", CostBytes: target, Labelled: true})
	if !d.Allow {
		return false
	}
	attr := ""
	if m.cur.b%2 == 0 {
		attr = wt
	}
	x := m.newCont("", "", attr, map[string]string{protocol.LeaseLabel: d.LeaseID})
	x.firstGated = true
	m.step("%s: docker run → %s as %s", wt, d.LeaseID, x.id)
	m.start(x, d.LeaseID)
	return true
}

// forgeCompose is a repo in w2 whose compose service's labels: name another
// worktree's open lease. Its own up is checked; the label must count for
// nothing.
func (m *model) forgeCompose() {
	if len(m.of("evil", "w2")) > 0 {
		return // one stack; its later ups are the ordinary ones above
	}
	var victim string
	for _, l := range m.b.List() {
		if l.Worktree == "w1" {
			victim = l.ID
		}
	}
	d := m.check(policy.Request{Worktree: "w2", Kind: "compose", Op: "up", Command: "docker compose -p evil up -d",
		CostBytes: target, Target: "evil", OnEngine: true})
	if !d.Allow {
		return
	}
	x := m.newCont("evil", "e", "w2", map[string]string{protocol.ComposeProjectLabel: "evil",
		protocol.ComposeServiceLabel: "e", protocol.ComposeConfigHashLabel: "h", protocol.LeaseLabel: victim})
	m.step("w2: compose up evil labelled %q → %s as %s", victim, d.LeaseID, x.id)
	m.start(x, d.LeaseID)
	if victim != "" {
		// As declined: a live lease's label unbinds the forger's own stack
		// (false ungated, "never appeared" for its own lease). Only the
		// victim's reservation is checked.
		m.starts[d.LeaseID]--
		x.startedBy = ""
	} else {
		x.firstGated = true
	}
}

// dressedRun is a docker run in w2 dressed as a container of w1's p1. The
// shim labels it with its own lease; it must not bind p1's.
func (m *model) dressedRun() {
	d := m.check(policy.Request{Worktree: "w2", Kind: "container", Command: "docker run -l com.docker.compose.project=p1 img",
		CostBytes: target, Labelled: true})
	if !d.Allow {
		return
	}
	x := m.newCont("", "", "", map[string]string{protocol.ComposeProjectLabel: "p1",
		protocol.ComposeConfigHashLabel: "h", protocol.LeaseLabel: d.LeaseID})
	m.step("w2: dressed docker run → %s as %s", d.LeaseID, x.id)
	m.start(x, d.LeaseID)
	m.starts[d.LeaseID]-- // it loses its own lease, as declined: only the forger is hurt
	x.boundBy = ""        // and binds none (lease.go key: its mark)
}

func (m *model) rawRun() {
	x := m.newCont("", "", "", nil)
	m.step("raw docker run as %s", x.id)
	m.start(x, "")
}

var neverRE = regexp.MustCompile(`never appeared.*lease=(\S+)`)

func (m *model) tick() {
	m.c.t = m.c.t.Add(5 * time.Second)
	if m.low--; m.headroom != plenty && m.low < 0 {
		m.headroom = plenty
		m.step("headroom plenty again")
	}
	for _, x := range m.conts {
		if x.running {
			x.cur = min(x.cur+target/2, target)
		}
	}
	m.ticks++
	m.flick = (m.cur.a+m.ticks*7)%8 == 0
	for _, x := range m.conts {
		if x.running && !x.missing {
			// A hit's container the last reading before its start showed is
			// not new to its first (shown): back after missing it, it is
			// back as any other (#168 F4).
			back, gone := (x.guess == "" || x.shown) && x.missedAt > 0 && x.missedAt == m.ticks-1, x.read < m.ticks-2
			x.read = m.ticks
			x.owned, x.verdict, x.verdictAt = x.owned || !m.flick && x.wt != "", true, m.c.t
			m.deadReading(x)
			m.firstReading(x)
			m.pendingReading(x)
			if back {
				m.backReading(x, gone)
			}
		}
	}
	m.judge()
	m.readAt = m.c.t
	m.step("tick %s (flicker %v)", m.c.t.Format("15:04:05"), m.flick)
	m.b.Observe(m.snapshot())
	for _, x := range m.conts {
		if x.running && !x.missing {
			x.vcrashed = false // after the reading's bindings (lease.go Observe)
		}
		if x.verdict && m.c.t.Sub(x.verdictAt) >= timeout {
			x.verdict, x.owned, x.vcrashed = false, false, false // absent for the timeout (lease.go Observe)
		}
	}
	for _, x := range m.conts {
		switch {
		case !x.miss:
		case x.running && !x.missing:
			x.judgedAt = m.c.t
		case m.c.t.Sub(x.judgedAt) >= timeout:
			x.judged = unjudged // absent for the timeout (lease.go Observe)
		}
	}
	m.invariants()
	m.heldForNothing()
}

// judge gives each miss's container this reading shows its verdict: its
// kept one, else gated if a sibling the reading shows has a gated one
// (lease.go gatedProject, judged before this reading's), else ungated if
// no lease binds it, else none yet (baseline).
func (m *model) judge() {
	shows := func(y *mcont) bool { return y.running && !y.missing }
	gated := map[*mcont]bool{}
	for _, x := range m.conts {
		gated[x] = x.miss && shows(x) && x.judged == judgedGated
	}
	for _, x := range m.conts {
		switch {
		case !x.miss || !shows(x) || x.judged != unjudged:
		case !x.unbound:
			x.judged = judgedBaseline
		case slices.ContainsFunc(m.of(x.project, x.wt), func(y *mcont) bool { return y != x && gated[y] }):
			x.judged = judgedGated
		default:
			x.judged = judgedUngated
		}
		if shows(x) {
			x.unbound = false
		}
	}
}

// deadReading ends x's dead mark at the first reading that shows it running
// again: the lease that reading binds it to takes it if it was checked in
// one of its deaths (lease.go Observe, deadIn).
func (m *model) deadReading(x *mcont) {
	if !m.dead(x) {
		return
	}
	m.letGo(x, m.binder(x))
	x.deaths = nil
}

// pendingReading binds x, whose start bound no lease at its event
// (mcont.pending), at the next reading, if x is new to it (lease.go Observe
// fresh, binder).
func (m *model) pendingReading(x *mcont) {
	if x.pending && !x.shown && !m.bound(x) {
		x.boundBy = m.binder(x)
	}
	x.pending = false
}

// firstReading settles a guessed hit's container at its first reading: if
// the reading attributes it, it binds the lease keyed would (binder), and is
// gated; a guess it binds is confirmed. Unless an up of its project there
// took it at its event and still binds it, or the last reading showed it,
// so it is not new. A flickered reading binds it to an up that keys it
// with no attribution (binder), which judges it gated, else leaves it
// judged as with no key (O1): nothing is asserted of it, unless an up took
// it, which judged it gated.
func (m *model) firstReading(x *mcont) {
	if x.guess == "" {
		return
	}
	switch {
	case x.taken && (m.bound(x) || m.flick):
	case m.flick:
		m.outcomes["flicker"]++
		if id := m.binder(x); id != "" && !x.shown && !x.pending {
			x.boundBy, x.owned = id, true // an up keys it with no attribution, as at an event (#168 F5)
		} else {
			x.firstGated = false
		}
	case x.shown:
	default:
		if id := m.binder(x); id != "" {
			x.boundBy = id
		}
	}
	x.guess = ""
}

// backReading settles a container back after a reading it was missing
// from, and bound to no lease, whoever started it: it binds the lease the
// reading keys it to (binder). Missing from one, it is held (lease.go
// Observe) until it stops: a guess that holds it is confirmed (lease.go
// composeTakes) until then. Missing from two in a row, it went, and binds
// as new, as at a first reading.
func (m *model) backReading(x *mcont, gone bool) {
	if x.miss || m.bound(x) {
		return
	}
	id := m.binder(x)
	x.owned = x.owned || id != "" // its project's lease's worktree (lease.go boundBy)
	if !gone {
		x.heldBy = id
		return
	}
	x.boundBy = id
}

// confirmed reports whether guess id binds a container, held or not: a
// later up of its project in its worktree takes it over then (lease.go
// composeTakes).
func (m *model) confirmed(id string) bool {
	return slices.ContainsFunc(m.conts, func(x *mcont) bool { return x.boundBy == id || x.heldBy == id })
}

// binder is the open lease a reading binds x, a new container of a compose
// project, to (lease.go keyed): an up of the project in x's worktree (its
// dir), then, if the reading attributes x, a guess of the project there,
// then an up of the project in another worktree; the oldest of equals.
// Back after a crash its verdict kept (vcrashed), only a lease of the
// worktree the book knows it in binds it (lease.go binder, bindsAfterCrash).
func (m *model) binder(x *mcont) string {
	if x.vcrashed && m.flick && !x.owned {
		return "" // the book knows no worktree of it
	}
	if up := m.upOf(x.project, x.wt); up != "" {
		return up
	}
	if g := m.guessOf(x); g != "" && !m.flick {
		return g
	}
	if x.vcrashed {
		return ""
	}
	return m.upElsewhere(x)
}

// eventBinder is the open lease x's start event binds it to (lease.go
// keyed, tied): an up of its project in its worktree, else one in another
// worktree, unless a guess of the project in x's worktree, which may be
// x's, ties with it.
func (m *model) eventBinder(x *mcont) string {
	if up := m.upOf(x.project, x.wt); up != "" || m.guessOf(x) != "" {
		return up
	}
	return m.upElsewhere(x)
}

// guessOf is the oldest open guess in x's worktree that names its project.
func (m *model) guessOf(x *mcont) string {
	for _, l := range m.b.List() {
		if g := m.guesses[l.ID]; l.Worktree == x.wt && g != nil && g.name == x.project {
			return l.ID
		}
	}
	return ""
}

// upElsewhere is the oldest open up of x's project in another worktree.
func (m *model) upElsewhere(x *mcont) string {
	for _, l := range m.b.List() {
		if l.Worktree != x.wt && x.project != "" && m.leases[l.ID] == upCommand(x.project) {
			return l.ID
		}
	}
	return ""
}

// upCommand is the call of an up of project, named with -p.
func upCommand(project string) string { return "docker compose -p " + project + " up -d" }

// upOf is the oldest open lease of an up of project in wt, "" if none.
func (m *model) upOf(project, wt string) string {
	for _, l := range m.b.List() {
		if l.Worktree == wt && m.leases[l.ID] == upCommand(project) {
			return l.ID
		}
	}
	return ""
}

func (m *model) invariants() {
	m.t.Helper()
	ungated := map[string]bool{}
	for _, u := range m.b.Ungated() {
		ungated[strings.TrimPrefix(u.Key, "container:")] = true
	}
	var need uint64
	for _, x := range m.conts {
		if !x.running {
			continue
		}
		switch {
		case x.missing:
			// Not in this reading, so neither flagged nor not. It still
			// needs what it was checked for.
		case x.firstGated && ungated[x.id]:
			m.fail("false ungated: %s was started by a checked call", x.id)
		case x.raw && !ungated[x.id] && m.c.t.Sub(x.startedAt) >= 5*time.Second:
			m.fail("missing ungated: %s started past the shim", x.id)
		case x.miss && x.judged == judgedUngated && !ungated[x.id] && m.c.t.Sub(x.startedAt) >= 5*time.Second:
			m.fail("miss not ungated: %s was started by a guessed up that missed", x.id)
		}
		if x.startedBy != "" && x.cur < target && m.c.t.Sub(x.startedAt) < 2*time.Minute {
			need += target - x.cur
		}
	}
	if got := reserved(m.b); got < need {
		m.fail("lost reservation: %d MiB reserved, warming checked containers need %d MiB", got>>20, need>>20)
	}
	for _, mm := range neverRE.FindAllStringSubmatch(m.log.String(), -1) {
		if id := strings.Trim(mm[1], `"`); m.starts[id] > 0 {
			m.fail("false never appeared: %s (%s) started %d container(s)", id, m.leases[id], m.starts[id])
		}
	}
}

// heldForNothing fails on a lease held for no reason: every lease is
// renewed or taken by an allowed call, so none expires later than the
// timeout after the last one. A denial extends nothing (#88).
func (m *model) heldForNothing() {
	for _, l := range m.b.List() {
		if limit := m.last.Add(timeout); l.Expires.After(limit) {
			m.fail("held for no reason: %s (%s) expires at %s, after %s, the timeout after the last allowed call",
				l.ID, m.leases[l.ID], l.Expires.Format("15:04:05.000"), limit.Format("15:04:05.000"))
		}
	}
}

// call is the agent's call that op kind makes, nil if it has nothing to
// start; the call reports whether it was allowed.
func (m *model) call(kind int, o op) func() bool {
	switch kind {
	case 0, 1, 2:
		st := m.stackAt(o.a)
		guess := -1
		if kind == 2 && o.b%2 == 1 {
			guess = (o.b / 2) % 3
		}
		return func() bool { return m.composeUp(st, guess) }
	case 5:
		stopped := func(x *mcont) bool { return !x.running && !x.gone }
		x := m.pick(stopped)
		if x == nil {
			return nil
		}
		xs := []*mcont{x}
		if y := m.pick(func(y *mcont) bool { return stopped(y) && y != x }); y != nil && o.a%3 == 0 {
			xs = append(xs, y) // docker start a b
		}
		return func() bool { return m.dockerStart(xs...) }
	case 7:
		wt := []string{"w1", "w2"}[o.a%2]
		return func() bool { return m.dockerRun(wt) }
	}
	return nil
}

func (m *model) run(ops []op) {
	for _, o := range ops {
		m.cur = o
		m.c.t = m.c.t.Add(10 * time.Millisecond) // calls and events take time
		st := m.stackAt(o.a)
		switch o.kind {
		case 0, 1, 2, 5, 7:
			if call := m.call(o.kind, o); call != nil {
				call()
			}
		case 3:
			if slices.ContainsFunc(m.of(st.project, st.wt), m.tiedUnread) {
				break // #109
			}
			for _, x := range m.of(st.project, st.wt) {
				if x.running {
					m.stop(x, false)
					m.step("compose stop %s", x.id)
				}
			}
		case 4:
			if slices.ContainsFunc(m.of(st.project, st.wt), m.tiedUnread) {
				break // #109
			}
			for _, x := range m.of(st.project, st.wt) {
				if x.running {
					m.stop(x, false)
				}
				x.gone = true
				m.step("compose down %s", x.id)
			}
		case 6:
			if x := m.pick(func(x *mcont) bool { return x.running && !m.tiedUnread(x) }); x != nil {
				m.stop(x, o.b%2 == 0)
				m.step("stop/crash(%v) %s", o.b%2 == 0, x.id)
			}
		case 8:
			m.rawRun()
		case 12:
			m.forgeCompose()
		case 13:
			m.dressedRun()
		case 9:
			m.step("wait 1m")
			for range 12 {
				m.tick() // the daemon reads every 5 s
			}
		case 14:
			// Memory runs short for lowReadings readings, half the time, or
			// frees up again: the next reading says.
			m.headroom, m.low = []int64{plenty, 0, plenty, int64(gib), plenty, int64(2 * gib)}[o.b%6], lowReadings
			m.step("headroom %d MiB", m.headroom>>20)
			m.tick()
		case 15:
			// A call with BUDGET_WAIT=1: denied, it asks again after each
			// reading, until it is allowed or gives up. Memory runs short
			// for waitReadings readings, then is as it was.
			call := m.call([]int{0, 5, 7}[o.b%3], o)
			if call == nil {
				break
			}
			was, wasLow := m.headroom, m.low
			m.headroom, m.low = 0, waitReadings
			m.step("BUDGET_WAIT, headroom 0 MiB")
			m.tick()
			short := true
			restore := func() {
				m.headroom, m.low, short = was, wasLow, false
				m.step("headroom %d MiB again", m.headroom>>20)
			}
			for i := 0; !call() && i < waitReadings; i++ {
				if i == waitReadings-1 {
					restore()
				}
				m.tick()
			}
			if short {
				// Allowed while memory runs short (covered, idle): the next
				// reading says it is as it was.
				restore()
				m.tick()
			}
		case 16:
			// A running container left out of one reading: its stats failed,
			// or a restart backoff spans it. Left out of the last one too, it
			// is gone: the book lets go of it (lease.go).
			x := m.pick(func(x *mcont) bool { return x.running })
			if x == nil {
				break
			}
			twice := x.missedAt == m.ticks && m.ticks > 0
			x.missing = true
			m.step("%s missing from the reading", x.id)
			m.tick()
			x.missing, x.missedAt = false, m.ticks
			if twice {
				x.heldBy = ""
			}
		case 17:
			// Docker's restart policy restarts a crashed container, with a
			// backoff of at most a minute, which may span readings. One
			// labelled with another worktree's project is #89's (nameTaken).
			// A removed container stays down.
			ok := func(x *mcont) bool { return m.on(89) || !nameTaken(x) }
			if x := m.pick(func(x *mcont) bool {
				return !x.gone && !x.crashed.IsZero() && m.c.t.Sub(x.crashed) <= time.Minute && ok(x)
			}); x != nil {
				m.restart(x)
				m.step("restart policy started %s", x.id)
			}
		default:
			m.tick()
		}
		if m.diff {
			m.differ()
		}
	}
}

// differ fails where the model's bookkeeping of guessed ups is not the
// book's: the open leases that bind each container a guess started, and
// held which, and whether each open guess bound anything, which a later up
// of its project takes over (lease.go composeTakes).
func (m *model) differ() {
	for _, x := range m.conts {
		if !x.byGuess {
			continue
		}
		var want []string
		for _, l := range m.b.List() {
			switch l.ID {
			case x.heldBy:
				want = append(want, l.ID+" (held)")
			case x.boundBy:
				want = append(want, l.ID)
			}
		}
		if got := lease.Bindings(m.b, x.id); !slices.Equal(got, want) {
			m.fail("binding differs: %s is bound by %q in the model, %q in the book", x.id, want, got)
		}
	}
	for _, l := range m.b.List() {
		g := m.guesses[l.ID]
		if g == nil {
			continue
		}
		if c := m.confirmed(l.ID); c != (len(lease.Bound(m.b, l.ID)) > 0) {
			m.fail("confirmed differs: the guess %s is confirmed %v in the model, binds anything %v in the book", l.ID, c, !c)
		}
	}
}

func (m *model) pick(ok func(*mcont) bool) *mcont {
	var xs []*mcont
	for _, x := range m.conts {
		if ok(x) {
			xs = append(xs, x)
		}
	}
	if len(xs) == 0 {
		return nil
	}
	return xs[m.cur.b%len(xs)]
}

// play runs ops on a fresh book: the failure, if any.
func play(t *testing.T, ops []op) *failure {
	f, _ := playCounting(t, ops)
	return f
}

// playCounting is play, and how often each guessed outcome ran.
func playCounting(t *testing.T, ops []op) (f *failure, outcomes map[string]int) {
	m := newModel(t)
	return m.try(ops), m.outcomes
}

// try runs ops: the failure, if any.
func (m *model) try(ops []op) (f *failure) {
	defer func() {
		if r := recover(); r != nil {
			ff, ok := r.(failure)
			if !ok {
				panic(r)
			}
			f = &ff
		}
	}()
	m.run(ops)
	return nil
}

// runOK runs ops in a directed case, which a failure ends.
func (m *model) runOK(ops ...op) {
	m.t.Helper()
	if f := m.try(ops); f != nil {
		m.t.Fatalf("%s", f.msg)
	}
}

// reserves is what lease id reserves, and whether it is open.
func (m *model) reserves(id string) (uint64, bool) {
	ls := m.b.List()
	i := slices.IndexFunc(ls, func(l protocol.Lease) bool { return l.ID == id })
	if i < 0 {
		return 0, false
	}
	return ls[i].Bytes, true
}

// newModel is a model on a fresh book, which has read an empty snapshot.
func newModel(t *testing.T) *model {
	b, c, log := book(t)
	m := &model{t: t, b: b, c: c, log: log, leases: map[string]string{}, starts: map[string]int{}, headroom: plenty,
		open: openIssues(), guesses: map[string]*mguess{}, outcomes: map[string]int{}, several: map[string]int{}, diff: os.Getenv("HEADROOM_MODEL_DIFF") == "1"}
	b.Observe(m.snapshot())
	return m
}

// shrink drops ops one at a time while the same kind of failure stays.
func shrink(t *testing.T, ops []op, kind string) []op {
	for again := true; again; {
		again = false
		for i := 0; i < len(ops); i++ {
			try := slices.Delete(slices.Clone(ops), i, i+1)
			if f := play(t, try); f != nil && f.kind == kind {
				ops, again = try, true
				i--
			}
		}
	}
	return ops
}

// TestTheLeaseModel runs HEADROOM_MODEL_SEEDS random runs (default 300)
// and shows each failure shrunk to the ops it needs. HEADROOM_MODEL_OPEN
// turns on the cases of open bugs (openBugs).
func TestTheLeaseModel(t *testing.T) {
	seeds := 300
	if v, err := strconv.Atoi(os.Getenv("HEADROOM_MODEL_SEEDS")); err == nil {
		seeds = v
	}
	kinds, outcomes := map[string]int{}, map[string]int{}
	for seed := range seeds {
		rng := rand.New(rand.NewPCG(uint64(seed), 1))
		ops := make([]op, 80)
		for i := range ops {
			ops[i] = op{rng.IntN(18), rng.IntN(1000), rng.IntN(1000)}
		}
		f, n := playCounting(t, ops)
		for k, v := range n {
			outcomes[k] += v
		}
		if f == nil {
			continue
		}
		kinds[f.kind]++
		if kinds[f.kind] > 1 {
			continue
		}
		small := shrink(t, ops, f.kind)
		t.Errorf("seed %d, shrunk to %d ops:\n%s", seed, len(small), play(t, small).msg)
	}
	if len(kinds) > 0 {
		t.Logf("failing seeds by kind: %v", kinds)
	}
	// Each guessed outcome ran (#120), at the default seeds.
	t.Logf("guessed outcomes: %v", outcomes)
	if seeds >= 300 {
		for _, k := range []string{"hit", "miss", "miss other", "takeover", "flicker"} {
			if outcomes[k] == 0 {
				t.Errorf("no guessed %s ran: %v", k, outcomes)
			}
		}
	}
}

// TestTheLeaseModelOn87 plays #87's case, which random runs do not reach:
// a service warms in two readings, and a missing reading and the next take
// two. An up holds c1 and c2, which run; c2 stops; c1 is missing from one
// reading while that up's lease holds it; an up of the stack starts c2;
// c1 comes back.
func TestTheLeaseModelOn87(t *testing.T) {
	ops := []op{
		{0, 0, 0},  // w1: compose up p1, starts c1 and c2
		{10, 0, 0}, // tick
		{10, 0, 0}, // tick: they use the lease's cost, it ends
		{0, 0, 0},  // w1: compose up p1 holds c1 and c2
		{6, 0, 1},  // stop c2
		{16, 0, 0}, // c1, held, missing from the reading
		{0, 0, 0},  // w1: compose up p1 starts c2
		{10, 0, 0}, // tick: c1 is back
	}
	if f := play(t, ops); f != nil {
		t.Errorf("#87:\n%s", f.msg)
	}
}

// TestTheLeaseModelOnAVerdictAged plays a case random runs do not reach: a
// miss's container whose gated verdict aged out is ungated again when a
// miss restarts it (#120). w1's guessed up misses and starts c1 and c2; c2
// stops, a docker start of it judges it gated, and it stops again; it stays
// stopped past the timeout; a miss starts it again.
func TestTheLeaseModelOnAVerdictAged(t *testing.T) {
	ops := []op{
		{2, 0, 3}, {10, 0, 0}, {10, 0, 0}, // w1: a guessed up that misses starts c1 and c2
		{6, 0, 1}, {10, 0, 0}, // stop c2
		{5, 0, 1}, {10, 0, 0}, // docker start c2: gated
		{6, 0, 1}, {10, 0, 0}, // stop c2
		{9, 0, 0}, {9, 0, 0}, {9, 0, 0}, // 3m: its verdict ages out
		{2, 0, 3}, {10, 0, 0}, {10, 0, 0}, // the miss again starts c2: ungated
	}
	if f := play(t, ops); f != nil {
		t.Errorf("a verdict aged:\n%s", f.msg)
	}
}

// TestTheLeaseModelOnAGuessTakenAtItsStart plays a case random runs reach
// and nothing asserts (#120's probe P6, #150): an up of p1, open in w1,
// takes what a guessed hit of p1 starts at its start event, which the guess
// does not key (lease.go entry.key). An up starts c1 and c2; they go; the hit
// starts c3 and c4. Both are the up's, as in the book, which spends the
// up's reservation on them at the next reading and binds the guess nothing.
func TestTheLeaseModelOnAGuessTakenAtItsStart(t *testing.T) {
	m := newModel(t)
	m.runOK(op{0, 0, 0}) // w1: compose up p1 starts c1 and c2
	up := m.b.List()[0].ID
	m.runOK(
		op{4, 0, 0}, // compose down p1
		op{2, 0, 1}, // w1: a guessed hit of p1 starts c3 and c4
	)
	ls := m.b.List()
	guess := ls[len(ls)-1].ID
	bound := func(when string) {
		xs := m.of("p1", "w1")
		if len(xs) != 2 {
			t.Fatalf("%s, p1 in w1 has %d containers, want c3 and c4", when, len(xs))
		}
		for _, x := range xs {
			if x.boundBy != up {
				t.Errorf("%s, %s boundBy = %s, want the up's %s (the guess is %s)", when, x.id, x.boundBy, up, guess)
			}
		}
	}
	bound("at its start")
	m.runOK(op{10, 0, 0}) // tick
	bound("after the reading")
	if n, ok := m.reserves(up); !ok || n != target {
		t.Errorf("the up %s reserves %d MiB (open %v), want %d MiB: c3 and c4 use half its cost", up, n>>20, ok, target>>20)
	}
	if n, ok := m.reserves(guess); !ok || n != 2*target {
		t.Errorf("the guess %s reserves %d MiB (open %v), want all %d MiB: the book bound it nothing", guess, n>>20, ok, 2*target>>20)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAGuessOfAnotherDirAtItsStart plays #150's F1 (its probe Q1):
// a guessed hit starts containers while an up of their project is open in
// its worktree, and another worktree's open guess names that project. That
// guess's dir is not the one Compose labels them with, so the start event
// does not tie (lease.go keyed, #109): the up takes them at it. w2's up of
// p2 starts c1 and c2; a minute on, w1's guessed up names p2; w2's idle up
// of p2 holds c1 and c2; they stop; w2's guessed hit of p2 starts them
// again.
func TestTheLeaseModelOnAGuessOfAnotherDirAtItsStart(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{2, 177, 196}, // w2: compose up p2 starts c1 and c2
		op{9, 668, 538}, // wait 1m: its lease ends
		op{2, 756, 965}, // w1: a guessed up names p2 and comes up as m1
		op{0, 273, 657}, // w2: an idle compose up p2 holds c1 and c2
	)
	ls := m.b.List()
	up := ls[len(ls)-1].ID
	m.runOK(
		op{3, 881, 428}, // compose stop p2
		op{2, 169, 949}, // w2: a guessed hit of p2 starts c1 and c2
	)
	bound := func(when string) {
		xs := m.of("p2", "w2")
		if len(xs) != 2 {
			t.Fatalf("%s, p2 in w2 has %d containers, want c1 and c2", when, len(xs))
		}
		for _, x := range xs {
			if x.boundBy != up {
				t.Errorf("%s, %s boundBy = %q, want w2's idle up's %s: w1's guess does not tie", when, x.id, x.boundBy, up)
			}
		}
	}
	bound("at its start")
	m.runOK(op{10, 0, 0}, op{10, 0, 0}, op{10, 0, 0}) // ticks
	bound("after the readings")
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAGuessBackAfterAMissingReading plays #150's F2 (its
// probe Q2): a guessed hit's container, taken by an up at its start, is
// missing from a reading after the up ended, and back in the next. The book
// holds it then for the open guess (lease.go Observe), without spending the
// guess's reservation, until it stops: a later up takes nothing over.
func TestTheLeaseModelOnAGuessBackAfterAMissingReading(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{0, 0, 0}, // w1: compose up p1 starts c1 and c2
		op{4, 0, 0}, // compose down p1
		op{2, 0, 1}, // w1: a guessed hit of p1 starts c3 and c4, the up's
		op{10, 0, 0},
		op{10, 0, 0},
		op{10, 0, 0}, // ticks: the up ends
	)
	ls := m.b.List()
	if len(ls) != 1 || m.guesses[ls[0].ID] == nil {
		t.Fatalf("leases %+v, want only the guess", ls)
	}
	guess := ls[0].ID
	m.runOK(
		op{16, 0, 0}, // c3 missing from a reading
		op{10, 0, 0}, // tick: c3 is back
	)
	c3 := m.of("p1", "w1")[0]
	if !m.held(c3) || c3.heldBy != guess {
		t.Errorf("%s held by %q (held %v), want by the guess %s, as in the book", c3.id, c3.heldBy, m.held(c3), guess)
	}
	if n, ok := m.reserves(guess); !ok || n != 2*target {
		t.Errorf("the guess %s reserves %d MiB (open %v), want all %d MiB: it holds c3", guess, n>>20, ok, 2*target>>20)
	}
	m.runOK(op{3, 0, 0}) // compose stop p1: the guess lets c3 go
	if m.bound(c3) {
		t.Errorf("%s bound by %q, want by nothing: it stopped", c3.id, cmp.Or(c3.heldBy, c3.boundBy))
	}
	m.runOK(op{0, 0, 0}) // w1: compose up p1 starts c3 and c4
	if _, ok := m.reserves(guess); !ok || m.outcomes["takeover"] != 0 {
		t.Errorf("the guess %s open %v, takeovers %d: want it open, bound nothing, in the book and the model", guess, ok, m.outcomes["takeover"])
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAGuessBackAfterItWent is F2 with c3 missing from two
// readings in a row: gone to the book, it is new when it comes back, and
// binds the guess as at a first reading (lease.go Observe fresh), which a
// later up then takes over (composeTakes).
func TestTheLeaseModelOnAGuessBackAfterItWent(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{0, 0, 0}, // w1: compose up p1 starts c1 and c2
		op{4, 0, 0}, // compose down p1
		op{2, 0, 1}, // w1: a guessed hit of p1 starts c3 and c4, the up's
		op{10, 0, 0},
		op{10, 0, 0},
		op{10, 0, 0}, // ticks: the up ends
	)
	ls := m.b.List()
	if len(ls) != 1 || m.guesses[ls[0].ID] == nil {
		t.Fatalf("leases %+v, want only the guess", ls)
	}
	guess := ls[0].ID
	m.runOK(
		op{16, 0, 0}, // c3 missing from a reading
		op{16, 0, 0}, // and the next: gone
		op{10, 0, 0}, // tick: c3 is back
	)
	c3 := m.of("p1", "w1")[0]
	if c3.boundBy != guess || !m.confirmed(guess) {
		t.Errorf("%s boundBy = %q, the guess %s confirmed %v: want the guess's, confirmed, as in the book", c3.id, c3.boundBy, guess, m.confirmed(guess))
	}
	m.runOK(
		op{3, 0, 0}, // compose stop p1
		op{0, 0, 0}, // w1: compose up p1 starts c3 and c4: it takes the guess over
	)
	if _, ok := m.reserves(guess); ok || m.outcomes["takeover"] != 1 {
		t.Errorf("the guess %s open %v, takeovers %d: want it taken over, in the book and the model", guess, ok, m.outcomes["takeover"])
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAGuessFirstReadAfterItsUpEnded plays #150's F2 at a
// container's first reading: an up took a guessed hit's container at its
// start, and ended while a reading missed it. The next reading finds it new
// and bound to no lease, and binds it to the guess (lease.go Observe fresh),
// which a later up then takes over (composeTakes). An up starts c1 and c2;
// a minute on, an idle up holds them; they go; the hit starts c3 and c4,
// the idle up's; c3 misses a reading, at which the idle up ends.
func TestTheLeaseModelOnAGuessFirstReadAfterItsUpEnded(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{0, 0, 0}, // w1: compose up p1 starts c1 and c2
		op{9, 0, 0}, // wait 1m: its lease ends
		op{0, 0, 0}, // w1: an idle compose up p1 holds c1 and c2
		op{4, 0, 0}, // compose down p1
		op{2, 0, 1}, // w1: a guessed hit of p1 starts c3 and c4, the idle up's
	)
	ls := m.b.List()
	guess := ls[len(ls)-1].ID
	m.runOK(
		op{16, 0, 0}, // c3 missing from a reading: the idle up ends
		op{10, 0, 0}, // tick: c3's first reading
	)
	if len(m.b.List()) != 1 {
		t.Fatalf("leases %+v, want only the guess", m.b.List())
	}
	c3 := m.of("p1", "w1")[0]
	if c3.boundBy != guess || !m.confirmed(guess) {
		t.Errorf("%s boundBy = %q, the guess %s confirmed %v: want the guess's, confirmed, as in the book", c3.id, c3.boundBy, guess, m.confirmed(guess))
	}
	m.runOK(
		op{3, 0, 0}, // compose stop p1
		op{0, 0, 0}, // w1: compose up p1 starts c3 and c4: it takes the guess over
	)
	if _, ok := m.reserves(guess); ok || m.outcomes["takeover"] != 1 {
		t.Errorf("the guess %s open %v, takeovers %d: want it taken over, in the book and the model", guess, ok, m.outcomes["takeover"])
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAGuessThenAnIdleUp plays #150's probe Q4: an up of p1
// in w1 comes before a guess of it at a reading (lease.go keyed), even an
// idle one. A guessed hit of p1 starts c1 and c2; an idle up of p1 opens;
// the reading binds them to the up, which ends, and the guess nothing.
func TestTheLeaseModelOnAGuessThenAnIdleUp(t *testing.T) {
	m := newModel(t)
	m.runOK(op{2, 0, 1}) // w1: a guessed hit of p1 starts c1 and c2
	guess := m.b.List()[0].ID
	m.runOK(
		op{0, 0, 0},  // w1: an idle compose up p1
		op{10, 0, 0}, // tick: the up binds c1 and c2
	)
	if m.confirmed(guess) {
		t.Errorf("the guess %s confirmed, want not: the up binds c1 and c2", guess)
	}
	for _, x := range m.of("p1", "w1") {
		if m.bound(x) {
			t.Errorf("%s bound by %s, want by nothing open: the up that bound it ended", x.id, x.boundBy)
		}
	}
	if n, ok := m.reserves(guess); !ok || n != 2*target {
		t.Errorf("the guess %s reserves %d MiB (open %v), want all %d MiB: the book bound it nothing", guess, n>>20, ok, 2*target>>20)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAGuessTakenThenAFlicker plays a taken hit whose first
// reading has no attribution: the up bound its containers at their start
// event, so they stay gated (firstGated), and the flicker asserts nothing
// less of them.
func TestTheLeaseModelOnAGuessTakenThenAFlicker(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{0, 0, 0},  // w1: compose up p1 starts c1 and c2
		op{4, 0, 0},  // compose down p1
		op{2, 0, 1},  // w1: a guessed hit of p1 starts c3 and c4, the up's
		op{10, 1, 0}, // tick: a flicker ((1 + 7) % 8)
	)
	if !m.flick {
		t.Fatal("the reading had attribution, want a flicker")
	}
	m.runOK(op{10, 0, 0}) // tick
	for _, x := range m.of("p1", "w1") {
		if !x.firstGated {
			t.Errorf("%s not asserted gated, want it: the up bound it at its start", x.id)
		}
	}
	if n := m.outcomes["flicker"]; n != 0 {
		t.Errorf("%d flickered first readings, want none: the up took the hit's containers", n)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAnIdleUpBeforeAReading plays #168's differential's
// first row: an up holds only what the last reading showed running (lease.go
// Check stack), and before any reading, it showed nothing. A guessed hit of
// p1 starts c1 and c2; an idle up of p1 holds neither.
func TestTheLeaseModelOnAnIdleUpBeforeAReading(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{2, 0, 1}, // w1: a guessed hit of p1 starts c1 and c2
		op{0, 0, 0}, // w1: an idle compose up p1
	)
	for _, x := range m.of("p1", "w1") {
		if m.held(x) {
			t.Errorf("%s held by %s, want by nothing: no reading showed it", x.id, x.heldBy)
		}
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAGuessThatHolds plays #168's F3: a guess that holds a
// container has bound it (lease.go composeTakes counts held ones), so a later
// up of its project in its worktree takes it over, and holds what it held.
// As TestTheLeaseModelOnAGuessBackAfterAMissingReading, until the guess
// holds c3; an up of p1 then comes while c3 runs.
func TestTheLeaseModelOnAGuessThatHolds(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{0, 0, 0}, // w1: compose up p1 starts c1 and c2
		op{4, 0, 0}, // compose down p1
		op{2, 0, 1}, // w1: a guessed hit of p1 starts c3 and c4, the up's
		op{10, 0, 0},
		op{10, 0, 0},
		op{10, 0, 0}, // ticks: the up ends
		op{16, 0, 0}, // c3 missing from a reading
		op{10, 0, 0}, // tick: c3 is back, held by the guess
	)
	ls := m.b.List()
	if len(ls) != 1 || m.guesses[ls[0].ID] == nil {
		t.Fatalf("leases %+v, want only the guess", ls)
	}
	guess := ls[0].ID
	m.runOK(op{0, 0, 0}) // w1: compose up p1, idle: it takes the guess over
	ls = m.b.List()
	up := ls[len(ls)-1].ID
	if _, ok := m.reserves(guess); ok || m.outcomes["takeover"] != 1 {
		t.Errorf("the guess %s open %v, takeovers %d: want it taken over, in the book and the model", guess, ok, m.outcomes["takeover"])
	}
	if c3 := m.of("p1", "w1")[0]; c3.heldBy != up || !m.held(c3) {
		t.Errorf("%s held by %q (held %v), want by the up %s, which took the guess over", c3.id, c3.heldBy, m.held(c3), up)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnATakeoverOfRemovedContainers plays #168's (d): an up
// that takes a lease over binds all it bound, removed containers too
// (lease.go Check). A guessed hit of app in w1 binds c1 and c2 at a reading;
// compose down removes them; an up of app takes the guess over.
func TestTheLeaseModelOnATakeoverOfRemovedContainers(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{2, 2, 1},  // w1: a guessed hit of app starts c1 and c2
		op{10, 0, 0}, // tick: they bind the guess
		op{4, 2, 0},  // compose down app
		op{0, 2, 0},  // w1: compose up app starts c3 and c4: it takes the guess over
	)
	ls := m.b.List()
	up := ls[len(ls)-1].ID
	if m.outcomes["takeover"] != 1 {
		t.Fatalf("takeovers %d, want the up %s to take the guess over", m.outcomes["takeover"], up)
	}
	for _, x := range m.conts[:2] {
		if x.boundBy != up {
			t.Errorf("%s (removed) boundBy = %q, want the up's %s, which took the guess over", x.id, x.boundBy, up)
		}
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnACoveredStartOfSeveralThatStops plays #168's #111
// let-go row: a docker start a b its lease covers makes no lease, so the
// stop after it lets nothing go (lease.go markDead marks only a start's
// lease). A guessed hit of p1 binds c1 and c2 at a reading; compose stop;
// docker start c1 c2, covered; compose stop.
func TestTheLeaseModelOnACoveredStartOfSeveralThatStops(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{2, 0, 1},  // w1: a guessed hit of p1 starts c1 and c2
		op{10, 0, 0}, // tick: they bind the guess
		op{3, 0, 0},  // compose stop p1
		op{5, 0, 0},  // w1: docker start c1 c2: the guess covers them
		op{3, 0, 0},  // compose stop p1
	)
	guess := m.b.List()[0].ID
	for _, x := range m.of("p1", "w1") {
		if x.boundBy != guess {
			t.Errorf("%s boundBy = %q, want the guess's %s: no start of several's lease let it go", x.id, x.boundBy, guess)
		}
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAStartOfSeveralRestarted is the #111 let-go row with a
// lease: a start of several's container that crashed stays its lease's
// when a restart policy brings it back, no lease checked since its death
// being there to take it (lease.go deaths.takenBy). A guessed hit of p1
// binds c1 and c2 and ends; compose stop; docker start c1 c2; c1 crashes and
// is restarted.
func TestTheLeaseModelOnAStartOfSeveralRestarted(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{2, 0, 1},  // w1: a guessed hit of p1 starts c1 and c2
		op{9, 0, 0},  // wait 1m: they bind the guess and use its cost
		op{3, 0, 0},  // compose stop p1
		op{5, 0, 0},  // w1: docker start c1 c2
		op{10, 0, 0}, // tick
	)
	ls := m.b.List()
	if len(ls) != 1 {
		t.Fatalf("leases %+v, want only the start's", ls)
	}
	start := ls[0].ID
	m.runOK(
		op{6, 0, 0},  // c1 crashes
		op{17, 0, 0}, // the restart policy starts c1
		op{10, 0, 0}, // tick
	)
	if c1 := m.conts[0]; c1.boundBy != start || !m.bound(c1) {
		t.Errorf("%s boundBy = %q (bound %v), want the start's %s: no lease was checked since it died", c1.id, c1.boundBy, m.bound(c1), start)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAnUpsContainerBackToAGuess plays #168's (c): any
// container back after one reading it was missing from, and bound to no
// lease, is held by the lease the reading binds it to (lease.go Observe), a
// guess too, whoever started it. An up of p1 starts c1 and c2 and ends; a
// guessed hit of p1 finds them running; c1 is missing from a reading.
func TestTheLeaseModelOnAnUpsContainerBackToAGuess(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{0, 0, 0}, // w1: compose up p1 starts c1 and c2
		op{9, 0, 0}, // wait 1m: its lease ends
		op{2, 0, 1}, // w1: a guessed hit of p1 starts nothing
	)
	ls := m.b.List()
	if len(ls) != 1 || m.guesses[ls[0].ID] == nil {
		t.Fatalf("leases %+v, want only the guess", ls)
	}
	guess := ls[0].ID
	m.runOK(
		op{16, 0, 0}, // c1 missing from a reading
		op{10, 0, 0}, // tick: c1 is back
	)
	if c1 := m.conts[0]; c1.heldBy != guess || !m.confirmed(guess) {
		t.Errorf("%s held by %q, the guess %s confirmed %v: want held by the guess, as in the book", c1.id, c1.heldBy, guess, m.confirmed(guess))
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAShownHitMissingItsFirstReading plays #168's F4: a
// guessed hit's container that the last reading showed is not new to the
// next (lease.go Observe prev), and when it misses that one, the reading
// after holds it for the guess, as for any container back (backReading).
// An up of p1 starts c1 and c2 and ends; compose stop; a guessed hit of p1
// starts them again; c1 misses its first reading.
func TestTheLeaseModelOnAShownHitMissingItsFirstReading(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{0, 0, 0}, // w1: compose up p1 starts c1 and c2
		op{9, 0, 0}, // wait 1m: its lease ends
		op{3, 0, 0}, // compose stop p1
		op{2, 0, 1}, // w1: a guessed hit of p1 starts c1 and c2
	)
	ls := m.b.List()
	if len(ls) != 1 || m.guesses[ls[0].ID] == nil {
		t.Fatalf("leases %+v, want only the guess", ls)
	}
	guess := ls[0].ID
	m.runOK(
		op{16, 0, 0}, // c1 missing from its first reading
		op{10, 0, 0}, // tick: c1 is back
	)
	if c1 := m.conts[0]; c1.heldBy != guess || !m.confirmed(guess) {
		t.Errorf("%s held by %q, the guess %s confirmed %v: want held by the guess, as in the book", c1.id, c1.heldBy, guess, m.confirmed(guess))
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAHitRestartingACrash plays #168's restart row: a
// guessed hit that starts a container crashed since the last reading, which
// showed it, while an up of its project checked before the crash is open:
// the start event binds it to no lease, an up of the stack restarting
// it without a stop being no call of that up's (lease.go ContainerEvent), and
// the next reading finds nothing new. An up of app in w2 starts c1 and c2; an
// idle up holds them; c1 crashes; a guessed hit of app starts it again.
func TestTheLeaseModelOnAHitRestartingACrash(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{0, 3, 0},  // w2: compose up app starts c1 and c2
		op{10, 0, 0}, // tick
		op{10, 0, 0}, // tick: they use its cost, it ends
		op{0, 3, 0},  // w2: an idle compose up app holds c1 and c2
		op{6, 0, 0},  // c1 crashes
		op{2, 3, 1},  // w2: a guessed hit of app starts c1
	)
	c1 := m.conts[0]
	if m.bound(c1) {
		t.Errorf("%s bound by %q, want by nothing: the up was checked before it crashed", c1.id, c1.boundBy)
	}
	m.runOK(op{10, 0, 0}) // tick
	if m.bound(c1) {
		t.Errorf("after the reading, %s bound by %q, want by nothing: it is not new", c1.id, c1.boundBy)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAnotherWorktreesUpAtAReading plays a row of #168's
// differential: with no lease of its own worktree keying it, a container
// of app back after a missing reading binds another worktree's open up of
// app, the dir only preferring its own (lease.go keyed), and a later up
// that takes that one over holds it (lease.go Check). A guessed hit of app
// in w2 binds c1 and c2 and ends; c1 is missing from a reading; w1's up of
// app starts c3 and c4; c1 is back; w1's next up of app.
func TestTheLeaseModelOnAnotherWorktreesUpAtAReading(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{2, 3, 1},  // w2: a guessed hit of app starts c1 and c2
		op{9, 0, 0},  // wait 1m: they bind it and use its cost
		op{16, 0, 0}, // c1 missing from a reading
		op{0, 2, 0},  // w1: compose up app starts c3 and c4
	)
	ls := m.b.List()
	if len(ls) != 1 {
		t.Fatalf("leases %+v, want only w1's up", ls)
	}
	up := ls[0].ID
	m.runOK(op{10, 0, 0}) // tick: c1 is back
	c1 := m.conts[0]
	if c1.heldBy != up {
		t.Errorf("%s held by %q, want by w1's up %s, as in the book", c1.id, c1.heldBy, up)
	}
	m.runOK(op{0, 2, 0}) // w1: compose up app takes the up over
	ls = m.b.List()
	if again := ls[len(ls)-1].ID; c1.heldBy != again || !m.held(c1) {
		t.Errorf("%s held by %q (held %v), want by w1's later up %s, which took %s over", c1.id, c1.heldBy, m.held(c1), again, up)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnATiedHitThenAFlicker plays #168's F5: a guessed hit's
// start event ties when only another worktree's up of its project is open
// (the guess, of its own dir, may be its: lease.go tied), and binds
// nothing; a first reading with no attribution then binds it to that up, a
// guess keying only what a reading attributes (lease.go keyed). w1's up of
// app starts c1 and c2; w2's guessed hit of app starts c3 and c4; a
// flicker.
func TestTheLeaseModelOnATiedHitThenAFlicker(t *testing.T) {
	m := newModel(t)
	m.runOK(op{0, 2, 0}) // w1: compose up app starts c1 and c2
	up := m.b.List()[0].ID
	m.runOK(op{2, 3, 1}) // w2: a guessed hit of app starts c3 and c4
	for _, x := range m.of("app", "w2") {
		if m.bound(x) {
			t.Errorf("at its start, %s bound by %q, want by nothing: its event ties", x.id, x.boundBy)
		}
	}
	m.runOK(op{10, 1, 0}) // tick: a flicker ((1 + 7) % 8)
	if !m.flick {
		t.Fatal("the reading had attribution, want a flicker")
	}
	for _, x := range m.of("app", "w2") {
		if x.boundBy != up {
			t.Errorf("after the flicker, %s boundBy = %q, want w1's up %s, as in the book", x.id, x.boundBy, up)
		}
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAStartOfSeveralsContainerStartedByAnUp is the #111
// let-go row where a lease takes the container: an up checked after it
// died starts it again, and its start event binds it to that up (lease.go
// ContainerEvent, deaths.takenBy). A guessed hit of app in w2 binds c1 and
// c2 and ends; compose stop; docker start c1 c2; c1 crashes; an up of app
// starts it again.
func TestTheLeaseModelOnAStartOfSeveralsContainerStartedByAnUp(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{2, 3, 1},  // w2: a guessed hit of app starts c1 and c2
		op{9, 0, 0},  // wait 1m: they bind it and use its cost
		op{3, 3, 0},  // compose stop app
		op{5, 0, 0},  // w2: docker start c1 c2
		op{10, 0, 0}, // tick
		op{6, 0, 0},  // c1 crashes
		op{0, 3, 0},  // w2: compose up app starts c1
	)
	ls := m.b.List()
	up := ls[len(ls)-1].ID
	if c1 := m.conts[0]; c1.boundBy != up {
		t.Errorf("%s boundBy = %q, want the up's %s: it was checked since c1 died", c1.id, c1.boundBy, up)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnADressedRun plays a row of #168's differential: a
// docker run dressed as a compose container loses its own lease (as
// declined), so no lease binds it, and a later docker start of it with a
// guess's container is a start of several (lease.go Check covered).
func TestTheLeaseModelOnADressedRun(t *testing.T) {
	m := newModel(t)
	m.runOK(op{13, 0, 0}, op{10, 0, 0}) // w2: a dressed docker run starts c1; tick
	c1 := m.conts[0]
	if got := lease.Bindings(m.b, c1.id); m.bound(c1) || len(got) > 0 {
		t.Errorf("%s bound by %q in the model, %q in the book: want by nothing in both", c1.id, c1.boundBy, got)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnADeadMarkAfterItsLease plays a row of #168's
// differential: a dead mark is the start of several's lease's (lease.go
// entry.dead), and ends with it: a later start's lease that binds the
// container keeps it at the reading. An up of p1 starts c1 and c2 and ends;
// compose stop; docker start c1 c2; c1 crashes; a guessed hit of p1 starts
// it; compose stop ends the start's lease; docker start c1 c2 again.
func TestTheLeaseModelOnADeadMarkAfterItsLease(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{0, 0, 0},  // w1: compose up p1 starts c1 and c2
		op{9, 0, 0},  // wait 1m: its lease ends
		op{3, 0, 0},  // compose stop p1
		op{5, 0, 0},  // w1: docker start c1 c2
		op{10, 0, 0}, // tick
		op{6, 0, 0},  // c1 crashes
		op{2, 0, 1},  // w1: a guessed hit of p1 starts c1
		op{3, 0, 0},  // compose stop p1: the start's lease ends
		op{5, 0, 0},  // w1: docker start c1 c2
	)
	ls := m.b.List()
	start := ls[len(ls)-1].ID
	m.runOK(op{10, 0, 0}) // tick
	if c1 := m.conts[0]; c1.boundBy != start {
		t.Errorf("%s boundBy = %q, want the later start's %s", c1.id, c1.boundBy, start)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnACrashOfNoKnownWorktree plays a row of #168's
// differential: an up's start of a crashed container whose worktree no
// reading named, nor a project's lease it bound, binds no lease at its event
// (lease.go bindsAfterCrash, #89); the next reading, which attributes it,
// binds it. A guessed hit of app in w2 starts c1 and c2, stopped before a
// reading; docker start c1; c1 crashes; an up of app starts it again.
func TestTheLeaseModelOnACrashOfNoKnownWorktree(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{2, 3, 1}, // w2: a guessed hit of app starts c1 and c2
		op{3, 3, 0}, // compose stop app
		op{5, 1, 0}, // w2: docker start c1
		op{6, 0, 0}, // c1 crashes: the start's lease ends
		op{0, 3, 0}, // w2: compose up app starts c1 and c2
	)
	ls := m.b.List()
	up := ls[len(ls)-1].ID
	c1 := m.conts[0]
	if m.bound(c1) {
		t.Errorf("at its start, %s bound by %q, want by nothing: no reading named its worktree", c1.id, c1.boundBy)
	}
	m.runOK(op{10, 0, 0}) // tick
	if c1.boundBy != up {
		t.Errorf("after the reading, %s boundBy = %q, want the up's %s", c1.id, c1.boundBy, up)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnACrashAReadingForgot is TestTheLeaseModelOnACrashOfNoKnownWorktree
// with a reading between the crash and the start: the reading drops the
// crash's event, and no reading ever showed the container, so the book
// keeps no verdict of it, and its start is no restart after a crash
// (lease.go ContainerEvent crashed): the up binds it at its event. A guessed
// hit of p2 starts c1 and c2; c1 crashes; a reading; an up of p2 starts it.
func TestTheLeaseModelOnACrashAReadingForgot(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{2, 1, 1},  // w2: a guessed hit of p2 starts c1 and c2
		op{6, 0, 0},  // c1 crashes
		op{10, 0, 0}, // tick
		op{1, 1, 0},  // w2: compose up p2 starts c1
	)
	ls := m.b.List()
	up := ls[len(ls)-1].ID
	if c1 := m.conts[0]; c1.boundBy != up {
		t.Errorf("%s boundBy = %q, want the up's %s: the book forgot the crash", c1.id, c1.boundBy, up)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAWorktreeForgotten plays a row of #168's
// differential: the book forgets a container's worktree with its verdict,
// absent for the timeout (lease.go Observe), and a docker start's event
// judges it afresh with none, so an up's start of it after it crashes binds
// no lease at its event (lease.go bindsAfterCrash). A guessed hit of app in
// w2 binds c1 and c2 at a reading; compose stop; 2m pass; docker start c1;
// c1 crashes; an up of app starts it.
func TestTheLeaseModelOnAWorktreeForgotten(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{2, 3, 1},  // w2: a guessed hit of app starts c1 and c2
		op{10, 0, 0}, // tick: they bind it
		op{3, 3, 0},  // compose stop app
		op{9, 0, 0},
		op{9, 0, 0}, // 2m: their verdicts go
		op{5, 1, 0}, // w2: docker start c1
		op{6, 0, 0}, // c1 crashes
		op{0, 3, 0}, // w2: compose up app starts c1 and c2
	)
	if c1 := m.conts[0]; m.bound(c1) {
		t.Errorf("%s bound by %q, want by nothing: the book forgot its worktree", c1.id, c1.boundBy)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnACrashedStartOfSeveralsContainer plays a row of
// #168's differential: a start of several's container whose worktree the
// book does not know, crashed, stays that start's when an up starts it
// again, the event binding no project's lease to it (lease.go
// bindsAfterCrash). A guessed hit of app in w1 starts c1 and c2; c1
// crashes; docker start c1 c2; c1 crashes; an up of app starts it.
func TestTheLeaseModelOnACrashedStartOfSeveralsContainer(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{2, 2, 1}, // w1: a guessed hit of app starts c1 and c2
		op{3, 2, 0}, // compose stop app
		op{5, 0, 0}, // w1: docker start c1 c2
	)
	start := m.b.List()[len(m.b.List())-1].ID
	m.runOK(
		op{6, 0, 0}, // c1 crashes
		op{0, 2, 0}, // w1: compose up app starts c1
	)
	if c1 := m.conts[0]; c1.boundBy != start {
		t.Errorf("%s boundBy = %q, want the start's %s: no reading named its worktree", c1.id, c1.boundBy, start)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAWorktreeFromAFlickeredBinding plays a row of #168's
// differential: a project's lease that binds a container tells the book
// its worktree (lease.go boundBy), at a reading with no attribution too, so
// a later up's start of it after a crash binds at its event. A guessed hit
// of app in w2 starts c1 and c2; an idle up of app; a flickered reading
// binds them to it; c1 crashes; an up of app starts it.
func TestTheLeaseModelOnAWorktreeFromAFlickeredBinding(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{2, 3, 1},  // w2: a guessed hit of app starts c1 and c2
		op{0, 3, 0},  // w2: an idle compose up app
		op{10, 1, 0}, // tick: a flicker ((1 + 7) % 8)
	)
	if !m.flick {
		t.Fatal("the reading had attribution, want a flicker")
	}
	m.runOK(
		op{6, 0, 0}, // c1 crashes
		op{0, 3, 0}, // w2: compose up app starts c1
	)
	ls := m.b.List()
	up := ls[len(ls)-1].ID
	if c1 := m.conts[0]; c1.boundBy != up {
		t.Errorf("%s boundBy = %q, want the up's %s: the book knows its worktree", c1.id, c1.boundBy, up)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnAStackItsLeasePlaced plays the model's readings after
// #74: the book re-derives the latest reading's attribution with the
// attribution matcher (lease.go Observe), so the model's must be what the
// matcher finds (model.snapshot), or a compose up finds no stack running in
// its worktree. An up of p1 starts c1 and c2, and its lease ends; an idle up
// of p1 holds them.
func TestTheLeaseModelOnAStackItsLeasePlaced(t *testing.T) {
	m := newModel(t)
	m.runOK(
		op{0, 0, 0}, // w1: compose up p1 starts c1 and c2
		op{9, 0, 0}, // wait 1m: its lease ends
		op{0, 0, 0}, // w1: an idle compose up p1 holds c1 and c2
	)
	up := m.b.List()[0].ID
	if got, want := lease.Bound(m.b, up), []string{"container:c1", "container:c2"}; !slices.Equal(got, want) {
		t.Errorf("the idle up %s binds %q in the book, want %q: it holds the stack", up, got, want)
	}
	for _, x := range m.of("p1", "w1") {
		if x.heldBy != up {
			t.Errorf("%s held by %q in the model, want by the idle up %s", x.id, x.heldBy, up)
		}
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnARestartAnUpTakes plays #207's event row: a restart
// policy's start of a crashed container binds at its event as a call's
// start would (lease.go ContainerEvent): an up of its project in its
// worktree, checked before the crash, binds it once a reading dropped it,
// the book knowing its worktree (bindsAfterCrash). A guessed hit of p1
// binds c1 and c2 and ends; an idle up of p1 holds them; c1 crashes and
// c2 stops; a reading; the restart policy starts c1.
func TestTheLeaseModelOnARestartAnUpTakes(t *testing.T) {
	m := newModel(t)
	m.diff = true
	m.runOK(
		op{2, 0, 1},  // w1: a guessed hit of p1 starts c1 and c2
		op{9, 0, 0},  // wait 1m: they bind it and use its cost
		op{0, 0, 0},  // w1: an idle compose up p1 holds c1 and c2
		op{6, 0, 0},  // c1 crashes
		op{3, 0, 0},  // compose stop p1: c2 stops
		op{10, 0, 0}, // tick: the reading drops c1
	)
	up := m.upOf("p1", "w1")
	if up == "" {
		t.Fatalf("leases %+v, want the idle up open", m.b.List())
	}
	m.runOK(op{17, 0, 0}) // the restart policy starts c1
	if c1 := m.conts[0]; c1.boundBy != up {
		t.Errorf("%s boundBy = %q, want the up %s, as in the book", c1.id, c1.boundBy, up)
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}

// TestTheLeaseModelOnARestartAGuessTakes plays #207's reading row: a
// restart policy's start of a crashed container that no lease binds at its
// event is new to the next reading, which binds it as a call's start
// (lease.go Observe fresh, binder): a guess of its project in its worktree
// keys no event, but the reading's attribution matches it. An up of p1
// starts c1 and c2 and ends; a guessed hit of p1 finds them running; c1
// crashes; a reading; the restart policy starts c1; a reading.
func TestTheLeaseModelOnARestartAGuessTakes(t *testing.T) {
	m := newModel(t)
	m.diff = true
	m.runOK(
		op{0, 0, 0},  // w1: compose up p1 starts c1 and c2
		op{9, 0, 0},  // wait 1m: its lease ends
		op{2, 0, 1},  // w1: a guessed hit of p1 starts nothing
		op{6, 0, 0},  // c1 crashes
		op{10, 0, 0}, // tick: the reading drops c1
		op{17, 0, 0}, // the restart policy starts c1
	)
	ls := m.b.List()
	if len(ls) != 1 || m.guesses[ls[0].ID] == nil {
		t.Fatalf("leases %+v, want only the guess", ls)
	}
	guess := ls[0].ID
	c1 := m.conts[0]
	if m.bound(c1) {
		t.Errorf("%s bound by %q at its event, want by nothing: the guess keys no event", c1.id, c1.boundBy)
	}
	m.runOK(op{10, 0, 0}) // tick: c1 is new to it
	if c1.boundBy != guess || !m.confirmed(guess) {
		t.Errorf("%s boundBy = %q, the guess %s confirmed %v: want bound by the guess, as in the book", c1.id, c1.boundBy, guess, m.confirmed(guess))
	}
	if t.Failed() {
		t.Log(strings.Join(m.trace, "\n"))
	}
}
