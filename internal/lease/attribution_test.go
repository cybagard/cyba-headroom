package lease_test

import (
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/attribution"
	"github.com/cybagard/cyba-headroom/internal/lease"
	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// A container a worktree's docker run started, with no mount or compose
// dir to say whose it is, is placed under that worktree by its lease (#74).
func TestObservePlacesALeasedContainerUnderItsWorktree(t *testing.T) {
	b, _, _ := book(t)
	const w = "repo::/Users/dev/w/project-a"
	tick := func(cs ...protocol.Container) *protocol.Snapshot {
		s := snap()
		s.Orca.Worktrees = []protocol.Worktree{{ID: w, Path: "/Users/dev/w/project-a", Name: "A"}}
		s.Docker.Containers = cs
		a := attribution.Attribute(s)
		s.Attribution = &a
		b.Observe(s)
		return s
	}
	tick()
	d := b.Check(req(w, gib), snap(), cfg)
	if !d.Allow {
		t.Fatalf("check = %+v", d)
	}
	s := tick(protocol.Container{ID: "c1", Name: "app", MemoryBytes: gib / 2,
		Labels: map[string]string{protocol.LeaseLabel: d.LeaseID}})
	cs := s.Attribution.Worktrees[0].Containers
	if len(cs) != 1 || cs[0].ID != "c1" || cs[0].By != attribution.ByLease || s.Attribution.Worktrees[0].ContainerMemoryBytes != gib/2 ||
		len(s.Attribution.Unattributed.Containers) != 0 {
		t.Fatalf("attribution = %+v", s.Attribution)
	}
}

// A stack placed under its worktree only by its lease is not that
// worktree's to the book: an up of it again, once the lease ended, holds
// and binds nothing it would not without the lease (#74, B1).
func TestALeasePlacedStackIsNotHeldByTheNextUp(t *testing.T) {
	b, c, _ := book(t)
	const w = "repo::/Users/dev/w/project-a"
	tick := func(cs ...protocol.Container) *protocol.Snapshot {
		s := snap()
		s.Orca.Worktrees = []protocol.Worktree{{ID: w, Path: "/Users/dev/w/project-a", Name: "A",
			Agents: []protocol.Agent{{State: "working"}}}}
		s.Docker.Containers = cs
		a := attribution.Attribute(s)
		s.Attribution = &a
		b.Observe(read(s, c.t))
		return s
	}
	up := policy.Request{Worktree: w, Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: gib, Target: "app", OnEngine: true}
	db := protocol.Container{ID: "db", Name: "db", MemoryBytes: gib, Labels: map[string]string{
		protocol.ComposeProjectLabel: "app", protocol.ComposeWorkingDirLabel: "/Users/dev/elsewhere"}}
	tick()
	if d := b.Check(up, snap(), cfg); !d.Allow {
		t.Fatalf("first up = %+v", d)
	}
	c.t = c.t.Add(5 * time.Second)
	s := tick(db) // it uses the cost: the lease ends
	if l := b.List(); len(l) != 0 {
		t.Fatalf("leases = %+v", l)
	}
	if cs := s.Attribution.Worktrees[0].Containers; len(cs) != 1 || cs[0].By != attribution.ByLease {
		t.Fatalf("db not placed by its lease: %+v", s.Attribution)
	}
	d := b.Check(up, snap(), cfg)
	if !d.Allow {
		t.Fatalf("second up = %+v", d)
	}
	if k := lease.Bound(b, d.LeaseID); len(k) != 0 {
		t.Fatalf("second up's lease bound %v at its check", k)
	}
	c.t = c.t.Add(5 * time.Second)
	tick(db)
	if k := lease.Bound(b, d.LeaseID); len(k) != 0 {
		t.Fatalf("second up's lease bound %v", k)
	}
}
