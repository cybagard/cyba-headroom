package suggest_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/budget"
	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/samples"
	"github.com/cybagard/cyba-headroom/internal/suggest"
)

// A snapshot recorded as a sample and replayed gives the budget the daemon
// computed from the original.
func TestReplayGivesTheDaemonsBudget(t *testing.T) {
	p := budget.Params{HostBaselineBytes: 8 * gib, DockerOverheadBytes: 2 * gib, PodmanOverheadBytes: gib, LMStudioIdleBytes: gib / 2, OllamaIdleBytes: gib / 8}
	snaps := map[string]*protocol.Snapshot{
		"everything running": {
			Host: &protocol.Host{TotalBytes: 64 * gib, UsedBytes: u64(30 * gib), CompressorBytes: u64(gib), CompressedBytes: u64(3 * gib)},
			Engines: []protocol.Engine{{Kind: protocol.EngineDockerDesktop, Running: true, VMRunning: true, VMLimitBytes: 31 * gib, VMFootprintBytes: 6 * gib,
				VMSwappedBytes: u64(gib), Containers: []protocol.Container{{Name: "db", MemoryBytes: 5 * gib}}}},
			Tart: &protocol.Tart{Installed: true, VMs: []protocol.TartVM{{Name: "ci", MemoryBytes: 8 * gib, FootprintBytes: u64(9 * gib), SwappedBytes: u64(2 * gib)}}},
			LMStudio: &protocol.LMStudio{Installed: true, Running: true, FootprintBytes: u64(13 * gib),
				Models: []protocol.LoadedModel{{Key: "m", SizeBytes: 12 * gib}}},
			Ollama: &protocol.Ollama{Installed: true, Running: true, FootprintBytes: u64(3 * gib),
				Models: []protocol.OllamaModel{{Name: "o", SizeBytes: 2 * gib}}},
		},
		"nothing running": {
			Host:    &protocol.Host{TotalBytes: 64 * gib, UsedBytes: u64(18 * gib), CompressorBytes: u64(0), CompressedBytes: u64(0)},
			Engines: []protocol.Engine{}, Tart: &protocol.Tart{Installed: true}, LMStudio: &protocol.LMStudio{Installed: true},
			Ollama: &protocol.Ollama{Installed: true},
		},
		"sources unknown": {
			Host: &protocol.Host{TotalBytes: 64 * gib},
		},
		"docker VM unreadable": {
			Host: &protocol.Host{TotalBytes: 64 * gib},
			Engines: []protocol.Engine{{Kind: protocol.EngineDockerDesktop, Running: true, VMLimitBytes: 31 * gib, VMError: "lsof: timed out",
				Containers: []protocol.Container{{Name: "db", MemoryBytes: 3 * gib}}}},
			Tart: &protocol.Tart{}, LMStudio: &protocol.LMStudio{}, Ollama: &protocol.Ollama{},
		},
		"docker VM stopped by Resource Saver": {
			Host:    &protocol.Host{TotalBytes: 64 * gib},
			Engines: []protocol.Engine{{Kind: protocol.EngineDockerDesktop, Running: true, VMLimitBytes: 31 * gib}},
			Tart:    &protocol.Tart{}, LMStudio: &protocol.LMStudio{}, Ollama: &protocol.Ollama{},
		},
		"engines, Podman VMs and other VMs": {
			Host: &protocol.Host{TotalBytes: 64 * gib, UsedBytes: u64(40 * gib), CompressorBytes: u64(gib), CompressedBytes: u64(3 * gib)},
			Engines: []protocol.Engine{
				{Kind: protocol.EngineDockerDesktop, Socket: "/Users/dev/.docker/run/docker.sock", Running: true, VMRunning: true,
					VMLimitBytes: 16 * gib, VMFootprintBytes: 6 * gib, Containers: []protocol.Container{{Name: "db", MemoryBytes: 5 * gib}}},
				{Kind: protocol.EnginePodman, Socket: "/Users/dev/.local/share/containers/podman/machine/dev.sock", Machine: "dev",
					Running: true, VMRunning: true, VMLimitBytes: 4 * gib, VMFootprintBytes: 3 * gib,
					Containers: []protocol.Container{{Name: "cache", MemoryBytes: gib}}},
				{Kind: protocol.EngineDocker, Socket: "/var/run/docker.sock"},
				{Kind: protocol.EngineDocker, Socket: "/Users/dev/.colima/docker.sock", Running: true, VMError: "unknown VM",
					Containers: []protocol.Container{{Name: "web", MemoryBytes: 2 * gib}}},
			},
			// dev's VM is its engine's; ci's has no engine, so it counts here.
			Podman: &protocol.Podman{VMs: []protocol.PodmanVM{{Machine: "dev", PID: 30, FootprintBytes: 3 * gib}, {Machine: "ci", PID: 31, FootprintBytes: 2 * gib, SwappedBytes: u64(gib)}}},
			VMs:    &protocol.VMs{Other: []protocol.OtherVM{{PID: 40, Name: "qemu-system-aarch64", FootprintBytes: 5 * gib, SwappedBytes: u64(gib)}}},
			Tart:   &protocol.Tart{Installed: true}, LMStudio: &protocol.LMStudio{}, Ollama: &protocol.Ollama{},
		},
		"podman and vms unknown": {
			Host:    &protocol.Host{TotalBytes: 64 * gib, UsedBytes: u64(18 * gib), CompressorBytes: u64(0), CompressedBytes: u64(0)},
			Sources: map[string]protocol.SourceStatus{"podman": {}, "vms": {}},
			Engines: []protocol.Engine{}, Tart: &protocol.Tart{}, LMStudio: &protocol.LMStudio{}, Ollama: &protocol.Ollama{},
		},
		"podman and vms read, none running": {
			Host:    &protocol.Host{TotalBytes: 64 * gib, UsedBytes: u64(18 * gib), CompressorBytes: u64(0), CompressedBytes: u64(0)},
			Sources: map[string]protocol.SourceStatus{"podman": {At: day0}, "vms": {At: day0}},
			Engines: []protocol.Engine{}, Tart: &protocol.Tart{}, LMStudio: &protocol.LMStudio{}, Ollama: &protocol.Ollama{},
		},
		"ollama models unknown": {
			Host:    &protocol.Host{TotalBytes: 64 * gib},
			Engines: []protocol.Engine{}, Tart: &protocol.Tart{}, LMStudio: &protocol.LMStudio{},
			Ollama: &protocol.Ollama{Installed: true, Running: true, ModelsError: "remote", FootprintBytes: u64(5 * gib)},
		},
	}
	for name, s := range snaps {
		t.Run(name, func(t *testing.T) {
			want := budget.Compute(s, p)
			s.Budget = &want
			got := budget.Compute(suggest.Snapshot(samples.FromSnapshot(s)), p)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("replayed budget\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

// warnAt makes the sample at index i of each day a warn onset, with the
// Docker VM holding footprint GiB then.
// Samples recorded before the Ollama source existed replay as Ollama not
// running, not as unknown: their unaccounted memory stays known.
func TestReplayOfASampleWithoutOllama(t *testing.T) {
	s := samples.Sample{Host: &samples.Host{TotalBytes: 64 * gib, UsedBytes: u64(20 * gib), CompressorBytes: u64(0), CompressedBytes: u64(0)},
		Budget: &protocol.Budget{}}
	b := budget.Compute(suggest.Snapshot(s), budget.Params{})
	if b.UnaccountedBytes == nil {
		t.Fatalf("unaccounted unknown; unknown = %v", b.Unknown)
	}
}

func TestMinHeadroomFromWarnOnsets(t *testing.T) {
	onset := map[int]uint64{100: 10, 200: 11, 300: 12, 400: 13}
	ss := threeDays(func(s *samples.Sample, i int) {
		s.Engines = desktopVM(0, 0) // running throughout: no VM start
		if fp, ok := onset[i]; ok {
			s.Host.Pressure, s.Host.FreePercent = "warn", 38
			s.Engines = desktopVM(0, fp*gib)
		}
	})
	r := aggregate(ss)
	v := value(t, r, "min_headroom_gb")
	// Replayed with the suggested 5 GiB baseline: headroom at the onsets is
	// 64 − 5 − 10..13 = 49..46 GiB. p90 49 + 1 GiB margin.
	if !v.OK || v.GB != 50 || !strings.Contains(v.Rule, "12 warn onsets") {
		t.Fatalf("min headroom = %+v", v)
	}
	if !strings.Contains(strings.Join(r.Advice, "\n"), "warn at a median of 38% free") {
		t.Errorf("advice = %v", r.Advice)
	}
}

func TestMinHeadroomNeedsOnsets(t *testing.T) {
	v := value(t, aggregate(threeDays(func(*samples.Sample, int) {})), "min_headroom_gb")
	if v.OK || !strings.Contains(v.Why, "warn") {
		t.Fatalf("min headroom = %+v", v)
	}
}

func TestAnOnsetNeedsANormalSampleJustBefore(t *testing.T) {
	// Warn right after a gap (the Mac slept) is not an onset: what came
	// before is unknown.
	ss := threeDays(func(s *samples.Sample, i int) {
		if i >= 100 {
			s.T = s.T.Add(time.Hour)
		}
		if i == 100 {
			s.Host.Pressure = "warn"
		}
	})
	if v := value(t, aggregate(ss), "min_headroom_gb"); v.OK {
		t.Fatalf("counted onsets after a gap: %+v", v)
	}
}

func TestCriticalOnsetsFromWarnAndFromNormal(t *testing.T) {
	ss := threeDays(func(s *samples.Sample, i int) {
		switch i {
		case 100: // normal → warn → critical
			s.Host.Pressure, s.Host.FreePercent = "warn", 38
		case 101:
			s.Host.Pressure, s.Host.FreePercent = "critical", 12
		case 200: // normal → critical
			s.Host.Pressure, s.Host.FreePercent = "critical", 12
		}
	})
	adv := strings.Join(aggregate(ss).Advice, "\n")
	if !strings.Contains(adv, "turned critical at a median of 12% free (6 times)") {
		t.Fatalf("advice = %s", adv)
	}
}

// A warn onset recorded on 2026-10-18 (docs/baseline.md, "Warn onsets"): 47%
// free, nothing swapping, two 16 GiB Tart VMs and the Docker VM at its
// high-water mark. Replayed with the host baseline suggest learned then
// (23 GiB), today's rule gives a negative headroom (#311). The sample has no
// per-VM compressed share, so the test gives one: the VMs hold all the
// compressor's content but what it held before they booted, 3 GB
// compressed (docs/baseline.md, "An episode looks the same each time"),
// split between them by footprint.
func TestReplayedOnsetHasHeadroomOnceVMCompressionIsCredited(t *testing.T) {
	raw, err := os.ReadFile("testdata/onset-2026-10-18.json")
	if err != nil {
		t.Fatal(err)
	}
	var s samples.Sample
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	p := config.Defaults("/tmp").BudgetParams()
	p.HostBaselineBytes = 23 * gib
	headroom := func(s samples.Sample) int64 {
		b := budget.Compute(suggest.Snapshot(s), p)
		if b.HeadroomBytes == nil {
			t.Fatalf("headroom unknown; unknown = %v", b.Unknown)
		}
		return *b.HeadroomBytes
	}
	before := headroom(s)
	if h := before; h >= 0 {
		t.Fatalf("uncredited headroom %.1f GB, want negative as recorded", float64(h)/1e9)
	}

	ratio := float64(*s.Host.CompressedBytes) / float64(*s.Host.CompressorBytes)
	inVMs := float64(*s.Host.CompressedBytes) - 3e9*ratio
	fp := float64(s.Engines[0].VMFootprintBytes)
	for _, vm := range s.TartVMs {
		fp += float64(*vm.FootprintBytes)
	}
	share := func(footprint uint64) *uint64 { return u64(uint64(float64(footprint) / fp * inVMs)) }
	s.Engines[0].VMSwappedBytes = share(s.Engines[0].VMFootprintBytes)
	for i, vm := range s.TartVMs {
		s.TartVMs[i].SwappedBytes = share(*vm.FootprintBytes)
	}
	h := headroom(s)
	if h < 0 || s.Host.FreePercent != 47 {
		t.Fatalf("credited headroom %.1f GB at %d%% free, want ≥ 0", float64(h)/1e9, s.Host.FreePercent)
	}
	t.Logf("headroom %.1f GB uncredited, %.1f GB credited", float64(before)/1e9, float64(h)/1e9)
}
