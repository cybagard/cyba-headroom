// Package suggest learns headroom's thresholds from recorded samples (#23):
// what the host really needs outside every component, what the Docker VM and
// LM Studio cost beyond their payload, how much headroom was left when
// memory pressure turned, and how much one worktree uses at its peak.
//
// Only working samples count: at least one agent working and no stale budget
// input. A value with too little evidence is reported as such, never guessed.
package suggest

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/cybagard/cyba-headroom/internal/attribution"
	"github.com/cybagard/cyba-headroom/internal/budget"
	"github.com/cybagard/cyba-headroom/internal/samples"
)

// Options configure an Aggregator.
type Options struct {
	// Interval is the daemon's sample interval: one sample stands for this
	// much time.
	Interval time.Duration
	// Loc is the time zone days are counted in.
	Loc *time.Location
	// Current are the budget parameters in use, for values that cannot be
	// learned yet.
	Current budget.Params
}

// Minimum evidence.
const (
	minWorkingDays    = 3
	minWorkingPerDay  = time.Hour
	minDockerTime     = time.Hour
	minLMIdleTime     = 10 * time.Minute
	minWarnOnsets     = 3
	minWorktreeDays   = 5
	headroomMarginGiB = 1.0
	idleModelAfter    = time.Hour
)

// Day summarises one local day of samples.
type Day struct {
	Date         string
	Samples      int
	Working      int
	Hours        float64
	WorkingHours float64
	// PeakWorking is the most agents working at once.
	PeakWorking int
	WarnMinutes float64
	CritMinutes float64
	PeakSwap    uint64
}

// Value is one suggested setting.
type Value struct {
	Section, Key string
	// OK is false when there was too little evidence; Why says what is
	// missing, and GB is meaningless.
	OK  bool
	GB  float64
	Why string
	// Rule says how GB was derived, with the evidence behind it.
	Rule string
}

// Result is what the samples suggest.
type Result struct {
	Days   []Day
	Values []Value
	Advice []string
	// Samples left out of the budget maths, and why.
	Stale, SwappingOut int
}

// Aggregator streams samples and keeps what the suggestions need.
type Aggregator struct {
	o    Options
	days map[string]*Day
	// order keeps days in the order first seen.
	order []string

	unaccounted     []float64
	stale, swapping int
	// Docker VM footprint − containers while the VM ran.
	dockerExtra []float64
	// LM Studio's footprint with no model loaded.
	lmIdle []float64

	// prev is the previous sample, for pressure onsets.
	prev *samples.Sample
	// Samples at which pressure turned warn, or critical, from normal.
	warnOnsets, critOnsets []samples.Sample

	// worktreePeak is each worktree's peak use (containers + Tart VMs'
	// configured memory) per day.
	worktreePeak map[worktreeDay]uint64

	// For advice: the Docker VM's footprint and largest limit, each model's
	// idle-and-loaded time per day, and the most agents working at normal
	// pressure.
	dockerFootprint []float64
	dockerLimit     uint64
	idleModel       map[modelDay]time.Duration
	modelSize       map[string]uint64
	maxCalmWorking  int
}

type modelDay struct{ key, date string }

type worktreeDay struct{ id, date string }

// New returns an empty Aggregator.
func New(o Options) *Aggregator {
	if o.Loc == nil {
		o.Loc = time.Local
	}
	return &Aggregator{o: o, days: map[string]*Day{}, worktreePeak: map[worktreeDay]uint64{},
		idleModel: map[modelDay]time.Duration{}, modelSize: map[string]uint64{}}
}

// budgetSources are the inputs to the budget; a stale one makes a sample
// unfit for budget maths.
var budgetSources = []string{"host", "docker", "tart", "lmstudio"}

