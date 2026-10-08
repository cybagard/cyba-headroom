package samples_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/samples"
)

func u64(v uint64) *uint64 { return &v }

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// fullSnapshot has every section filled, with values a sample must not keep.
func fullSnapshot() *protocol.Snapshot {
	cpu := 12.5
	headroom := int64(30 << 30)
	ttl := time.Hour
	lastUsed := t0.Add(-2 * time.Hour)
	return &protocol.Snapshot{
		Seq: 7, CollectedAt: t0,
		Sources: map[string]protocol.SourceStatus{"host": {}, "docker": {Stale: true, Err: "timed out"}},
		Host: &protocol.Host{TotalBytes: 64 << 30, UsedBytes: u64(20 << 30), CompressorBytes: u64(1 << 30),
			CompressedBytes: u64(3 << 30), Pressure: "warn", FreePercent: 41, SwapUsedBytes: 1 << 20},
		Docker: &protocol.Docker{Running: true, VMRunning: true, VMFootprintBytes: 4 << 30, Containers: []protocol.Container{{
			ID: "abc", Name: "db", Image: "postgres:17", MemoryBytes: 1 << 30, CPUPercent: &cpu,
			Labels: map[string]string{
				"com.docker.compose.project.working_dir": "/Users/dev/project-a",
				"secret.token":                           "s3cr3t-value",
			},
			Mounts: []string{"/Users/dev/project-a/data"},
		}}},
		Tart: &protocol.Tart{Installed: true, VMs: []protocol.TartVM{{
			Name: "ci-mac", OS: "darwin", CPUs: 4, MemoryBytes: 8 << 30, FootprintBytes: u64(5 << 30),
			RunPID: 4242, LaunchCwd: "/Users/dev/project-b", SharedDirs: []string{"/Users/dev/project-b"},
		}}},
		LMStudio: &protocol.LMStudio{Installed: true, Running: true, FootprintBytes: u64(13 << 30),
			Models: []protocol.LoadedModel{{Key: "some-model", SizeBytes: 12 << 30, Status: "idle", TTL: &ttl, LastUsedAt: &lastUsed},
				{Key: "pinned", SizeBytes: 1 << 30}}},
		Orca: &protocol.Orca{Installed: true, Running: true, AppMemoryBytes: 2 << 30, Worktrees: []protocol.Worktree{{
			ID: "00000000-0000-4000-8000-000000000001::/Users/dev/project-a", Path: "/Users/dev/project-a",
			Name: "project-a", Branch: "dev/feature", MemoryBytes: 3 << 30, CPUPercent: 55,
			SessionPIDs: []int{100}, Agents: []protocol.Agent{{PaneKey: "p1", Type: "claude", State: "working"}},
		}}},
		Budget: &protocol.Budget{TotalBytes: 64 << 30, ReservedBytes: 34 << 30, HeadroomBytes: &headroom},
	}
}

func TestFromSnapshotKeepsAttributionKeys(t *testing.T) {
	s := samples.FromSnapshot(fullSnapshot())
	if s.V != samples.Version || !s.T.Equal(t0) {
		t.Fatalf("v=%d t=%v", s.V, s.T)
	}
	if s.Host == nil || s.Host.Pressure != "warn" || *s.Host.CompressedBytes != 3<<30 {
		t.Errorf("host = %+v", s.Host)
	}
	if s.Budget == nil || s.Budget.ReservedBytes != 34<<30 {
		t.Errorf("budget = %+v", s.Budget)
	}
	if len(s.Containers) != 1 || s.Containers[0].ComposeDir != "/Users/dev/project-a" ||
		s.Containers[0].Mounts[0] != "/Users/dev/project-a/data" || s.Containers[0].MemoryBytes != 1<<30 {
		t.Errorf("containers = %+v", s.Containers)
	}
	if s.Docker == nil || s.Docker.VMFootprintBytes != 4<<30 {
		t.Errorf("docker = %+v", s.Docker)
	}
	if len(s.TartVMs) != 1 || s.TartVMs[0].LaunchCwd != "/Users/dev/project-b" || *s.TartVMs[0].FootprintBytes != 5<<30 {
		t.Errorf("tart = %+v", s.TartVMs)
	}
	if s.LMStudio == nil || *s.LMStudio.FootprintBytes != 13<<30 || s.LMStudio.Models[0].SizeBytes != 12<<30 {
		t.Errorf("lmstudio = %+v", s.LMStudio)
	}
	if s.OrcaAppBytes == nil || *s.OrcaAppBytes != 2<<30 || len(s.Worktrees) != 1 ||
		s.Worktrees[0].Path != "/Users/dev/project-a" || s.Worktrees[0].Agents[0] != "working" {
		t.Errorf("orca = %v %+v", s.OrcaAppBytes, s.Worktrees)
	}
	if !s.Stale["docker"] || s.Stale["host"] {
		t.Errorf("stale = %v", s.Stale)
	}
}

func TestFromSnapshotKeepsModelIdleness(t *testing.T) {
	snap := fullSnapshot()
	m := samples.FromSnapshot(snap).LMStudio.Models
	if m[0].TTL == nil || *m[0].TTL != time.Hour || m[0].LastUsedAt == nil || !m[0].LastUsedAt.Equal(t0.Add(-2*time.Hour)) ||
		m[0].Status != "idle" {
		t.Fatalf("model = %+v", m[0])
	}
	// Copies, not the snapshot's pointers.
	*snap.LMStudio.Models[0].TTL = time.Minute
	if *m[0].TTL != time.Hour {
		t.Fatal("sample shares the snapshot's TTL")
	}
	if m[1].TTL != nil || m[1].LastUsedAt != nil {
		t.Fatalf("pinned model = %+v: want no TTL and no last use", m[1])
	}
}

func TestFromSnapshotDropsLabelsAndProcessDetails(t *testing.T) {
	b, err := json.Marshal(samples.FromSnapshot(fullSnapshot()))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"s3cr3t", "secret.token", "4242", "session", "pane"} {
		if strings.Contains(strings.ToLower(string(b)), leak) {
			t.Errorf("sample contains %q: %s", leak, b)
		}
	}
}

func TestFromEmptySnapshot(t *testing.T) {
	s := samples.FromSnapshot(&protocol.Snapshot{CollectedAt: t0})
	if s.Host != nil || s.Docker != nil || s.Containers != nil || s.Worktrees != nil {
		t.Fatalf("empty snapshot gave %+v", s)
	}
}
