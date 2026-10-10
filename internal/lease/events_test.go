package lease_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/lease"
	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// docker run --rm alpine true: its container starts and exits between two
// readings. Docker's events bind it, and its exit ends the lease at once,
// with no "never appeared" warning (#67).
func TestAContainerOnlyEventsSawEndsItsLease(t *testing.T) {
	b, c, log := book(t)
	b.Observe(snap())
	d := b.Check(req("w1", 2*gib), snap(), cfg)
	b.ContainerEvent("start", "Q", "quick", map[string]string{protocol.LeaseLabel: d.LeaseID})
	b.ContainerEvent("die", "Q", "quick", map[string]string{protocol.LeaseLabel: d.LeaseID})
	if l := b.List(); len(l) != 0 {
		t.Fatalf("leases = %+v, want none", l)
	}
	c.t = c.t.Add(3 * 60e9)
	b.Observe(snap())
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("log: %s", log)
	}
}

// docker start a b: the lease waits for both, through events too.
func TestEventsEndAStartOfSeveralOnlyWhenEachExited(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start a", Target: "a", ContainerID: "A",
		Others: []policy.Start{{ID: "B"}}}, snap(), cfg)
	b.ContainerEvent("start", "A", "a", nil)
	b.ContainerEvent("die", "A", "a", nil)
	if len(b.List()) != 1 {
		t.Fatal("the lease ended before b started")
	}
	b.ContainerEvent("start", "B", "b", nil)
	b.ContainerEvent("die", "B", "b", nil)
	if l := b.List(); len(l) != 0 {
		t.Fatalf("leases = %+v, want none", l)
	}
}

// A container a reading already holds (a restart policy's restart) binds a
// lease only by an exact key: a lease keyed by its name is another's.
func TestAnEventOfAKnownContainerBindsNoNamedLease(t *testing.T) {
	b, _, _ := book(t)
	s := addContainer(snap(), protocol.Container{ID: "R", Name: "db", MemoryBytes: gib / 4}, "w1")
	b.Observe(s)
	b.Observe(s)
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker run --name db x", Name: "db"}, s, cfg)
	b.ContainerEvent("die", "R", "db", nil)
	b.ContainerEvent("start", "R", "db", nil)
	b.ContainerEvent("die", "R", "db", nil) // would end the lease had it bound
	if l := b.List(); len(l) != 1 {
		t.Fatalf("leases = %+v, want the run's, unbound", l)
	}
}

// A container that appears with no check is still found ungated by the
// next reading: an event only ever binds.
func TestAnEventBindsNothingUnchecked(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.ContainerEvent("start", "U", "u", nil)
	b.Observe(addContainer(snap(), protocol.Container{ID: "U", Name: "u"}, "w1"))
	if got := ungatedKeys(b); len(got) != 1 {
		t.Fatalf("ungated = %v", got)
	}
}

// read is a snapshot whose Docker reading began at began.
func read(s *protocol.Snapshot, began time.Time) *protocol.Snapshot {
	s.Sources = map[string]protocol.SourceStatus{"docker": {At: began.Add(3 * time.Second), Took: time.Second, Began: began}}
	return s
}

// A reading begun before a container's start event may lack it: that does
// not end the lease the event bound.
func TestAReadingOlderThanTheStartKeepsTheLease(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(read(snap(), t0))
	d := b.Check(req("w1", 2*gib), snap(), cfg)
	c.t = t0.Add(2 * time.Second)
	b.ContainerEvent("start", "X", "x", map[string]string{protocol.LeaseLabel: d.LeaseID})
	b.Observe(read(snap(), t0.Add(time.Second)))
	if len(b.List()) != 1 {
		t.Fatal("a reading from before the start ended the lease")
	}
	c.t = t0.Add(10 * time.Second)
	b.Observe(read(snap(), t0.Add(5*time.Second)))
	// Missing from one later reading is not gone (#87): a second ends it.
	c.t = t0.Add(15 * time.Second)
	b.Observe(read(snap(), t0.Add(10*time.Second)))
	if l := b.List(); len(l) != 0 {
		t.Fatalf("leases = %+v: later readings without x end it", l)
	}
}

