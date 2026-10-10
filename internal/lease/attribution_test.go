package lease_test

import (
	"testing"

	"github.com/cybagard/cyba-headroom/internal/attribution"
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
