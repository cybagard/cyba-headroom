// Package host collects host memory pressure and swap without sudo (R1).
package host

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// Sysctl reads kernel values by name. The darwin implementation is System.
type Sysctl interface {
	Uint32(name string) (uint32, error)
	Uint64(name string) (uint64, error)
	Raw(name string) ([]byte, error)
}

// minTrendSpan is the history needed before a direction is reported; over a
// shorter span a 1-point change in the integer free % reads as a steep slope.
const minTrendSpan = time.Minute

// steadySlope is the |free %/min| below which pressure counts as steady.
// A starting guess; calibrate from the #23 baseline.
const steadySlope = 1.0

// Source is the host memory source. It keeps state between samples, so the
// daemon must not call Collect concurrently (it does not: see daemon.Tick).
type Source struct {
	sys    Sysctl
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	prev    *counters
	history []sample
}

// sample is one pressure reading kept for the trend.
type sample struct {
	at    time.Time
	level uint32 // 1, 2, 4: ordered by severity
	free  int
}

// counters are the cumulative swap counters at one sample. ok is false when
// they cannot be read: macOS 15 has no vm.compressor.swapper.* totals, and a
// future release may change their size. They are optional, so no error fails
// the rest of the reading.
type counters struct {
	at                time.Time
	ok                bool
	swapins, swapouts uint64
}

func (s *Source) readCounters(at time.Time) counters {
	c := counters{at: at, ok: true}
	var err error
	if c.swapins, err = s.sys.Uint64("vm.compressor.swapper.swapins_total"); err == nil {
		c.swapouts, err = s.sys.Uint64("vm.compressor.swapper.swapouts_total")
	}
	if err != nil {
		return counters{at: at}
	}
	return c
}

// New returns a source reading sys, keeping window of trend history.
func New(sys Sysctl, window time.Duration, now func() time.Time) *Source {
	return &Source{sys: sys, window: window, now: now}
}

// Name implements daemon.Source.
func (s *Source) Name() string { return "host" }