// A die after a reading began is kept: with its sibling's, it ends the lease.
func TestADieNewerThanTheReadingIsKept(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(read(snap(), t0))
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start a", Target: "a", ContainerID: "A",
		Others: []policy.Start{{ID: "B"}}}, snap(), cfg)
	both := addContainer(addContainer(snap(), protocol.Container{ID: "A", Name: "a"}, "w1"), protocol.Container{ID: "B", Name: "b"}, "w1")
	c.t = t0.Add(5 * time.Second)
	b.Observe(read(both, t0.Add(4*time.Second)))
	c.t = t0.Add(10 * time.Second)
	b.ContainerEvent("die", "A", "a", nil)
	b.Observe(read(both, t0.Add(9*time.Second))) // began before a's die
	b.ContainerEvent("die", "B", "b", nil)
	if l := b.List(); len(l) != 0 {
		t.Fatalf("leases = %+v, want none: both exited", l)
	}
}

// Two worktrees bring up one project name: an event cannot tell whose
// container it is, so the reading's attribution binds it.
func TestAnEventLeavesATieToTheReading(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	up := policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 2 * gib, Target: "app"}
	b.Check(up, snap(), cfg)
	up.Worktree = "w2"
	b.Check(up, snap(), cfg)
	b.ContainerEvent("start", "web", "web", map[string]string{protocol.ComposeProjectLabel: "app"})
	b.Observe(withComposeContainer(snap(), "web", "w2", gib/2))
	for _, l := range b.List() {
		if want := map[string]uint64{"w1": 2 * gib, "w2": 2*gib - gib/2}[l.Worktree]; l.Bytes != want {
			t.Errorf("%s holds %d MiB, want %d", l.Worktree, l.Bytes>>20, want>>20)
		}
	}
}

// A failed Docker read keeps the last good reading: it drops no event.
func TestAStaleReadingDropsNoEvent(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(read(snap(), t0))
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start a", Target: "a", ContainerID: "A",
		Others: []policy.Start{{ID: "B"}}}, snap(), cfg)
	both := func() *protocol.Snapshot {
		return addContainer(addContainer(snap(), protocol.Container{ID: "A", Name: "a"}, "w1"), protocol.Container{ID: "B", Name: "b"}, "w1")
	}
	c.t = t0.Add(5 * time.Second)
	b.Observe(read(both(), t0.Add(4*time.Second)))
	c.t = t0.Add(7 * time.Second)
	b.ContainerEvent("die", "A", "a", nil)
	stale := read(both(), t0.Add(9*time.Second)) // the last good reading, kept
	st := stale.Sources["docker"]
	st.Stale = true
	stale.Sources["docker"] = st
	c.t = t0.Add(10 * time.Second)
	b.Observe(stale)
	b.ContainerEvent("die", "B", "b", nil)
	if l := b.List(); len(l) != 0 {
		t.Fatalf("leases = %+v, want none: both exited", l)
	}
}

// docker compose stop && docker compose up -d before a reading: Docker's
// stop and start events say the containers really started again, so they
// bind the up's lease.
func TestAStopThenUpWithinATickBinds(t *testing.T) {
	b, c, log := book(t)
	lbl := map[string]string{protocol.ComposeProjectLabel: "app", "com.docker.compose.service": "db"}
	s := addContainer(snap(), protocol.Container{ID: "db", Name: "db", Labels: lbl}, "w1")
	b.Observe(s)
	b.Observe(s)
	b.ContainerEvent("stop", "db", "db", lbl)
	b.ContainerEvent("die", "db", "db", lbl)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", Target: "app", OnEngine: true}, s, cfg)
	b.ContainerEvent("start", "db", "db", lbl)
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(s)
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("log: %s", log)
	}
}

// A restart policy's restart (die, start, no stop) binds no named lease:
// see TestAnEventOfAKnownContainerBindsNoNamedLease.

// docker compose kill && docker compose up -d before a reading: a kill is a
// stop as much as docker stop is.
func TestAKillThenUpWithinATickBinds(t *testing.T) {
	b, c, log := book(t)
	lbl := map[string]string{protocol.ComposeProjectLabel: "app", "com.docker.compose.service": "db"}
	s := addContainer(snap(), protocol.Container{ID: "db", Name: "db", Labels: lbl}, "w1")
	b.Observe(s)
	b.Observe(s)
	b.ContainerEvent("kill", "db", "db", map[string]string{protocol.ComposeProjectLabel: "app", "com.docker.compose.service": "db", "signal": "9"})
	b.ContainerEvent("die", "db", "db", lbl)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", Target: "app", OnEngine: true}, s, cfg)
	b.ContainerEvent("start", "db", "db", lbl)
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(s)
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("log: %s", log)
	}
}

