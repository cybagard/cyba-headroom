package suggest_test

import (
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/samples"
)

const gib = uint64(1 << 30)

func u64(v uint64) *uint64   { return &v }
func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

// day0 is local midnight of the first synthetic day (UTC keeps tests stable).
var day0 = time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)

const tick = 5 * time.Second

// sample is a 64 GiB Mac at normal pressure with one worktree; agents are
// the agents' states, unaccounted the memory no component explains.
func sample(at time.Time, agents []string, unaccounted uint64) samples.Sample {
	used := 20 * gib
	un := int64(unaccounted)
	return samples.Sample{
		V: samples.Version, T: at,
		Host: &samples.Host{TotalBytes: 64 * gib, UsedBytes: &used, CompressorBytes: u64(0), CompressedBytes: u64(0),
			Pressure: "normal", FreePercent: 70, SwapoutsPerSec: f64(0)},
		Budget: &protocol.Budget{TotalBytes: 64 * gib, ReservedBytes: 10 * gib, HeadroomBytes: i64(int64(54 * gib)),
			UnaccountedBytes: &un, Components: []protocol.BudgetComponent{{Name: "host_baseline"}}},
		Worktrees: []samples.Worktree{{ID: "w1", Path: "/Users/dev/w/project-a", Name: "A", Agents: agents}},
	}
}

// series returns n samples a tick apart from start, built by f.
func series(start time.Time, n int, f func(i int, at time.Time) samples.Sample) []samples.Sample {
	out := make([]samples.Sample, n)
	for i := range out {
		at := start.Add(time.Duration(i) * tick)
		out[i] = f(i, at)
	}
	return out
}

// workingHours is h hours of working samples on day d with unaccounted u.
func workingHours(d int, h float64, u uint64) []samples.Sample {
	n := int(h * float64(time.Hour/tick))
	return series(day0.AddDate(0, 0, d).Add(9*time.Hour), n, func(_ int, at time.Time) samples.Sample {
		return sample(at, []string{"working"}, u)
	})
}
