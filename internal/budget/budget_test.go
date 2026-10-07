package budget_test

import (
	"slices"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/budget"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

const gib = uint64(1 << 30)

func TestEmptySnapshotMarksEverySourceUnknown(t *testing.T) {
	b := budget.Compute(&protocol.Snapshot{}, budget.Params{})
	if b.TotalBytes != 0 || b.ReservedBytes != 0 || b.HeadroomBytes != nil {
		t.Fatalf("got total=%d reserved=%d headroom=%v, want zeros and unknown headroom", b.TotalBytes, b.ReservedBytes, b.HeadroomBytes)
	}
	want := []string{"host", "docker", "tart", "lmstudio"}
	if !slices.Equal(b.Unknown, want) {
		t.Fatalf("unknown = %v, want %v", b.Unknown, want)
	}
}

func u64(v uint64) *uint64 { return &v }

// component returns the named component, failing the test if it is missing.
func component(t *testing.T, b protocol.Budget, name string) protocol.BudgetComponent {
	t.Helper()
	for _, c := range b.Components {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q component in %+v", name, b.Components)
	return protocol.BudgetComponent{}
}

// usedEq reports whether a component's used bytes match want (nil = unknown).
func usedEq(got, want *uint64) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

func TestTartReservesConfiguredMemory(t *testing.T) {
	cases := map[string]struct {
		vms      []protocol.TartVM
		reserved uint64
		used     *uint64
	}{
		"no VMs": {nil, 0, u64(0)},
		"two VMs": {
			[]protocol.TartVM{
				{Name: "a", MemoryBytes: 8 * gib, FootprintBytes: u64(5 * gib)},
				{Name: "b", MemoryBytes: 4 * gib, FootprintBytes: u64(3 * gib)},
			},
			12 * gib, u64(8 * gib),
		},
		"footprint above configured": {
			[]protocol.TartVM{{Name: "a", MemoryBytes: 4 * gib, FootprintBytes: u64(4*gib + gib/2)}},
			4*gib + gib/2, u64(4*gib + gib/2),
		},
		"footprint unknown": {
			[]protocol.TartVM{
				{Name: "a", MemoryBytes: 8 * gib, FootprintBytes: u64(5 * gib)},
				{Name: "booting", MemoryBytes: 4 * gib},
			},
			12 * gib, nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := &protocol.Snapshot{Tart: &protocol.Tart{Installed: true, VMs: tc.vms}}
			c := component(t, budget.Compute(s, budget.Params{}), "tart")
			if c.ReservedBytes != tc.reserved || !usedEq(c.UsedBytes, tc.used) {
				t.Fatalf("got reserved=%d used=%v, want %d %v", c.ReservedBytes, c.UsedBytes, tc.reserved, tc.used)
			}
		})
	}
}

func TestDockerReservesFootprintOrContainersPlusOverhead(t *testing.T) {
	p := budget.Params{DockerOverheadBytes: 2 * gib}
	running := func(footprint uint64, containers ...uint64) *protocol.Docker {
		d := &protocol.Docker{Running: true, VMRunning: true, VMLimitBytes: 31 * gib, VMFootprintBytes: footprint}
		for _, m := range containers {
			d.Containers = append(d.Containers, protocol.Container{MemoryBytes: m})
		}
		return d
	}
	cases := map[string]struct {
		d        *protocol.Docker
		reserved uint64
		used     *uint64
	}{
		"not running":           {&protocol.Docker{}, 0, u64(0)},
		"resource saver":        {&protocol.Docker{Running: true, VMLimitBytes: 31 * gib}, 0, u64(0)},
		"retained high water":   {running(6*gib, 1*gib), 6 * gib, u64(6 * gib)},
		"containers + overhead": {running(3*gib, 2*gib, 1*gib), 5 * gib, u64(3 * gib)},
		"VM unreadable": {
			&protocol.Docker{Running: true, VMLimitBytes: 31 * gib, VMError: "boom",
				Containers: []protocol.Container{{MemoryBytes: 1 * gib}}},
			3 * gib, nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := component(t, budget.Compute(&protocol.Snapshot{Docker: tc.d}, p), "docker")
			if c.ReservedBytes != tc.reserved || !usedEq(c.UsedBytes, tc.used) {
				t.Fatalf("got reserved=%d used=%v, want %d %v", c.ReservedBytes, c.UsedBytes, tc.reserved, tc.used)
			}
		})
	}
}