// docker kill -s HUP web reloads it: it still runs, so a start lease
// holding web and api does not end when api exits.
func TestAKillThatStopsNothingIsNoStop(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start web", Target: "web", ContainerID: "web",
		Others: []policy.Start{{ID: "api"}}}, snap(), cfg)
	b.ContainerEvent("start", "web", "web", nil)
	b.ContainerEvent("start", "api", "api", nil)
	b.ContainerEvent("kill", "web", "web", map[string]string{"signal": "1"})
	b.ContainerEvent("die", "api", "api", nil)
	if len(b.List()) != 1 {
		t.Fatal("the lease ended while web still runs")
	}
}

// A die newer than a reading that still shows the container is gone, not
// back: it binds no lease checked since. A compose run's one-off that
// exited must not take the next run's place from that run's own one-off.
func TestADieNewerThanTheReadingBindsNoNewRun(t *testing.T) {
	b, c, log := book(t)
	r := func(id string) func(*protocol.Snapshot) *protocol.Snapshot {
		return func(s *protocol.Snapshot) *protocol.Snapshot {
			return addContainer(s, protocol.Container{ID: id, Name: id, MemoryBytes: gib / 4,
				Labels: map[string]string{"com.docker.compose.project": "p", "com.docker.compose.oneoff": "True"}}, "w1")
		}
	}
	b.Observe(read(snap(), t0))
	b.Check(run("w1", "p"), snap(), cfg)
	c.t = t0.Add(5 * time.Second)
	b.Observe(read(r("r1")(snap()), t0.Add(4*time.Second)))
	c.t = t0.Add(9 * time.Second)
	b.Check(run("w1", "p"), snap(), cfg) // the next compose run
	c.t = t0.Add(11 * time.Second)
	b.ContainerEvent("die", "r1", "r1", map[string]string{"com.docker.compose.project": "p", "com.docker.compose.oneoff": "True"})
	b.Observe(read(r("r1")(snap()), t0.Add(10*time.Second))) // began before r1's die
	c.t = t0.Add(16 * time.Second)
	b.Observe(read(r("r2")(snap()), t0.Add(15*time.Second)))
	if strings.Contains(log.String(), "ungated") {
		t.Fatalf("the run's own one-off was warned ungated: %s", log)
	}
}

// The same for a one-off no reading showed yet: its start event bound it,
// so a reading begun before its die that lists it does not make it new.
func TestADieNewerThanTheReadingBindsNoNewRunNeverRead(t *testing.T) {
	b, c, log := book(t)
	lab := map[string]string{"com.docker.compose.project": "p", "com.docker.compose.oneoff": "True"}
	b.Observe(read(snap(), t0))
	b.Check(run("w1", "p"), snap(), cfg)
	c.t = t0.Add(2 * time.Second)
	b.ContainerEvent("start", "r1", "r1", lab)
	c.t = t0.Add(5 * time.Second)
	b.Check(run("w1", "p"), snap(), cfg) // the next compose run
	c.t = t0.Add(6 * time.Second)
	b.ContainerEvent("die", "r1", "r1", lab)
	c.t = t0.Add(7 * time.Second)
	b.Observe(read(oneoff("r1", "p", "w1")(snap()), t0.Add(3*time.Second))) // began before r1's die
	c.t = t0.Add(16 * time.Second)
	b.Observe(read(oneoff("r2", "p", "w1")(snap()), t0.Add(15*time.Second)))
	if strings.Contains(log.String(), "ungated") {
		t.Fatalf("the run's own one-off was warned ungated: %s", log)
	}
	if ls := b.List(); len(ls) != 1 || ls[0].Bytes != gib-gib/4 {
		t.Fatalf("leases = %+v, want the next run's, bound to r2", ls)
	}
}

// A SIGTERM kill between the start and the die (docker stop) keeps the
// mark the start event set: the dead one-off still binds no new run.
func TestAKillBeforeTheDieBindsNoNewRunNeverRead(t *testing.T) {
	b, c, log := book(t)
	lab := map[string]string{"com.docker.compose.project": "p", "com.docker.compose.oneoff": "True"}
	b.Observe(read(snap(), t0))
	b.Check(run("w1", "p"), snap(), cfg)
	c.t = t0.Add(2 * time.Second)
	b.ContainerEvent("start", "r1", "r1", lab)
	c.t = t0.Add(5 * time.Second)
	b.Check(run("w1", "p"), snap(), cfg) // the next compose run
	c.t = t0.Add(5500 * time.Millisecond)
	b.ContainerEvent("kill", "r1", "r1", map[string]string{"com.docker.compose.project": "p", "com.docker.compose.oneoff": "True", "signal": "15"})
	c.t = t0.Add(6 * time.Second)
	b.ContainerEvent("die", "r1", "r1", lab)
	c.t = t0.Add(7 * time.Second)
	b.Observe(read(oneoff("r1", "p", "w1")(snap()), t0.Add(3*time.Second))) // began before r1's die
	c.t = t0.Add(16 * time.Second)
	b.Observe(read(oneoff("r2", "p", "w1")(snap()), t0.Add(15*time.Second)))
	if strings.Contains(log.String(), "ungated") {
		t.Fatalf("the run's own one-off was warned ungated: %s", log)
	}
	if ls := b.List(); len(ls) != 1 || ls[0].Bytes != gib-gib/4 {
		t.Fatalf("leases = %+v, want the next run's, bound to r2", ls)
	}
}

