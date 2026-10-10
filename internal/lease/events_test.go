package lease_test

import (
	"strings"
	"testing"
	"time"

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