func TestLMStudioReservesLoadedModels(t *testing.T) {
	p := budget.Params{LMStudioIdleBytes: 1 * gib}
	models := func(sizes ...uint64) []protocol.LoadedModel {
		var ms []protocol.LoadedModel
		for _, s := range sizes {
			ms = append(ms, protocol.LoadedModel{Key: "m", SizeBytes: s})
		}
		return ms
	}
	cases := map[string]struct {
		l        *protocol.LMStudio
		reserved uint64
		used     *uint64
	}{
		"not installed":   {&protocol.LMStudio{}, 0, u64(0)},
		"no model loaded": {&protocol.LMStudio{Installed: true, Running: true, FootprintBytes: u64(gib / 2)}, 1 * gib, u64(gib / 2)},
		"idle model, size wins": {
			&protocol.LMStudio{Installed: true, Running: true, Models: models(8*gib, 4*gib), FootprintBytes: u64(5 * gib)},
			13 * gib, u64(5 * gib),
		},
		"context and runtime on top": {
			&protocol.LMStudio{Installed: true, Running: true, Models: models(12 * gib), FootprintBytes: u64(14 * gib)},
			14 * gib, u64(14 * gib),
		},
		"loading, not listed yet": {
			&protocol.LMStudio{Installed: true, Running: true, FootprintBytes: u64(20 * gib)},
			20 * gib, u64(20 * gib),
		},
		"footprint unknown": {
			&protocol.LMStudio{Installed: true, Running: true, Models: models(12 * gib), FootprintError: "boom"},
			13 * gib, nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := component(t, budget.Compute(&protocol.Snapshot{LMStudio: tc.l}, p), "lmstudio")
			if c.ReservedBytes != tc.reserved || !usedEq(c.UsedBytes, tc.used) {
				t.Fatalf("got reserved=%d used=%v, want %d %v", c.ReservedBytes, c.UsedBytes, tc.reserved, tc.used)
			}
		})
	}
}

func i64(v int64) *int64 { return &v }

// busyMac is a 64 GiB Mac with every source reporting.
func busyMac() *protocol.Snapshot {
	return &protocol.Snapshot{
		// 1 GiB of compressor pages holding 4 GiB, which footprints count whole.
		Host: &protocol.Host{TotalBytes: 64 * gib, UsedBytes: u64(40 * gib),
			CompressorBytes: u64(1 * gib), CompressedBytes: u64(4 * gib)},
		Docker: &protocol.Docker{Running: true, VMRunning: true, VMFootprintBytes: 6 * gib,
			Containers: []protocol.Container{{MemoryBytes: 2 * gib}}},
		Tart: &protocol.Tart{Installed: true, VMs: []protocol.TartVM{
			{Name: "a", MemoryBytes: 8 * gib, FootprintBytes: u64(5 * gib)},
		}},
		LMStudio: &protocol.LMStudio{Installed: true, Running: true,
			Models: []protocol.LoadedModel{{SizeBytes: 12 * gib}}, FootprintBytes: u64(13 * gib)},
	}
}