// A start resets the mark: a one-off its start event bound in one life,
// started again as a tie (w1 and w2 each run p) and dead after a reading
// began that lists it, is the reading's to bind, as when no event bound it.
func TestATiedRestartOfAnEventBoundOneoffThatDiesDuringTheReadingBinds(t *testing.T) {
	b, c, log := book(t)
	lab := map[string]string{"com.docker.compose.project": "p", "com.docker.compose.oneoff": "True"}
	b.Observe(read(snap(), t0))
	b.Check(run("w1", "p"), snap(), cfg)
	c.t = t0.Add(1 * time.Second)
	b.ContainerEvent("start", "r1", "r1", lab)
	c.t = t0.Add(2 * time.Second)
	b.ContainerEvent("die", "r1", "r1", lab)
	if ls := b.List(); len(ls) != 0 {
		t.Fatalf("leases = %+v, want none", ls)
	}
	b.Check(run("w1", "p"), snap(), cfg)
	b.Check(run("w2", "p"), snap(), cfg)
	c.t = t0.Add(3 * time.Second)
	b.ContainerEvent("start", "r1", "r1", lab) // a tie: not bound
	c.t = t0.Add(5 * time.Second)
	b.ContainerEvent("die", "r1", "r1", lab)
	c.t = t0.Add(6 * time.Second)
	b.Observe(read(oneoff("r1", "p", "w1")(snap()), t0.Add(4*time.Second))) // began before r1's die
	c.t = t0.Add(3 * time.Minute)
	b.Observe(read(snap(), c.t.Add(-time.Second)))
	if strings.Contains(log.String(), "never appeared") && strings.Contains(log.String(), "worktree=w1") {
		t.Errorf("w1's run warned never appeared: %s", log)
	}
}

// A start that finds the container still bound to a lease is its start
// event binding it too: w1's compose run binds db and a one-off by their
// start events, and db's restart policy starts it again while the run's
// lease holds it. A compose up checked after the one-off exits starts
// nothing; db then dies after a reading began that lists it. The reading
// does not bind dead db to the up's lease, which ends as never appeared
// (#147).
func TestARestartOfABoundServiceThatDiesDuringTheReadingKeepsNeverAppeared(t *testing.T) {
	b, c, log := book(t)
	svc := map[string]string{"com.docker.compose.project": "p"}
	one := map[string]string{"com.docker.compose.project": "p", "com.docker.compose.oneoff": "True"}
	b.Observe(read(snap(), t0))
	b.Check(run("w1", "p"), snap(), cfg)
	c.t = t0.Add(1 * time.Second)
	b.ContainerEvent("start", "db", "db", svc)
	c.t = t0.Add(2 * time.Second)
	b.ContainerEvent("start", "r1", "r1", one)
	c.t = t0.Add(3 * time.Second)
	b.ContainerEvent("die", "db", "db", svc) // a crash
	c.t = t0.Add(3500 * time.Millisecond)
	b.ContainerEvent("start", "db", "db", svc) // its restart policy's: still bound
	c.t = t0.Add(4 * time.Second)
	b.ContainerEvent("die", "r1", "r1", one)
	c.t = t0.Add(5 * time.Second)
	b.Check(composeUp("w1", "p"), snap(), cfg)
	c.t = t0.Add(7 * time.Second)
	b.ContainerEvent("die", "db", "db", svc) // ends the run's lease
	c.t = t0.Add(8 * time.Second)
	b.Observe(read(service("db", "p", "w1")(snap()), t0.Add(6*time.Second))) // began before db's die
	ls := b.List()
	c.t = t0.Add(3 * time.Minute)
	b.Observe(read(snap(), c.t.Add(-time.Second)))
	if len(ls) == 1 && ls[0].Bytes != gib {
		t.Errorf("the up's lease bound dead db: %+v", ls)
	}
	if !strings.Contains(log.String(), "never appeared") {
		t.Errorf("no never appeared for an up that started nothing: %s", log)
	}
}