// Collect implements daemon.Source.
func (s *Source) Collect(context.Context) (daemon.Reading, error) {
	level, err := s.sys.Uint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return nil, err
	}
	free, err := s.sys.Uint32("kern.memorystatus_level")
	if err != nil {
		return nil, err
	}
	total, err := s.sys.Uint64("hw.memsize")
	if err != nil {
		return nil, err
	}
	raw, err := s.sys.Raw("vm.swapusage")
	if err != nil {
		return nil, err
	}
	swapTotal, swapUsed, err := decodeSwapUsage(raw)
	if err != nil {
		return nil, err
	}
	// Wall clock, not Go's monotonic reading: on darwin the monotonic clock
	// stops while the Mac sleeps, so old samples would survive a night asleep.
	cur := s.readCounters(s.now().Round(0))

	h := protocol.Host{
		TotalBytes:     total,
		Pressure:       pressureName(level),
		FreePercent:    int(free),
		SwapTotalBytes: swapTotal,
		SwapUsedBytes:  swapUsed,
		UsedBytes:      s.memoryUsed(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prev != nil && s.prev.ok && cur.ok {
		dt := cur.at.Sub(s.prev.at).Seconds()
		h.SwapinsPerSec = rate(s.prev.swapins, cur.swapins, dt)
		h.SwapoutsPerSec = rate(s.prev.swapouts, cur.swapouts, dt)
	}
	s.prev = &cur
	s.record(sample{at: cur.at, level: level, free: int(free)})
	h.Trend = s.trend()
	return reading{h}, nil
}

// record appends a sample and drops those older than the window.
func (s *Source) record(x sample) {
	s.history = append(s.history, x)
	cutoff := x.at.Add(-s.window)
	i := 0
	for i < len(s.history) && !s.history[i].at.After(cutoff) {
		i++
	}
	s.history = s.history[i:]
}

// trend summarises the history. It is never empty: record runs first.
func (s *Source) trend() protocol.Trend {
	first, last := s.history[0], s.history[len(s.history)-1]
	worst := severity(first.level)
	t := protocol.Trend{Samples: len(s.history), MinFreePercent: first.free}
	for i, x := range s.history {
		worst = max(worst, severity(x.level))
		t.MinFreePercent = min(t.MinFreePercent, x.free)
		if i == 0 {
			continue
		}
		// A sample's level covers the interval leading up to it, so pressure
		// that just turned critical counts on the tick that sees it.
		held := max(x.at.Sub(s.history[i-1].at).Seconds(), 0)
		switch x.level {
		case levelWarn:
			t.WarnSeconds += held
		case levelCritical:
			t.CriticalSeconds += held
		}
	}
	t.Worst = severityName[worst]
	t.FreeSlopePerMin = s.freeSlope()
	switch {
	case last.at.Sub(first.at) < minTrendSpan:
		t.Direction = "unknown"
	case t.FreeSlopePerMin <= -steadySlope:
		t.Direction = "rising"
	case t.FreeSlopePerMin >= steadySlope:
		t.Direction = "falling"
	default:
		t.Direction = "steady"
	}
	return t
}

// freeSlope is the least-squares slope of free % against time, in %/min.
func (s *Source) freeSlope() float64 {
	n := float64(len(s.history))
	if n < 2 {
		return 0
	}
	t0 := s.history[0].at
	var mx, my float64
	for _, x := range s.history {
		mx += x.at.Sub(t0).Minutes()
		my += float64(x.free)
	}
	mx, my = mx/n, my/n
	var sxy, sxx float64
	for _, x := range s.history {
		dx := x.at.Sub(t0).Minutes() - mx
		sxy += dx * (float64(x.free) - my)
		sxx += dx * dx
	}
	if sxx == 0 {
		return 0
	}
	return sxy / sxx
}

// memoryUsed is Activity Monitor's "Memory Used": app memory (anonymous pages
// less purgeable ones), wired pages, and what the compressor occupies. It is
// optional: nil if any counter is missing, so the rest of the reading stands.
func (s *Source) memoryUsed() *uint64 {
	var v [5]uint64
	for i, name := range []string{
		"vm.page_pageable_internal_count",
		"vm.page_purgeable_count",
		"vm.page_wired_count",
		"hw.pagesize",
		"vm.compressor_bytes_used",
	} {
		var err error
		if v[i], err = s.uint(name); err != nil {
			return nil
		}
	}
	internal, purgeable, wired, pagesize, compressed := v[0], v[1], v[2], v[3], v[4]
	app := internal - min(purgeable, internal)
	used := (app+wired)*pagesize + compressed
	return &used
}

// uint reads an integer sysctl of either width. The page counters mix 32 and
// 64 bits, and their widths have changed between macOS releases.
func (s *Source) uint(name string) (uint64, error) {
	b, err := s.sys.Raw(name)
	if err != nil {
		return 0, err
	}
	switch len(b) {
	case 4:
		return uint64(binary.LittleEndian.Uint32(b)), nil
	case 8:
		return binary.LittleEndian.Uint64(b), nil
	}
	return 0, fmt.Errorf("%s: %d bytes, want 4 or 8", name, len(b))
}

// rate is the per-second increase of a cumulative counter; a counter that went
// backwards (reset) counts as 0.
func rate(prev, cur uint64, seconds float64) *float64 {
	r := 0.0
	if cur >= prev && seconds > 0 {
		r = float64(cur-prev) / seconds
	}
	return &r
}

// decodeSwapUsage reads darwin's struct xsw_usage: u64 total, u64 avail,
// u64 used, u32 pagesize, boolean_t encrypted (32 bytes, little endian).
func decodeSwapUsage(b []byte) (total, used uint64, err error) {
	if len(b) < 32 {
		return 0, 0, fmt.Errorf("vm.swapusage: %d bytes, want 32", len(b))
	}
	return binary.LittleEndian.Uint64(b[0:]), binary.LittleEndian.Uint64(b[16:]), nil
}

// kern.memorystatus_vm_pressure_level values (xnu's kVMPressureNormal,
// kVMPressureWarning, kVMPressureCritical).
const (
	levelNormal   = 1
	levelWarn     = 2
	levelCritical = 4
)

// severity orders levels; an unknown level ranks below normal.
func severity(level uint32) int {
	switch level {
	case levelNormal:
		return 1
	case levelWarn:
		return 2
	case levelCritical:
		return 3
	}
	return 0
}

var severityName = []string{"unknown", "normal", "warn", "critical"}

// pressureName names a level. An unknown one (a newer macOS) is reported as
// "unknown" rather than failing the free %, swap and trend read alongside it.
func pressureName(level uint32) string { return severityName[severity(level)] }

type reading struct{ h protocol.Host }

func (r reading) Apply(s *protocol.Snapshot) {
	h := r.h
	s.Host = &h
}