// Add takes one sample.
func (a *Aggregator) Add(s samples.Sample) {
	onset := a.onset(s)
	a.prev = &s
	day := a.day(s.T)
	day.Samples++
	working := countWorking(s)
	if working > 0 {
		day.Working++
	}
	day.PeakWorking = max(day.PeakWorking, working)
	if h := s.Host; h != nil {
		switch h.Pressure {
		case "warn":
			day.WarnMinutes += a.o.Interval.Minutes()
		case "critical":
			day.CritMinutes += a.o.Interval.Minutes()
		}
		day.PeakSwap = max(day.PeakSwap, h.SwapUsedBytes)
	}
	if working == 0 {
		return
	}
	if slices.ContainsFunc(budgetSources, func(n string) bool { return s.Stale[n] }) {
		a.stale++
		return
	}
	switch onset {
	case "warn":
		a.warnOnsets = append(a.warnOnsets, s)
	case "critical":
		a.warnOnsets = append(a.warnOnsets, s)
		a.critOnsets = append(a.critOnsets, s)
	case "critical-from-warn":
		a.critOnsets = append(a.critOnsets, s)
	}
	if s.Host != nil && s.Host.Pressure == "normal" {
		a.maxCalmWorking = max(a.maxCalmWorking, working)
	}
	if l := s.LMStudio; l != nil {
		for _, m := range l.Models {
			// Loaded, not busy, unused for over an hour, and nothing will
			// unload it.
			if m.TTL == nil && m.Status != "generating" && m.LastUsedAt != nil && s.T.Sub(*m.LastUsedAt) > idleModelAfter {
				a.idleModel[modelDay{m.Key, day.Date}] += a.o.Interval
				a.modelSize[m.Key] = m.SizeBytes
			}
		}
	}
	if d := s.Docker; d != nil && d.VMRunning {
		a.dockerFootprint = append(a.dockerFootprint, float64(d.VMFootprintBytes))
		a.dockerLimit = max(a.dockerLimit, d.VMLimitBytes)
		var payload uint64
		for _, c := range s.Containers {
			payload += c.MemoryBytes
		}
		a.dockerExtra = append(a.dockerExtra, max(float64(d.VMFootprintBytes)-float64(payload), 0))
	}
	if l := s.LMStudio; l != nil && len(l.Models) == 0 && l.FootprintBytes != nil {
		a.lmIdle = append(a.lmIdle, float64(*l.FootprintBytes))
	}
	a.addWorktreeUse(s, day.Date)
	if s.Budget != nil && s.Budget.UnaccountedBytes != nil {
		// Footprints may count pages swapped out to disk, so unaccounted
		// memory reads low while swap-outs run.
		if h := s.Host; h != nil && h.SwapoutsPerSec != nil && *h.SwapoutsPerSec > 0 {
			a.swapping++
		} else {
			a.unaccounted = append(a.unaccounted, float64(*s.Budget.UnaccountedBytes))
		}
	}
}

// onset reports whether pressure rose with s: "warn" when it left normal,
// "critical" when it jumped from normal to critical (both onsets at once),
// "critical-from-warn" when it went on from warn to critical. Only a
// sample right after the previous one counts; after a gap, such as sleep,
// what came before is unknown.
func (a *Aggregator) onset(s samples.Sample) string {
	p := a.prev
	if p == nil || p.Host == nil || s.Host == nil || s.T.Sub(p.T) > 3*a.o.Interval {
		return ""
	}
	switch {
	case p.Host.Pressure == "normal" && s.Host.Pressure == "critical":
		return "critical"
	case p.Host.Pressure == "normal" && s.Host.Pressure == "warn":
		return "warn"
	case p.Host.Pressure == "warn" && s.Host.Pressure == "critical":
		return "critical-from-warn"
	}
	return ""
}

func (a *Aggregator) day(t time.Time) *Day {
	date := t.In(a.o.Loc).Format("2006-01-02")
	d, ok := a.days[date]
	if !ok {
		d = &Day{Date: date}
		a.days[date] = d
		a.order = append(a.order, date)
	}
	return d
}

func countWorking(s samples.Sample) int {
	n := 0
	for _, w := range s.Worktrees {
		for _, st := range w.Agents {
			if st == "working" {
				n++
			}
		}
	}
	return n
}

