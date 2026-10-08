package lease_test

import (
	"strings"
	"testing"

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
