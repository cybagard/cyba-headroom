package suggest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/budget"
	"github.com/cybagard/cyba-headroom/internal/samples"
	"github.com/cybagard/cyba-headroom/internal/suggest"
)

func aggregate(ss ...[]samples.Sample) suggest.Result {
	a := suggest.New(suggest.Options{Interval: tick, Loc: time.UTC, Current: budget.Params{}})
	for _, s := range ss {
		for _, x := range s {
			a.Add(x)
		}
	}
	return a.Result()
}

func value(t *testing.T, r suggest.Result, key string) suggest.Value {
	t.Helper()
	for _, v := range r.Values {
		if v.Key == key {
			return v
		}
	}
	t.Fatalf("no value %q in %+v", key, r.Values)
	return suggest.Value{}
}

func TestDaysTable(t *testing.T) {
	idle := series(day0.Add(8*time.Hour), 720, func(_ int, at time.Time) samples.Sample {
		return sample(at, []string{"done"}, 5*gib)
	})
	two := series(day0.Add(10*time.Hour), 360, func(_ int, at time.Time) samples.Sample {
		s := sample(at, []string{"working"}, 5*gib)
		s.Worktrees = append(s.Worktrees, samples.Worktree{ID: "w2", Agents: []string{"working", "waiting"}})
		return s
	})
	r := aggregate(idle, two)
	if len(r.Days) != 1 {
		t.Fatalf("days = %+v", r.Days)
	}
	d := r.Days[0]
	if d.Date != "2026-10-08" || d.Samples != 1080 || d.Working != 360 || d.PeakWorking != 2 ||
		d.Hours != 1.5 || d.WorkingHours != 0.5 {
		t.Fatalf("day = %+v", d)
	}
}

func TestTooFewWorkingDaysMeansNoValues(t *testing.T) {
	r := aggregate(workingHours(0, 2, 5*gib), workingHours(1, 2, 5*gib))
	for _, v := range r.Values {
		if v.OK || !strings.Contains(v.Why, "not enough data") {
			t.Errorf("%s: %+v", v.Key, v)
		}
	}
	// A day with under an hour of work does not count toward the three.
	r = aggregate(workingHours(0, 2, 5*gib), workingHours(1, 2, 5*gib), workingHours(2, 0.5, 5*gib))
	if v := value(t, r, "host_baseline_gb"); v.OK {
		t.Fatalf("counted a half-hour day: %+v", v)
	}
}

func TestIdleSamplesAreLeftOut(t *testing.T) {
	work := append(append(workingHours(0, 2, 5*gib), workingHours(1, 2, 5*gib)...), workingHours(2, 2, 5*gib)...)
	// Idle evenings with far more unaccounted memory must not move the value.
	idle := series(day0.Add(20*time.Hour), 2000, func(_ int, at time.Time) samples.Sample {
		return sample(at, nil, 30*gib)
	})
	v := value(t, aggregate(work, idle), "host_baseline_gb")
	if !v.OK || v.GB != 5 {
		t.Fatalf("baseline = %+v, want 5 from working samples only", v)
	}
}

// threeDays is three days of 2 h work each, built by f from the plain sample.
func threeDays(f func(s *samples.Sample, i int)) []samples.Sample {
	var out []samples.Sample
	for d := 0; d < 3; d++ {
		for i, s := range workingHours(d, 2, 5*gib) {
			f(&s, i)
			out = append(out, s)
		}
	}
	return out
}

func TestHostBaselineIsP95RoundedUp(t *testing.T) {
	// 90% of samples at 5 GiB, 10% at 9.2 GiB: p95 is 9.2, rounded up to 9.5.
	ss := threeDays(func(s *samples.Sample, i int) {
		if i%10 == 0 {
			un := int64(9*gib + gib/5)
			s.Budget.UnaccountedBytes = &un
		}
	})
	v := value(t, aggregate(ss), "host_baseline_gb")
	if !v.OK || v.GB != 9.5 || !strings.Contains(v.Rule, "p95") {
		t.Fatalf("baseline = %+v", v)
	}
	// Dropping the 9.2 GiB samples entirely gives 5.
	ss = threeDays(func(*samples.Sample, int) {})
	if v := value(t, aggregate(ss), "host_baseline_gb"); v.GB != 5 {
		t.Fatalf("baseline = %+v", v)
	}
}

