package suggest_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/budget"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/samples"
	"github.com/cybagard/cyba-headroom/internal/suggest"
)

// A snapshot recorded as a sample and replayed gives the budget the daemon
// computed from the original.
func TestReplayGivesTheDaemonsBudget(t *testing.T) {
	p := budget.Params{HostBaselineBytes: 8 * gib, DockerOverheadBytes: 2 * gib, LMStudioIdleBytes: gib / 2}
	snaps := map[string]*protocol.Snapshot{
		"everything running": {
			Host: &protocol.Host{TotalBytes: 64 * gib, UsedBytes: u64(30 * gib), CompressorBytes: u64(gib), CompressedBytes: u64(3 * gib)},
			Docker: &protocol.Docker{Running: true, VMRunning: true, VMLimitBytes: 31 * gib, VMFootprintBytes: 6 * gib,
				Containers: []protocol.Container{{Name: "db", MemoryBytes: 5 * gib}}},
			Tart: &protocol.Tart{Installed: true, VMs: []protocol.TartVM{{Name: "ci", MemoryBytes: 8 * gib, FootprintBytes: u64(9 * gib)}}},
			LMStudio: &protocol.LMStudio{Installed: true, Running: true, FootprintBytes: u64(13 * gib),
				Models: []protocol.LoadedModel{{Key: "m", SizeBytes: 12 * gib}}},
		},
		"nothing running": {
			Host:   &protocol.Host{TotalBytes: 64 * gib, UsedBytes: u64(18 * gib), CompressorBytes: u64(0), CompressedBytes: u64(0)},
			Docker: &protocol.Docker{}, Tart: &protocol.Tart{Installed: true}, LMStudio: &protocol.LMStudio{Installed: true},
		},
		"sources unknown": {
			Host: &protocol.Host{TotalBytes: 64 * gib},
		},
		"docker VM unreadable": {
			Host: &protocol.Host{TotalBytes: 64 * gib},
			Docker: &protocol.Docker{Running: true, VMLimitBytes: 31 * gib, VMError: "lsof: timed out",
				Containers: []protocol.Container{{Name: "db", MemoryBytes: 3 * gib}}},
			Tart: &protocol.Tart{}, LMStudio: &protocol.LMStudio{},
		},
		"docker VM stopped by Resource Saver": {
			Host:   &protocol.Host{TotalBytes: 64 * gib},
			Docker: &protocol.Docker{Running: true, VMLimitBytes: 31 * gib},
			Tart:   &protocol.Tart{}, LMStudio: &protocol.LMStudio{},
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
func TestMinHeadroomFromWarnOnsets(t *testing.T) {
	onset := map[int]uint64{100: 10, 200: 11, 300: 12, 400: 13}
	ss := threeDays(func(s *samples.Sample, i int) {
		if fp, ok := onset[i]; ok {
			s.Host.Pressure, s.Host.FreePercent = "warn", 38
			s.Docker = &samples.Docker{VMRunning: true, VMFootprintBytes: fp * gib}
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