// A second die with no start between says the events lost a start (a
// reconnect): the life it ends is one no event bound, so the first life's
// mark does not keep docker start x from binding x1 from the reading.
func TestADieAfterALostStartBindsFromTheReading(t *testing.T) {
	b, c, log := book(t)
	b.Observe(read(snap(), t0))
	b.Check(named("w1", "x", "alpine"), snap(), cfg)
	c.t = t0.Add(1 * time.Second)
	b.ContainerEvent("start", "x1", "x", nil)
	c.t = t0.Add(2 * time.Second)
	b.ContainerEvent("die", "x1", "x", nil)
	if ls := b.List(); len(ls) != 0 {
		t.Fatalf("leases = %+v, want none", ls)
	}
	c.t = t0.Add(3 * time.Second)
	b.Check(startOf("w1", "x"), snap(), cfg)
	// x1's second start is lost.
	c.t = t0.Add(5 * time.Second)
	b.ContainerEvent("die", "x1", "x", nil)
	c.t = t0.Add(6 * time.Second)
	b.Observe(read(withNamed(snap(), "x1", "x", "alpine", "w1"), t0.Add(4*time.Second))) // began before x1's die
	if ls := b.List(); len(ls) != 0 {
		t.Errorf("leases = %+v, want docker start x bound to x1 and ended", ls)
	}
	c.t = t0.Add(3 * time.Minute)
	b.Observe(read(snap(), c.t.Add(-time.Second)))
	if strings.Contains(log.String(), "never appeared") {
		t.Errorf("w1's docker start warned never appeared: %s", log)
	}
}

// w1 and w2 each run a one-off of project p, so its start event is a
// tie and the reading binds it. A one-off the reading lists that exits
// (--rm) after the reading began ends w1's lease at once, not at its
// timeout with "never appeared".
func TestATiedOneoffThatDiesDuringTheReadingEndsItsLease(t *testing.T) {
	b, c, log := book(t)
	lab := map[string]string{"com.docker.compose.project": "p", "com.docker.compose.oneoff": "True"}
	b.Observe(read(snap(), t0))
	b.Check(run("w1", "p"), snap(), cfg)
	b.Check(run("w2", "p"), snap(), cfg)
	c.t = t0.Add(2 * time.Second)
	b.ContainerEvent("start", "r1", "r1", lab) // a tie: not bound
	c.t = t0.Add(6 * time.Second)
	b.ContainerEvent("die", "r1", "r1", lab)
	c.t = t0.Add(7 * time.Second)
	b.Observe(read(oneoff("r1", "p", "w1")(snap()), t0.Add(3*time.Second))) // began before r1's die
	for _, l := range b.List() {
		if l.Worktree == "w1" {
			t.Errorf("w1's lease still holds %d MiB after its one-off ran and exited", l.Bytes>>20)
		}
	}
	c.t = t0.Add(3 * time.Minute)
	b.Observe(read(snap(), c.t.Add(-time.Second)))
	if strings.Contains(log.String(), "never appeared") && strings.Contains(log.String(), "worktree=w1") {
		t.Errorf("w1's run warned never appeared: %s", log)
	}
}

// A tied start newer than the reading is no die: a one-off judged in an
// earlier run, gone since, and started again while w1 and w2 each run one
// of project p, binds w1's lease from the reading that lists it.
func TestATiedStartDuringTheReadingOfAJudgedOneoffBindsItsLease(t *testing.T) {
	b, c, _ := book(t)
	lab := map[string]string{"com.docker.compose.project": "p", "com.docker.compose.oneoff": "True"}
	b.Observe(read(snap(), t0))
	b.Check(run("w1", "p"), snap(), cfg)
	c.t = t0.Add(5 * time.Second)
	b.Observe(read(oneoff("r1", "p", "w1")(snap()), t0.Add(4*time.Second))) // judged: its run's
	for i := range 3 {
		c.t = t0.Add(time.Duration(10+5*i) * time.Second)
		b.Observe(read(snap(), c.t.Add(-time.Second))) // gone: its lease ends
	}
	if ls := b.List(); len(ls) != 0 {
		t.Fatalf("leases = %+v, want none", ls)
	}
	b.Check(run("w1", "p"), snap(), cfg)
	b.Check(run("w2", "p"), snap(), cfg)
	c.t = t0.Add(32 * time.Second)
	b.ContainerEvent("start", "r1", "r1", lab) // a tie: not bound
	c.t = t0.Add(33 * time.Second)
	b.Observe(read(oneoff("r1", "p", "w1")(snap()), t0.Add(31*time.Second))) // began before r1's start
	ls := b.List()
	i := slices.IndexFunc(ls, func(l protocol.Lease) bool { return l.Worktree == "w1" })
	if i < 0 || ls[i].Bytes != gib-gib/4 {
		t.Fatalf("leases = %+v, want w1's bound to r1", ls)
	}
}