func TestSwapOutsAndStaleInputsAreLeftOut(t *testing.T) {
	ss := threeDays(func(s *samples.Sample, i int) {
		switch i % 10 {
		case 0: // swapping out: footprints may count swapped pages
			s.Host.SwapoutsPerSec = f64(50)
			un := int64(1 * gib)
			s.Budget.UnaccountedBytes = &un
		case 1: // stale Docker reading
			s.Stale = map[string]bool{"docker": true}
			un := int64(40 * gib)
			s.Budget.UnaccountedBytes = &un
		}
	})
	r := aggregate(ss)
	if v := value(t, r, "host_baseline_gb"); v.GB != 5 {
		t.Fatalf("baseline = %+v", v)
	}
	if r.SwappingOut == 0 || r.Stale == 0 {
		t.Fatalf("left-out counts: swapping %d, stale %d", r.SwappingOut, r.Stale)
	}
}

func TestDockerOverhead(t *testing.T) {
	ss := threeDays(func(s *samples.Sample, i int) {
		s.Docker = &samples.Docker{VMRunning: true, VMLimitBytes: 31 * gib, VMFootprintBytes: 4 * gib}
		s.Containers = []samples.Container{{Name: "db", MemoryBytes: 2*gib + gib/2}}
		if i%50 == 0 { // image pull: page cache spikes
			s.Docker.VMFootprintBytes = 6 * gib
		}
	})
	v := value(t, aggregate(ss), "docker_overhead_gb")
	// Footprint − containers = 1.5 GiB (3.5 GiB 2% of the time): p95 = 1.5.
	if !v.OK || v.GB != 1.5 || !strings.Contains(v.Rule, "p50 1.5") || !strings.Contains(v.Rule, "max 3.5") {
		t.Fatalf("docker overhead = %+v", v)
	}
	// Under an hour of the VM running is not enough.
	short := threeDays(func(s *samples.Sample, i int) {
		if i < 200 { // 3 × 200 samples = 50 min
			s.Docker = &samples.Docker{VMRunning: true, VMFootprintBytes: 4 * gib}
		}
	})
	if v := value(t, aggregate(short), "docker_overhead_gb"); v.OK || !strings.Contains(v.Why, "not enough data") {
		t.Fatalf("docker overhead = %+v", v)
	}
}

func TestLMStudioIdle(t *testing.T) {
	ss := threeDays(func(s *samples.Sample, i int) {
		s.LMStudio = &samples.LMStudio{FootprintBytes: u64(gib / 2)}
		if i%2 == 0 { // a model loaded: not the idle footprint
			s.LMStudio.Models = []samples.Model{{Key: "m", SizeBytes: 8 * gib}}
			s.LMStudio.FootprintBytes = u64(9 * gib)
		}
	})
	v := value(t, aggregate(ss), "lmstudio_idle_gb")
	if !v.OK || v.GB != 0.5 {
		t.Fatalf("lmstudio idle = %+v", v)
	}
	if v := value(t, aggregate(threeDays(func(*samples.Sample, int) {})), "lmstudio_idle_gb"); v.OK {
		t.Fatalf("no LM Studio at all: %+v", v)
	}
}

