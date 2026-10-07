package attribution_test

import (
	"encoding/json"
	"strings"
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
	if w.ContainerMemoryBytes != 3*gib || w.ContainerCPUPercent == nil || *w.ContainerCPUPercent != 15 {
		t.Errorf("container totals = %d, %v", w.ContainerMemoryBytes, w.ContainerCPUPercent)
	}
	if w.AgentMemoryBytes == nil || *w.AgentMemoryBytes != gib || w.AgentCPUPercent == nil || *w.AgentCPUPercent != 30 {
		t.Errorf("agent totals = %v, %v", w.AgentMemoryBytes, w.AgentCPUPercent)
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
	for name, edit := range map[string]func(*protocol.Snapshot){
		"no reading":  func(s *protocol.Snapshot) { s.Orca = nil },
		"not running": func(s *protocol.Snapshot) { s.Orca = &protocol.Orca{Installed: true, Worktrees: []protocol.Worktree{}} },
	} {
		t.Run(name, func(t *testing.T) {
			s := snapshot()
			edit(s)
			a := attribution.Attribute(s)
			if len(a.Worktrees) != 0 || len(a.Unattributed.Containers) != 4 || len(a.Unattributed.TartVMs) != 2 ||
				a.Unattributed.Containers[0].Reason != attribution.OrcaUnknown {
				t.Fatalf("got %+v", a)
			}
		})
	}
}

func TestStaleOrcaIsFlagged(t *testing.T) {
	s := snapshot()
	s.Sources = map[string]protocol.SourceStatus{"orca": {Stale: true}}
	if a := attribution.Attribute(s); !a.OrcaStale || len(a.Worktrees[0].Containers) != 2 {
		t.Fatalf("got stale=%v, %d containers; want the last worktrees used and flagged", a.OrcaStale, len(a.Worktrees[0].Containers))
	}
}

func TestUnknownAgentMemoryIsNotZero(t *testing.T) {
	s := snapshot()
	s.Orca.MemoryError = "diagnostics failed"
	s.Orca.Worktrees[0].MemoryBytes, s.Orca.Worktrees[0].CPUPercent = 0, 0
	if w := attribution.Attribute(s).Worktrees[0]; w.AgentMemoryBytes != nil || w.AgentCPUPercent != nil {
		t.Fatalf("agent memory = %v, cpu = %v; want unknown", w.AgentMemoryBytes, w.AgentCPUPercent)
	}
}

func TestContainerCPUUnknownWhenAnyIs(t *testing.T) {
	s := snapshot()
	s.Docker.Containers[1].CPUPercent = nil // first tick
	if w := attribution.Attribute(s).Worktrees[0]; w.ContainerCPUPercent != nil {
		t.Fatalf("cpu = %v, want unknown", *w.ContainerCPUPercent)
	}
}

func TestEmptySnapshot(t *testing.T) {
	a := attribution.Attribute(&protocol.Snapshot{})
	if len(a.Worktrees) != 0 || len(a.Unattributed.Containers) != 0 {
		t.Fatalf("got %+v", a)
	}
}

func TestEmptyListsAreArraysNotNull(t *testing.T) {
	s := snapshot()
	s.Docker, s.Tart = nil, nil
	b, err := json.Marshal(attribution.Attribute(s))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "null") {
		t.Fatalf("json has null lists: %s", b)
	}
}