// Result computes the suggestions from everything added so far.
func (a *Aggregator) Result() Result {
	r := Result{Stale: a.stale, SwappingOut: a.swapping}
	workDays := 0
	for _, date := range a.sortedDays() {
		d := *a.days[date]
		d.Hours = hours(d.Samples, a.o.Interval)
		d.WorkingHours = hours(d.Working, a.o.Interval)
		r.Days = append(r.Days, d)
		if time.Duration(float64(d.Working)*float64(a.o.Interval)) >= minWorkingPerDay {
			workDays++
		}
	}
	enough := workDays >= minWorkingDays
	gate := func(v Value) Value {
		if !enough {
			v.OK = false
			v.Why = fmt.Sprintf("not enough data: %d day(s) with ≥ 1 h of working agents, need %d", workDays, minWorkingDays)
		}
		return v
	}

	r.Values = append(r.Values, gate(a.hostBaseline()), gate(a.dockerOverhead()), gate(a.lmStudioIdle()))
	r.Values = append(r.Values, gate(a.minHeadroom(a.params(r.Values))), gate(a.perWorktreeCap()))
	r.Advice = append(r.Advice, a.pressureAdvice()...)
	r.Advice = append(r.Advice, a.concurrencyAdvice()...)
	r.Advice = append(r.Advice, a.dockerAdvice()...)
	r.Advice = append(r.Advice, a.modelAdvice()...)
	return r
}

// params are the budget parameters the suggestions make: learned values
// where there was enough evidence, the current ones otherwise.
func (a *Aggregator) params(vs []Value) budget.Params {
	p := a.o.Current
	for _, v := range vs {
		if !v.OK {
			continue
		}
		b := uint64(v.GB * (1 << 30))
		switch v.Key {
		case "host_baseline_gb":
			p.HostBaselineBytes = b
		case "docker_overhead_gb":
			p.DockerOverheadBytes = b
		case "lmstudio_idle_gb":
			p.LMStudioIdleBytes = b
		}
	}
	return p
}

// minHeadroom replays each warn onset with the suggested budget: the
// headroom the new config would have shown when pressure turned. A floor at
// their p90, plus a margin, would have held most of them off.
func (a *Aggregator) minHeadroom(p budget.Params) Value {
	v := Value{Section: "policy", Key: "min_headroom_gb"}
	if n := len(a.warnOnsets); n < minWarnOnsets {
		v.Why = fmt.Sprintf("not enough data: pressure turned warn %d time(s) while agents worked, need %d; no evidence for a floor yet", n, minWarnOnsets)
		return v
	}
	var hr []float64
	for _, s := range a.warnOnsets {
		b := budget.Compute(Snapshot(s), p)
		if b.HeadroomBytes != nil {
			hr = append(hr, float64(*b.HeadroomBytes))
		}
	}
	if len(hr) < minWarnOnsets {
		v.Why = "not enough data: host memory unknown at the warn onsets"
		return v
	}
	p90 := percentile(hr, 90) / (1 << 30)
	v.OK, v.GB = true, roundUp(max(p90, 0)+headroomMarginGiB, 0.5)
	v.Rule = fmt.Sprintf("p90 headroom at %d warn onsets, replayed with the suggested budget (%.1f GB), + %.0f GB margin",
		len(hr), p90, headroomMarginGiB)
	return v
}

// concurrencyAdvice reports how many agents worked in parallel before
// pressure turned.
func (a *Aggregator) concurrencyAdvice() []string {
	var out []string
	if a.maxCalmWorking > 0 {
		out = append(out, fmt.Sprintf("Up to %s worked in parallel at normal pressure.", plural(a.maxCalmWorking, "agent")))
	}
	if len(a.warnOnsets) > 0 {
		n := make([]float64, len(a.warnOnsets))
		for i, s := range a.warnOnsets {
			n[i] = float64(countWorking(s))
		}
		out = append(out, fmt.Sprintf("Pressure turned warn with a median of %s working.", plural(int(percentile(n, 50)), "agent")))
	}
	return out
}