func TestPerWorktreeCap(t *testing.T) {
	ss := threeDays(func(s *samples.Sample, i int) {
		d := uint64(s.T.Sub(day0).Hours() / 24)
		s.Worktrees = []samples.Worktree{
			{ID: "a", Path: "/Users/dev/w/project-a", Agents: []string{"working"}},
			{ID: "b", Path: "/Users/dev/w/project-b", Agents: []string{"waiting"}},
		}
		// project-a peaks at 2, 3, 4 GiB of containers on days 0..2, but
		// only in the last sample of each day.
		mem := gib
		if i == 1439 {
			mem = (2 + d) * gib
		}
		s.Containers = []samples.Container{
			{Name: "db", MemoryBytes: mem, Mounts: []string{"/Users/dev/w/project-a/data"}},
			{Name: "stray", MemoryBytes: 30 * gib}, // unattributed: not a worktree's
		}
		s.TartVMs = []samples.TartVM{{Name: "ci", MemoryBytes: 8 * gib, LaunchCwd: "/Users/dev/w/project-b"}}
	})
	v := value(t, aggregate(ss), "per_worktree_cap_gb")
	// Worktree-day peaks: a 2, 3, 4; b 8, 8, 8. p95 8 × 1.25 = 10.
	if !v.OK || v.GB != 10 || !strings.Contains(v.Rule, "6 worktree-days") || !strings.Contains(v.Rule, "max 8.0") {
		t.Fatalf("cap = %+v", v)
	}
}

func TestPerWorktreeCapNeedsWorktreeDays(t *testing.T) {
	v := value(t, aggregate(threeDays(func(*samples.Sample, int) {})), "per_worktree_cap_gb")
	if v.OK || !strings.Contains(v.Why, "worktree-days") {
		t.Fatalf("cap = %+v", v)
	}
}

func advice(r suggest.Result) string { return strings.Join(r.Advice, "\n") }

func TestDockerLimitAdvice(t *testing.T) {
	withVM := func(limit uint64) []samples.Sample {
		return threeDays(func(s *samples.Sample, _ int) {
			s.Docker = &samples.Docker{VMRunning: true, VMLimitBytes: limit, VMFootprintBytes: 4 * gib}
		})
	}
	if a := advice(aggregate(withVM(31 * gib))); !strings.Contains(a, "Docker Desktop's memory limit is 31 GB") || !strings.Contains(a, "lowering it to 6 GB") {
		t.Fatalf("advice = %s", a)
	}
	if a := advice(aggregate(withVM(8 * gib))); strings.Contains(a, "Docker Desktop") {
		t.Fatalf("advice for a fitting limit: %s", a)
	}
}

func TestIdleModelWithoutTTLAdvice(t *testing.T) {
	model := func(ttl *time.Duration, status string) []samples.Sample {
		return threeDays(func(s *samples.Sample, _ int) {
			last := s.T.Add(-2 * time.Hour) // unused for 2 h at every sample
			s.LMStudio = &samples.LMStudio{FootprintBytes: u64(13 * gib), Models: []samples.Model{
				{Key: "big-model", SizeBytes: 12 * gib, Status: status, TTL: ttl, LastUsedAt: &last}}}
		})
	}
	if a := advice(aggregate(model(nil, "idle"))); !strings.Contains(a, `"big-model" (12.0 GB) sat loaded and unused`) || !strings.Contains(a, "set a TTL") {
		t.Fatalf("advice = %s", a)
	}
	hour := time.Hour
	if a := advice(aggregate(model(&hour, "idle"))); strings.Contains(a, "big-model") {
		t.Fatalf("advice despite a TTL: %s", a)
	}
	if a := advice(aggregate(model(nil, "generating"))); strings.Contains(a, "big-model") {
		t.Fatalf("advice for a busy model: %s", a)
	}
}

func TestConcurrencyAdvice(t *testing.T) {
	ss := threeDays(func(s *samples.Sample, i int) {
		s.Worktrees = []samples.Worktree{{ID: "a", Agents: []string{"working", "working", "working"}}}
		if i == 100 { // four working agents, and pressure turns
			s.Worktrees[0].Agents = append(s.Worktrees[0].Agents, "working")
			s.Host.Pressure = "warn"
		}
	})
	a := advice(aggregate(ss))
	if !strings.Contains(a, "Up to 3 agents worked in parallel at normal pressure") || !strings.Contains(a, "a median of 4 agents working") {
		t.Fatalf("advice = %s", a)
	}
}