// The same for a compose up checked after a service died: the up that
// starts nothing still logs never appeared.
func TestADieNewerThanTheReadingKeepsNeverAppeared(t *testing.T) {
	b, c, log := book(t)
	app := func(s *protocol.Snapshot) *protocol.Snapshot {
		return addContainer(s, protocol.Container{ID: "A1", Name: "app-db-1", MemoryBytes: gib / 2,
			Labels: map[string]string{"com.docker.compose.project": "app", protocol.ComposeWorkingDirLabel: "/Users/dev/src/a"}}, "w1")
	}
	b.Observe(read(app(snap()), t0))
	c.t = t0.Add(5 * time.Second)
	b.Observe(read(app(snap()), t0.Add(4*time.Second)))
	c.t = t0.Add(10 * time.Second)
	lab := map[string]string{"com.docker.compose.project": "app"}
	b.ContainerEvent("stop", "A1", "app-db-1", lab)
	b.ContainerEvent("die", "A1", "app-db-1", lab)
	c.t = t0.Add(11 * time.Second)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, Target: "app"}, snap(), cfg)
	c.t = t0.Add(12 * time.Second)
	b.Observe(read(app(snap()), t0.Add(9*time.Second))) // began before the die
	for i := 0; i < 40; i++ {
		c.t = c.t.Add(5 * time.Second)
		b.Observe(read(snap(), c.t.Add(-time.Second)))
	}
	if !strings.Contains(log.String(), "never appeared") {
		t.Fatalf("no never appeared for an up that started nothing: %s", log)
	}
}

// composeUp is a compose up whose project the shim read.
func composeUp(wt, project string) policy.Request {
	return policy.Request{Worktree: wt, Kind: "compose", Command: "docker compose up", CostBytes: gib, Target: project}
}

// stopped runs A1, a service of project app, in a reading, then compose
// stop: two readings without it make it gone. Its verdict is kept.
func stopped(b *lease.Book, c *clock) {
	lab := map[string]string{"com.docker.compose.project": "app"}
	b.Observe(read(snap(), t0))
	c.t = t0.Add(5 * time.Second)
	b.Observe(read(service("A1", "app", "w1")(snap()), t0.Add(4*time.Second)))
	c.t = t0.Add(10 * time.Second)
	b.ContainerEvent("stop", "A1", "A1", lab)
	b.ContainerEvent("die", "A1", "A1", lab)
	for range 2 {
		c.t = c.t.Add(5 * time.Second)
		b.Observe(read(snap(), c.t.Add(-time.Second)))
	}
}

// diesDuringTheReading starts A1 again, and it exits (a one-shot
// service) after a reading began that lists it.
func diesDuringTheReading(b *lease.Book, c *clock) {
	lab := map[string]string{"com.docker.compose.project": "app"}
	c.t = c.t.Add(2 * time.Second)
	b.ContainerEvent("start", "A1", "A1", lab)
	began := c.t.Add(time.Second)
	c.t = c.t.Add(3 * time.Second)
	b.ContainerEvent("die", "A1", "A1", lab)
	c.t = c.t.Add(time.Second)
	b.Observe(read(service("A1", "app", "w1")(snap()), began))
}

// w1 and w2 each run compose up of project app, so the start event of
// A1, stopped since its last reading, is a tie. A1 exits during the
// reading that lists it: the reading binds it to w1's up, as for a new
// service. The verdict kept from its last life is not its start event's.
func TestATiedUpOfAStoppedServiceThatDiesDuringTheReadingBinds(t *testing.T) {
	b, c, log := book(t)
	stopped(b, c)
	log.Reset()
	b.Check(composeUp("w1", "app"), snap(), cfg)
	b.Check(composeUp("w2", "app"), snap(), cfg)
	diesDuringTheReading(b, c)
	if u := b.Ungated(); len(u) != 0 {
		t.Errorf("A1, started by a checked up, is listed ungated: %+v", u)
	}
	c.t = c.t.Add(4 * time.Minute)
	b.Observe(read(snap(), c.t.Add(-time.Second)))
	if strings.Contains(log.String(), "never appeared") && strings.Contains(log.String(), "worktree=w1") {
		t.Errorf("w1's up warned never appeared: %s", log)
	}
}

