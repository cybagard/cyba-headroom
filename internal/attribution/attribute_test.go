package attribution_test

import (
	"testing"

	"github.com/cybagard/cyba-headroom/internal/attribution"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

const gib = uint64(1 << 30)

func u64(v uint64) *uint64   { return &v }
func f64(v float64) *float64 { return &v }

func snapshot() *protocol.Snapshot {
	return &protocol.Snapshot{
		Orca: &protocol.Orca{Installed: true, Running: true, Worktrees: []protocol.Worktree{
			{ID: "repo::/Users/dev/w/fix-login", Path: "/Users/dev/w/fix-login", Name: "Fix login", MemoryBytes: 1 * gib, CPUPercent: 30},
			{ID: "repo::/Users/dev/w/project-b", Path: "/Users/dev/w/project-b", Name: "B"},
		}},
		Docker: &protocol.Docker{Running: true, Containers: []protocol.Container{
			{ID: "c1", Name: "db", MemoryBytes: 2 * gib, CPUPercent: f64(10),
				Labels: map[string]string{"com.docker.compose.project.working_dir": "/Users/dev/w/fix-login"}},
			{ID: "c2", Name: "web", MemoryBytes: 1 * gib, CPUPercent: f64(5), Mounts: []string{"/Users/dev/w/fix-login/src"}},
			{ID: "c3", Name: "stray", MemoryBytes: 3 * gib},
			{ID: "c4", Name: "both", MemoryBytes: gib, Mounts: []string{"/Users/dev/w/fix-login", "/Users/dev/w/project-b"}},
		}},
		Tart: &protocol.Tart{Installed: true, VMs: []protocol.TartVM{
			{Name: "ci", MemoryBytes: 8 * gib, FootprintBytes: u64(5 * gib), LaunchCwd: "/Users/dev/w/project-b"},
			{Name: "orphan", MemoryBytes: 4 * gib},
		}},
	}
}

// Given a container started from worktree fix-login, when attribution runs,
// then it appears under fix-login (R3).
func TestContainerFromWorktreeAppearsUnderIt(t *testing.T) {
	a := attribution.Attribute(snapshot())
	w := a.Worktrees[0]
	if w.Path != "/Users/dev/w/fix-login" || len(w.Containers) != 2 ||
		w.Containers[0].Name != "db" || w.Containers[0].By != attribution.ByComposeDir ||
		w.Containers[1].Name != "web" || w.Containers[1].By != attribution.ByMount {
		t.Fatalf("fix-login = %+v", w)
	}
	if w.ContainerMemoryBytes != 3*gib || w.ContainerCPUPercent != 15 {
		t.Errorf("container totals = %d, %.1f%%", w.ContainerMemoryBytes, w.ContainerCPUPercent)
	}
	if w.AgentMemoryBytes != gib || w.AgentCPUPercent != 30 {
		t.Errorf("agent totals = %d, %.1f%%", w.AgentMemoryBytes, w.AgentCPUPercent)
	}
}

func TestTartVMsAndTotals(t *testing.T) {
	a := attribution.Attribute(snapshot())
	w := a.Worktrees[1]
	if len(w.Containers) != 0 || len(w.TartVMs) != 1 || w.TartVMs[0].By != attribution.ByLaunchCwd ||
		w.TartMemoryBytes != 8*gib || w.TartFootprintBytes == nil || *w.TartFootprintBytes != 5*gib {
		t.Fatalf("project-b = %+v", w)
	}
}

func TestUnattributedWithReasons(t *testing.T) {
	u := attribution.Attribute(snapshot()).Unattributed
	if len(u.Containers) != 2 || u.Containers[0].Name != "stray" || u.Containers[0].Reason != attribution.NoMatch ||
		u.Containers[1].Name != "both" || u.Containers[1].Reason != attribution.Ambiguous {
		t.Fatalf("containers = %+v", u.Containers)
	}
	if len(u.TartVMs) != 1 || u.TartVMs[0].Name != "orphan" || u.ContainerMemoryBytes != 4*gib || u.TartMemoryBytes != 4*gib ||
		u.TartFootprintBytes != nil {
		t.Fatalf("unattributed = %+v", u)
	}
}

func TestEverythingUnattributedWithoutOrca(t *testing.T) {
	s := snapshot()
	s.Orca = nil
	a := attribution.Attribute(s)
	if len(a.Worktrees) != 0 || len(a.Unattributed.Containers) != 4 || len(a.Unattributed.TartVMs) != 2 ||
		a.Unattributed.Containers[0].Reason != attribution.OrcaUnknown {
		t.Fatalf("got %+v", a)
	}
}

func TestEmptySnapshot(t *testing.T) {
	a := attribution.Attribute(&protocol.Snapshot{})
	if len(a.Worktrees) != 0 || len(a.Unattributed.Containers) != 0 {
		t.Fatalf("got %+v", a)
	}
}
