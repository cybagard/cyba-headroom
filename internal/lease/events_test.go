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
	if l := b.List(); len(l) != 0 {
		t.Fatalf("leases = %+v: a later reading without x ends it", l)
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
