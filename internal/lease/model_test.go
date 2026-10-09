package lease_test

import (
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

type mcont struct {
	id, project, service, wt string
	labels                   map[string]string
	cur                      uint64
	running, gone            bool
	firstGated, raw          bool
	startedBy                string    // the lease of the call that last started it, "" if none
	startedAt                time.Time // when its call was checked
	crashed                  time.Time // when it last crashed, zero once it stopped or started
	crashTick                int       // the readings before it crashed
	crashHeld                bool      // it was held when it crashed
	heldBy                   string    // the up that found it running, "" once it stopped or started
	missing                  bool      // left out of this reading
	multi                    bool      // its call started others too (docker start a b)
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
	trace    []string
	last     time.Time // the last allowed call
	ticks    int
	headroom int64        // what the readings say is free
	open     map[int]bool // the open bugs whose cases run
}

// openBugs are the cases that expose a bug still open, by its issue. They
// run only when HEADROOM_MODEL_OPEN is 1 (all) or lists the issue
// (HEADROOM_MODEL_OPEN=87,89). Fixing the issue removes its entry.
var openBugs = map[int]string{
	87:  "a held container missing from one reading (or restarted after one) comes back new and binds the up's lease as counted",
	89:  "a restart by policy binds another worktree's same-named compose lease",
	109: "a project name used in w1 and w2: an event, or a reading without attribution, binds neither or the wrong one (related to #89)",
	110: "a container not held, missing from one reading (or restarted after one), ends its lease or binds an up's as counted (#87's cause)",
	111: "a crashed container stays bound to a two-container start's lease: a compose up restarting it binds nothing",
	112: "an up taking over its stack's lease while that lease's services warm holds the larger estimate, not both",
}

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
	x.running, x.cur, x.startedBy, x.startedAt, x.raw, x.crashed, x.multi, x.heldBy = true, 0, by, m.c.t, by == "", time.Time{}, false, ""
	if by != "" {
		m.starts[by]++
	}
	m.event("start", x)
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

// check asks the book about r. A denied call starts nothing.
func (m *model) check(r policy.Request) protocol.Decision {
	d := m.b.Check(r, m.snapshot(), cfg)
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

// stackAt is the stack op argument a names.
func (m *model) stackAt(a int) stack {
	if !m.on(109) {
		return stacks[a%2]
	}
	return stacks[a%len(stacks)]
}

func (m *model) composeUp(st stack) bool {
	project, wt := st.project, st.wt
	have := map[string]*mcont{}
	for _, x := range m.of(project, wt) {
		have[x.service] = x
	}
	var cost uint64 // what the services it starts will use
	for _, sv := range []string{"a", "b"} {
		if x := have[sv]; x == nil || !x.running {
			if x != nil && x.multi && !m.on(111) {
				return true // left out (#111)
			}
			cost += target
		}
	}
	idle := cost == 0
	if !idle && !m.on(112) && slices.ContainsFunc(m.of(project, wt), func(x *mcont) bool {
		return x.running && x.cur < target && strings.HasPrefix(m.leases[x.startedBy], "docker compose")
	}) {
		return true // left out: it takes over a lease whose services warm (#112)
	}
	d := m.check(policy.Request{Worktree: wt, Kind: "compose", Op: "up", Command: "docker compose -p " + project + " up -d",
		CostBytes: cost, Target: project, OnEngine: true, Idle: idle})
	if !d.Allow {
		return false
	}
	m.step("%s: compose up %s (idle %v) → %s", wt, project, idle, d.LeaseID)
	for _, sv := range []string{"a", "b"} {
		x := have[sv]
		if x != nil && x.running {
			x.heldBy = d.LeaseID // it found it running
		}
		if x == nil {
			x = m.newCont(project, sv, wt, map[string]string{protocol.ComposeProjectLabel: project,
				protocol.ComposeServiceLabel: sv, protocol.ComposeConfigHashLabel: "h", protocol.ComposeWorkingDirLabel: "/src/" + wt})
			x.firstGated = true
		}
		if !x.running {
			m.start(x, d.LeaseID)
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
	if crash {
		x.crashed, x.crashTick, x.crashHeld = m.c.t, m.ticks, m.held(x)
	} else {
		x.crashed = time.Time{}
	}
	x.running, x.heldBy = false, ""
}

// held reports whether x is held: an up found it running, and that up's
// lease is open.
func (m *model) held(x *mcont) bool {
	return x.heldBy != "" && slices.ContainsFunc(m.b.List(), func(l protocol.Lease) bool { return l.ID == x.heldBy })
}

// restart is Docker's restart policy bringing a crashed container back, as
// a start event and no call. It was checked if its first start was, and its
// call's reservation is long spent.
func (m *model) restart(x *mcont) {
	x.running, x.cur, x.startedBy, x.startedAt, x.crashed = true, 0, "", m.c.t, time.Time{}
	m.event("start", x)
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
		m.start(x, by)
		x.multi = len(xs) > 1
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
}

func (m *model) rawRun() {
	x := m.newCont("", "", "", nil)
	m.step("raw docker run as %s", x.id)
	m.start(x, "")
}

var neverRE = regexp.MustCompile(`never appeared.*lease=(\S+)`)

func (m *model) tick() {
	m.c.t = m.c.t.Add(5 * time.Second)
	for _, x := range m.conts {
		if x.running {
			x.cur = min(x.cur+target/2, target)
		}
	}
	m.ticks++
	m.flick = (m.cur.a+m.ticks*7)%8 == 0
	m.step("tick %s (flicker %v)", m.c.t.Format("15:04:05"), m.flick)
	m.b.Observe(m.snapshot())
	m.invariants()
	m.heldForNothing()
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
		return func() bool { return m.composeUp(st) }
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
			for _, x := range m.of(st.project, st.wt) {
				if x.running {
					m.stop(x, false)
					m.step("compose stop %s", x.id)
				}
			}
		case 4:
			for _, x := range m.of(st.project, st.wt) {
				if x.running {
					m.stop(x, false)
				}
				x.gone = true
				m.step("compose down %s", x.id)
			}
		case 6:
			if x := m.pick(func(x *mcont) bool { return x.running }); x != nil {
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
			// Memory runs short, or frees up again: the next reading says.
			m.headroom = []int64{int64(1024 * gib), 0, int64(gib), int64(2 * gib)}[o.b%4]
			m.step("headroom %d MiB", m.headroom>>20)
			m.tick()
		case 15:
			// A call with BUDGET_WAIT=1: denied, it asks again after each
			// reading, until it is allowed or gives up.
			call := m.call([]int{0, 5, 7}[o.b%3], o)
			if call == nil {
				break
			}
			m.step("BUDGET_WAIT")
			for i := 0; !call() && i < 3; i++ {
				m.tick()
			}
		case 16:
			// A running container left out of one reading: its stats failed,
			// or a restart backoff spans it.
			x := m.pick(func(x *mcont) bool { return x.running })
			if x == nil {
				break
			}
			issue := 110
			if m.held(x) {
				issue = 87
			}
			if !m.on(issue) {
				m.tick()
			} else {
				x.missing = true
				m.step("%s missing from the reading", x.id)
				m.tick()
				x.missing = false
			}
		case 17:
			// Docker's restart policy restarts a crashed container, with a
			// backoff of at most a minute. One that spans a reading is #87's
			// if it was held, else #110's.
			spans := func(x *mcont) bool {
				if x.crashHeld {
					return !m.on(87)
				}
				return !m.on(110)
			}
			if !m.on(89) {
				m.tick()
			} else if x := m.pick(func(x *mcont) bool {
				return !x.crashed.IsZero() && m.c.t.Sub(x.crashed) <= time.Minute && (x.crashTick == m.ticks || !spans(x))
			}); x != nil {
				m.restart(x)
				m.step("restart policy started %s", x.id)
			}
		default:
			m.tick()
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
func play(t *testing.T, ops []op) (f *failure) {
	b, c, log := book(t)
	m := &model{t: t, b: b, c: c, log: log, leases: map[string]string{}, starts: map[string]int{}, headroom: int64(1024 * gib),
		open: openIssues()}
	defer func() {
		if r := recover(); r != nil {
			ff, ok := r.(failure)
			if !ok {
				panic(r)
			}
			f = &ff
		}
	}()
	b.Observe(m.snapshot())
	m.run(ops)
	return nil
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
	kinds := map[string]int{}
	for seed := range seeds {
		rng := rand.New(rand.NewPCG(uint64(seed), 1))
		ops := make([]op, 80)
		for i := range ops {
			ops[i] = op{rng.IntN(18), rng.IntN(1000), rng.IntN(1000)}
		}
		f := play(t, ops)
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
}

// TestTheLeaseModelOn87 plays #87's case, which random runs do not reach:
// a service warms in two readings, and a missing reading and the next take
// two. An up holds c1 and c2, which run; c2 stops; c1 is missing from one
// reading while that up's lease holds it; an up of the stack starts c2;
// c1 comes back.
func TestTheLeaseModelOn87(t *testing.T) {
	if !(&model{open: openIssues()}).on(87) {
		t.Skip("open: #87")
	}
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
