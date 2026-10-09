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
// needing more than is reserved, and a container started past the shim
// not flagged.

const target = gib // what each container grows to

type mcont struct {
	id, project, service, wt string
	labels                   map[string]string
	cur                      uint64
	running, gone            bool
	firstGated, raw          bool
	startedBy                string    // the lease of the call that last started it, "" if none
	startedAt                time.Time // when its call was checked
}

type op struct{ kind, a, b int }

type model struct {
	t      *testing.T
	cur    op
	b      *lease.Book
	c      *clock
	log    interface{ String() string }
	conts  []*mcont
	n      int
	flick  bool              // attribution lost this tick
	leases map[string]string // lease → the call that took it
	starts map[string]int    // lease → containers its call started
	trace  []string
	last   time.Time // the last checked call
	ticks  int
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
		if !x.running {
			continue
		}
		wt := x.wt
		if m.flick {
			wt = ""
		}
		addContainer(s, protocol.Container{ID: x.id, Name: x.id, MemoryBytes: x.cur, Labels: x.labels}, wt)
	}
	s.CollectedAt = m.c.t
	s.Budget.HeadroomBytes = i64(int64(1024 * gib)) // decisions are not under test
	return read(s, m.c.t)
}

func (m *model) event(action string, x *mcont) { m.b.ContainerEvent(action, x.id, x.id, x.labels) }

func (m *model) start(x *mcont, by string) {
	x.running, x.cur, x.startedBy, x.startedAt, x.raw = true, 0, by, m.c.t, by == ""
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

func (m *model) of(project string) []*mcont {
	var out []*mcont
	for _, x := range m.conts {
		if x.project == project && !x.gone {
			out = append(out, x)
		}
	}
	return out
}

func (m *model) check(r policy.Request) protocol.Decision {
	d := m.b.Check(r, m.snapshot(), cfg)
	if !d.Allow {
		m.fail("%s denied: %+v", r.Command, d)
	}
	m.last = m.c.t
	if d.LeaseID != "" {
		m.leases[d.LeaseID] = r.Command
	}
	return d
}

var projects = map[string]string{"p1": "w1", "p2": "w2"}

func (m *model) composeUp(project string) {
	wt := projects[project]
	have := map[string]*mcont{}
	for _, x := range m.of(project) {
		have[x.service] = x
	}
	idle := true
	for _, sv := range []string{"a", "b"} {
		if x := have[sv]; x == nil || !x.running {
			idle = false
		}
	}
	d := m.check(policy.Request{Worktree: wt, Kind: "compose", Op: "up", Command: "docker compose -p " + project + " up -d",
		CostBytes: 2 * target, Target: project, OnEngine: true, Idle: idle})
	m.step("%s: compose up %s (idle %v) → %s", wt, project, idle, d.LeaseID)
	for _, sv := range []string{"a", "b"} {
		x := have[sv]
		if x == nil {
			x = m.newCont(project, sv, wt, map[string]string{protocol.ComposeProjectLabel: project,
				protocol.ComposeServiceLabel: sv, protocol.ComposeConfigHashLabel: "h"})
			x.firstGated = true
		}
		if !x.running {
			m.start(x, d.LeaseID)
			m.step("  started %s", x.id)
		}
	}
}

func (m *model) stop(x *mcont, crash bool) {
	if !crash {
		m.event("stop", x)
	}
	m.event("die", x)
	x.running = false
}

func (m *model) dockerStart(x *mcont) {
	wt := projects[x.project]
	if wt == "" {
		wt = x.wt
	}
	if wt == "" {
		wt = "w1"
	}
	d := m.check(policy.Request{Worktree: wt, Kind: "container", Command: "docker start " + x.id, Target: x.id,
		ContainerID: x.id, CostBytes: target, OnEngine: true, TakesOver: protocol.LeaseOf(x.labels)})
	m.step("%s: docker start %s → %q (%s)", wt, x.id, d.LeaseID, d.Message)
	by := d.LeaseID
	if by == "" {
		by = "covered"
	}
	m.start(x, by)
}

func (m *model) dockerRun(wt string) {
	d := m.check(policy.Request{Worktree: wt, Kind: "container", Command: "docker run img", CostBytes: target, Labelled: true})
	attr := ""
	if m.cur.b%2 == 0 {
		attr = wt
	}
	x := m.newCont("", "", attr, map[string]string{protocol.LeaseLabel: d.LeaseID})
	x.firstGated = true
	m.step("%s: docker run → %s as %s", wt, d.LeaseID, x.id)
	m.start(x, d.LeaseID)
}

// forgeCompose is a repo in w2 whose compose service's labels: name another
// worktree's open lease. Its own up is checked; the label must count for
// nothing.
func (m *model) forgeCompose() {
	if len(m.of("evil")) > 0 {
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
		if x.firstGated && ungated[x.id] {
			m.fail("false ungated: %s was started by a checked call", x.id)
		}
		if x.raw && !ungated[x.id] && m.c.t.Sub(x.startedAt) >= 5*time.Second {
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

func (m *model) run(ops []op) {
	for _, o := range ops {
		m.cur = o
		m.c.t = m.c.t.Add(10 * time.Millisecond) // calls and events take time
		project := []string{"p1", "p2"}[o.a%2]
		switch o.kind {
		case 0, 1, 2:
			m.composeUp(project)
		case 3:
			for _, x := range m.of(project) {
				if x.running {
					m.stop(x, false)
					m.step("compose stop %s", x.id)
				}
			}
		case 4:
			for _, x := range m.of(project) {
				if x.running {
					m.stop(x, false)
				}
				x.gone = true
				m.step("compose down %s", x.id)
			}
		case 5:
			if x := m.pick(func(x *mcont) bool { return !x.running && !x.gone }); x != nil {
				m.dockerStart(x)
			}
		case 6:
			if x := m.pick(func(x *mcont) bool { return x.running }); x != nil {
				m.stop(x, o.b%2 == 0)
				m.step("stop/crash(%v) %s", o.b%2 == 0, x.id)
			}
		case 7:
			m.dockerRun([]string{"w1", "w2"}[o.a%2])
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
	m := &model{t: t, b: b, c: c, log: log, leases: map[string]string{}, starts: map[string]int{}}
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
// and shows each failure shrunk to the ops it needs.
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
			ops[i] = op{rng.IntN(14), rng.IntN(1000), rng.IntN(1000)}
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