func TestTotalsAndHeadroom(t *testing.T) {
	p := budget.Params{HostBaselineBytes: 10 * gib, DockerOverheadBytes: 2 * gib, LMStudioIdleBytes: 1 * gib}
	b := budget.Compute(busyMac(), p)

	// docker 6 + tart 8 + lmstudio max(12+1, 13) + baseline 10
	if b.TotalBytes != 64*gib || b.ReservedBytes != 37*gib || b.HeadroomBytes == nil || *b.HeadroomBytes != int64(27*gib) {
		t.Fatalf("got total=%d reserved=%d headroom=%v", b.TotalBytes, b.ReservedBytes, b.HeadroomBytes)
	}
	if b.Unknown != nil {
		t.Errorf("unknown = %v, want none", b.Unknown)
	}
	// 40 used − 1 compressor + 4 compressed − (6 docker + 5 tart + 13 lmstudio)
	if b.UnaccountedBytes == nil || *b.UnaccountedBytes != int64(19*gib) {
		t.Errorf("unaccounted = %v, want 19 GiB", b.UnaccountedBytes)
	}
	names := []string{}
	for _, c := range b.Components {
		names = append(names, c.Name)
	}
	if want := []string{"docker", "tart", "lmstudio", "host_baseline"}; !slices.Equal(names, want) {
		t.Errorf("components = %v, want %v", names, want)
	}
	if c := component(t, b, "host_baseline"); c.ReservedBytes != 10*gib || !usedEq(c.UsedBytes, u64(19*gib)) {
		t.Errorf("host_baseline = %d used %v, want 10 GiB used 19 GiB", c.ReservedBytes, c.UsedBytes)
	}
}

func TestHeadroomGoesNegativeWhenOverCommitted(t *testing.T) {
	b := budget.Compute(busyMac(), budget.Params{HostBaselineBytes: 40 * gib})
	if b.HeadroomBytes == nil || *b.HeadroomBytes >= 0 {
		t.Fatalf("headroom = %v, want negative", b.HeadroomBytes)
	}
}

func TestHeadroomUnknownWithoutHost(t *testing.T) {
	s := busyMac()
	s.Host = nil
	b := budget.Compute(s, budget.Params{HostBaselineBytes: 10 * gib})
	if b.HeadroomBytes != nil || b.ReservedBytes == 0 {
		t.Fatalf("headroom = %v reserved = %d, want unknown headroom and reserved still summed", b.HeadroomBytes, b.ReservedBytes)
	}
}

func TestUnaccounted(t *testing.T) {
	cases := map[string]struct {
		edit func(*protocol.Snapshot)
		want *int64
	}{
		"host used unknown":      {func(s *protocol.Snapshot) { s.Host.UsedBytes = nil }, nil},
		"compressed unknown":     {func(s *protocol.Snapshot) { s.Host.CompressedBytes = nil }, nil},
		"tart footprint unknown": {func(s *protocol.Snapshot) { s.Tart.VMs[0].FootprintBytes = nil }, nil},
		"no docker reading":      {func(s *protocol.Snapshot) { s.Docker = nil }, nil},
		"negative from skew": {
			func(s *protocol.Snapshot) { s.Host.UsedBytes = u64(20 * gib) }, i64(-int64(1 * gib)),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := busyMac()
			tc.edit(s)
			got := budget.Compute(s, budget.Params{}).UnaccountedBytes
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("unaccounted = %v, want %v", got, tc.want)
			}
			if got == nil {
				if c := component(t, budget.Compute(s, budget.Params{}), "host_baseline"); c.UsedBytes != nil {
					t.Errorf("host_baseline used = %d, want unknown", *c.UsedBytes)
				}
			}
		})
	}
}

func TestStaleSourcesAreFlagged(t *testing.T) {
	s := busyMac()
	s.Sources = map[string]protocol.SourceStatus{
		"host":   {},
		"docker": {Stale: true, Err: "timed out"},
		"tart":   {Stale: true},
		"orca":   {Stale: true}, // not a budget input
	}
	b := budget.Compute(s, budget.Params{})
	if want := []string{"docker", "tart"}; !slices.Equal(b.Stale, want) {
		t.Fatalf("stale = %v, want %v", b.Stale, want)
	}
	if b.ReservedBytes == 0 {
		t.Error("stale readings must still count")
	}
}