// The same with no earlier life: the tied start binds nothing, and the
// reading binds A1.
func TestATiedUpOfANewServiceThatDiesDuringTheReadingBinds(t *testing.T) {
	b, c, log := book(t)
	b.Observe(read(snap(), t0))
	b.Check(composeUp("w1", "app"), snap(), cfg)
	b.Check(composeUp("w2", "app"), snap(), cfg)
	diesDuringTheReading(b, c)
	c.t = c.t.Add(4 * time.Minute)
	b.Observe(read(snap(), c.t.Add(-time.Second)))
	if strings.Contains(log.String(), "never appeared") && strings.Contains(log.String(), "worktree=w1") {
		t.Errorf("w1's up warned never appeared: %s", log)
	}
}

// The same when w1's up started A1 in its last life: its gated verdict,
// kept since compose stop, is not this start event's either.
func TestATiedUpOfAGatedStoppedServiceThatDiesDuringTheReadingBinds(t *testing.T) {
	b, c, log := book(t)
	lab := map[string]string{"com.docker.compose.project": "app"}
	b.Observe(read(snap(), t0))
	b.Check(composeUp("w1", "app"), snap(), cfg)
	c.t = t0.Add(2 * time.Second)
	b.ContainerEvent("start", "A1", "A1", lab) // binds w1's up: gated
	for c.t.Before(t0.Add(130 * time.Second)) {
		c.t = c.t.Add(5 * time.Second)
		b.Observe(read(service("A1", "app", "w1")(snap()), c.t.Add(-time.Second)))
	}
	if ls := b.List(); len(ls) != 0 {
		t.Fatalf("leases = %+v, want none", ls)
	}
	c.t = c.t.Add(2 * time.Second)
	b.ContainerEvent("stop", "A1", "A1", lab)
	b.ContainerEvent("die", "A1", "A1", lab)
	for range 2 {
		c.t = c.t.Add(5 * time.Second)
		b.Observe(read(snap(), c.t.Add(-time.Second)))
	}
	log.Reset()
	b.Check(composeUp("w1", "app"), snap(), cfg)
	b.Check(composeUp("w2", "app"), snap(), cfg)
	diesDuringTheReading(b, c)
	c.t = c.t.Add(4 * time.Minute)
	b.Observe(read(snap(), c.t.Add(-time.Second)))
	if strings.Contains(log.String(), "never appeared") && strings.Contains(log.String(), "worktree=w1") {
		t.Errorf("w1's up warned never appeared: %s", log)
	}
}

// A guessed up in one worktree: the start event's container has no
// worktree, so the guess keys no lease. A1, stopped since its last
// reading, exits during the reading that lists it: the reading binds it to
// w1's up and judges it gated.
func TestAGuessedUpOfAStoppedServiceThatDiesDuringTheReadingBinds(t *testing.T) {
	b, c, _ := book(t)
	stopped(b, c)
	b.Check(guessed("w1", "app"), snap(), cfg)
	diesDuringTheReading(b, c)
	if ls := b.List(); len(ls) != 1 || ls[0].Bytes != gib-gib/4 {
		t.Errorf("leases = %+v, want w1's bound to A1", ls)
	}
	if u := b.Ungated(); len(u) != 0 {
		t.Errorf("A1, started by w1's checked up, is listed ungated: %+v", u)
	}
}

// A container gated in its last life, stopped, then started through the
// socket with no check, that exits during the reading that lists it: it
// keeps its verdict, so it is not warned.
func TestAGatedContainerRestartedUngatedThatDiesDuringTheReadingKeepsItsVerdict(t *testing.T) {
	b, c, log := book(t)
	b.Observe(read(snap(), t0))
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker run", CostBytes: gib, Name: "x"}, snap(), cfg)
	c.t = t0.Add(5 * time.Second)
	b.Observe(read(withContainerMem(snap(), "x", "w1", gib/2), t0.Add(4*time.Second)))
	c.t = t0.Add(10 * time.Second)
	b.ContainerEvent("stop", "x", "x", nil)
	b.ContainerEvent("die", "x", "x", nil)
	for range 2 {
		c.t = c.t.Add(5 * time.Second)
		b.Observe(read(snap(), c.t.Add(-time.Second)))
	}
	log.Reset()
	c.t = c.t.Add(2 * time.Second)
	b.ContainerEvent("start", "x", "x", nil)
	began := c.t.Add(time.Second)
	c.t = c.t.Add(3 * time.Second)
	b.ContainerEvent("die", "x", "x", nil)
	c.t = c.t.Add(time.Second)
	b.Observe(read(withContainerMem(snap(), "x", "w1", gib/2), began))
	if strings.Contains(log.String(), "ungated") {
		t.Errorf("x was warned ungated: %s", log)
	}
	if u := b.Ungated(); len(u) != 0 {
		t.Errorf("ungated = %+v, want none", u)
	}
}