// dockerAdvice suggests a smaller Docker Desktop VM when its limit is far
// above what it used. headroom never changes it.
func (a *Aggregator) dockerAdvice() []string {
	if a.span(len(a.dockerFootprint)) < minDockerTime || a.dockerLimit == 0 {
		return nil
	}
	p99 := percentile(a.dockerFootprint, 99)
	if float64(a.dockerLimit) <= 2*p99 {
		return nil
	}
	to := max(roundUp(p99*1.5/(1<<30), 1), 2)
	return []string{fmt.Sprintf("Docker Desktop's memory limit is %.0f GB; its VM used at most %.1f GB (p99) while agents worked. Consider lowering it to %.0f GB in Docker Desktop's settings.",
		float64(a.dockerLimit)/(1<<30), p99/(1<<30), to)}
}

// modelAdvice names LM Studio models that sat loaded and unused for over an
// hour of working time on at least two days, with no TTL to unload them.
func (a *Aggregator) modelAdvice() []string {
	days := map[string]int{}
	total := map[string]time.Duration{}
	for k, d := range a.idleModel {
		total[k.key] += d
		if d >= time.Hour {
			days[k.key]++
		}
	}
	keys := make([]string, 0, len(days))
	for k, n := range days {
		if n >= 2 {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var out []string
	for _, k := range keys {
		out = append(out, fmt.Sprintf("LM Studio: %q (%.1f GB) sat loaded and unused for %.1f h on %d days with no TTL; set a TTL so it unloads when idle.",
			k, float64(a.modelSize[k])/(1<<30), total[k].Hours(), days[k]))
	}
	return out
}

// pressureAdvice reports the free % at which the kernel changed level.
func (a *Aggregator) pressureAdvice() []string {
	var out []string
	for _, o := range []struct {
		level string
		ss    []samples.Sample
	}{{"warn", a.warnOnsets}, {"critical", a.critOnsets}} {
		var free []float64
		for _, s := range o.ss {
			if s.Host != nil {
				free = append(free, float64(s.Host.FreePercent))
			}
		}
		if len(free) > 0 {
			out = append(out, fmt.Sprintf("Memory pressure turned %s at a median of %.0f%% free (%d times).", o.level, percentile(free, 50), len(free)))
		}
	}
	return out
}

// addWorktreeUse attributes s's containers and VMs to its worktrees with the
// same rules the daemon uses (#20), and keeps each worktree's daily peak.
func (a *Aggregator) addWorktreeUse(s samples.Sample, date string) {
	if len(s.Worktrees) == 0 {
		return
	}
	wts := make([]attribution.Worktree, len(s.Worktrees))
	for i, w := range s.Worktrees {
		wts[i] = attribution.Worktree{ID: w.ID, Path: w.Path}
	}
	m := attribution.NewMatcher(wts)
	use := map[string]uint64{}
	for _, c := range s.Containers {
		if r := m.Match(attribution.Keys{ComposeDir: c.ComposeDir, Mounts: c.Mounts}); r.WorktreeID != "" {
			use[r.WorktreeID] += c.MemoryBytes
		}
	}
	for _, vm := range s.TartVMs {
		if r := m.Match(attribution.Keys{LaunchCwd: vm.LaunchCwd, SharedDirs: vm.SharedDirs, VMName: vm.Name}); r.WorktreeID != "" {
			use[r.WorktreeID] += vm.MemoryBytes
		}
	}
	for id, b := range use {
		k := worktreeDay{id, date}
		a.worktreePeak[k] = max(a.worktreePeak[k], b)
	}
}

// perWorktreeCap is the p95 of worktrees' daily peaks, with a quarter on top
// so a typical peak day passes.
func (a *Aggregator) perWorktreeCap() Value {
	v := Value{Section: "policy", Key: "per_worktree_cap_gb"}
	if n := len(a.worktreePeak); n < minWorktreeDays {
		v.Why = fmt.Sprintf("not enough data: %d worktree-days with containers or VMs while agents worked, need %d", n, minWorktreeDays)
		return v
	}
	peaks := make([]float64, 0, len(a.worktreePeak))
	for _, b := range a.worktreePeak {
		peaks = append(peaks, float64(b))
	}
	_, p95, top := dist(peaks)
	v.OK, v.GB = true, roundUp(p95*1.25, 1)
	v.Rule = fmt.Sprintf("p95 of daily per-worktree peaks (containers + Tart VMs) × 1.25 (%d worktree-days; p95 %.1f, max %.1f GB)",
		len(peaks), p95, top)
	return v
}

// dockerOverhead is the p95 of what the Docker VM costs beyond its
// containers: guest kernel, dockerd, page cache from image pulls.
func (a *Aggregator) dockerOverhead() Value {
	v := Value{Section: "budget", Key: "docker_overhead_gb"}
	if have := a.span(len(a.dockerExtra)); have < minDockerTime {
		v.Why = fmt.Sprintf("not enough data: the Docker VM ran for %s of working time, need %s", have, minDockerTime)
		return v
	}
	p50, p95, top := dist(a.dockerExtra)
	v.OK, v.GB = true, roundUp(p95, 0.1)
	v.Rule = fmt.Sprintf("p95 of Docker VM footprint − containers while agents worked (%d samples; p50 %.1f, p95 %.1f, max %.1f GB)",
		len(a.dockerExtra), p50, p95, top)
	return v
}

// lmStudioIdle is the p95 of LM Studio's footprint with no model loaded.
func (a *Aggregator) lmStudioIdle() Value {
	v := Value{Section: "budget", Key: "lmstudio_idle_gb"}
	if have := a.span(len(a.lmIdle)); have < minLMIdleTime {
		v.Why = fmt.Sprintf("not enough data: LM Studio ran with no model loaded for %s of working time, need %s", have, minLMIdleTime)
		return v
	}
	p50, p95, top := dist(a.lmIdle)
	v.OK, v.GB = true, roundUp(p95, 0.1)
	v.Rule = fmt.Sprintf("p95 of LM Studio's footprint with no model loaded (%d samples; p50 %.1f, max %.1f GB)", len(a.lmIdle), p50, top)
	return v
}

// span is how long n samples cover.
func (a *Aggregator) span(n int) time.Duration { return time.Duration(n) * a.o.Interval }

// dist returns the p50, p95 and maximum of xs, in GB.
func dist(xs []float64) (p50, p95, top float64) {
	g := func(b float64) float64 { return b / (1 << 30) }
	return g(percentile(xs, 50)), g(percentile(xs, 95)), g(xs[len(xs)-1])
}

// hostBaseline is the p95 of memory in use outside every component.
func (a *Aggregator) hostBaseline() Value {
	v := Value{Section: "budget", Key: "host_baseline_gb"}
	if len(a.unaccounted) == 0 {
		v.Why = "not enough data: no working sample with a known unaccounted figure"
		return v
	}
	p := percentile(a.unaccounted, 95)
	v.OK, v.GB = true, roundUp(max(p, 0)/(1<<30), 0.5)
	v.Rule = fmt.Sprintf("p95 of memory in use outside every component while agents worked (%d samples; %d left out during swap-outs)",
		len(a.unaccounted), a.swapping)
	return v
}

func (a *Aggregator) sortedDays() []string {
	out := slices.Clone(a.order)
	slices.Sort(out)
	return out
}

func hours(n int, interval time.Duration) float64 {
	return math.Round(float64(n)*interval.Hours()*100) / 100
}

// percentile is the p-th percentile (nearest rank) of xs; xs is sorted in
// place.
func percentile(xs []float64, p float64) float64 {
	slices.Sort(xs)
	i := int(math.Ceil(p/100*float64(len(xs)))) - 1
	return xs[max(i, 0)]
}

// roundUp rounds x up to a multiple of step.
func roundUp(x, step float64) float64 {
	return math.Ceil(x/step-1e-9) * step
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