// x1's die and its second start come only when the events stream
// reconnects, replayed with the times they happened (#146). The die that
// follows ends docker start x, with no "never appeared" (#131's F1, which
// lost them).
func TestAReplayedDieAndStartEndTheStartThatRanAgain(t *testing.T) {
	b, c, log := book(t)
	b.Observe(read(snap(), t0))
	b.Check(named("w1", "x", "alpine"), snap(), cfg)
	c.t = t0.Add(1 * time.Second)
	b.ContainerEvent("start", "x1", "x", nil)
	c.t = t0.Add(3 * time.Second)
	b.Check(startOf("w1", "x"), snap(), cfg)
	c.t = t0.Add(5 * time.Second) // the reconnect
	b.ContainerEventAt(t0.Add(2*time.Second), "die", "x1", "x", nil)
	b.ContainerEventAt(t0.Add(4*time.Second), "start", "x1", "x", nil)
	b.ContainerEvent("die", "x1", "x", nil)
	c.t = t0.Add(6 * time.Second)
	b.Observe(read(snap(), t0.Add(5500*time.Millisecond)))
	if ls := b.List(); len(ls) != 0 {
		t.Errorf("leases = %+v, want docker start x bound to x1 and ended", ls)
	}
	c.t = t0.Add(3 * time.Minute)
	b.Observe(read(snap(), c.t.Add(-time.Second)))
	if strings.Contains(log.String(), "never appeared") {
		t.Errorf("warned never appeared: %s", log)
	}
}

// A crash, then compose up, then its start, the crash and the start
// replayed after the up: the crash is dated before the up, so the up was
// checked since, and its start binds db. The up then reserves only what db
// does not use yet, rather than count db twice (#146).
func TestAReplayedCrashBeforeAnUpBindsTheStart(t *testing.T) {
	b, c, _ := book(t)
	lab := map[string]string{protocol.ComposeProjectLabel: "app"}
	s := read(withComposeContainer(snap(), "db", "w1", gib), c.t)
	b.Observe(s)
	b.Observe(s)
	crash := c.t.Add(time.Second)
	c.t = c.t.Add(2 * time.Second)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: gib, Target: "app", OnEngine: true}, s, cfg)
	start := c.t.Add(time.Second)
	c.t = c.t.Add(2 * time.Second) // the reconnect
	b.ContainerEventAt(crash, "die", "db", "db", lab)
	b.ContainerEventAt(start, "start", "db", "db", lab)
	observe(b, c, withComposeContainer(snap(), "db", "w1", gib/4))
	if r := reserved(b); r != gib-gib/4 {
		t.Fatalf("reserved %d MiB once db is back, want 768", r>>20)
	}
}

// docker start c1 c2, then c1 crashes and a compose up starts it, the
// crash and the start replayed after the up: the up was checked between
// them, so it takes c1 (deaths.takenBy, #146).
func TestAReplayedDeathOfAStartOfSeveralIsTakenByTheUpBetween(t *testing.T) {
	b, c, log, p1, up := p1Started(t)
	lab := map[string]string{protocol.ComposeProjectLabel: "p1"}
	crash := c.t.Add(time.Second)
	c.t = c.t.Add(2 * time.Second)
	d := b.Check(up, p1(0, gib/8), cfg)
	start := c.t.Add(time.Second)
	c.t = c.t.Add(2 * time.Second) // the reconnect
	b.ContainerEventAt(crash, "die", "c1", "c1", lab)
	b.ContainerEventAt(start, "start", "c1", "c1", lab)
	for range 30 {
		c.t = c.t.Add(5 * time.Second)
		b.Observe(p1(gib, gib/8))
	}
	if slices.ContainsFunc(b.List(), func(l protocol.Lease) bool { return l.ID == d.LeaseID }) || strings.Contains(log.String(), "never appeared\" lease="+d.LeaseID) {
		t.Fatalf("w1's up %s never took c1: leases = %+v\n%s", d.LeaseID, b.List(), log)
	}
}
