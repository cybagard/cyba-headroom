// Package host collects host memory pressure and swap without sudo (R1).
package host

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"syscall"
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
// the kernel lacks them: macOS 15 has no vm.compressor.swapper.* totals.
type counters struct {
	at                time.Time
	ok                bool
	swapins, swapouts uint64
}

func (s *Source) readCounters() (counters, error) {
	c := counters{at: s.now(), ok: true}
	var err error
	if c.swapins, err = s.sys.Uint64("vm.compressor.swapper.swapins_total"); err == nil {
		c.swapouts, err = s.sys.Uint64("vm.compressor.swapper.swapouts_total")
	}
	switch {
	case errors.Is(err, syscall.ENOENT):
		return counters{at: c.at}, nil
	case err != nil:
		return counters{}, err
	}
	return c, nil
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
	pressure, err := pressureName(level)
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
	cur, err := s.readCounters()
	if err != nil {
		return nil, err
	}

	h := protocol.Host{
		TotalBytes:     total,
		Pressure:       pressure,
		FreePercent:    int(free),
		SwapTotalBytes: swapTotal,
		SwapUsedBytes:  swapUsed,
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
	worst := s.history[0].level
	t := protocol.Trend{Samples: len(s.history), MinFreePercent: s.history[0].free}
	for i, x := range s.history {
		worst = max(worst, x.level)
		t.MinFreePercent = min(t.MinFreePercent, x.free)
		if i+1 < len(s.history) {
			held := s.history[i+1].at.Sub(x.at).Seconds()
			switch x.level {
			case 2:
				t.WarnSeconds += held
			case 4:
				t.CriticalSeconds += held
			}
		}
	}
	t.Worst, _ = pressureName(worst)
	t.FreeSlopePerMin = s.freeSlope()
	switch {
	case len(s.history) < 2:
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
	if len(b) < 24 {
		return 0, 0, fmt.Errorf("vm.swapusage: %d bytes, want 32", len(b))
	}
	return binary.LittleEndian.Uint64(b[0:]), binary.LittleEndian.Uint64(b[16:]), nil
}

// pressureName maps kern.memorystatus_vm_pressure_level (xnu's
// kVMPressureNormal/Warning/Critical) to a name.
func pressureName(level uint32) (string, error) {
	switch level {
	case 1:
		return "normal", nil
	case 2:
		return "warn", nil
	case 4:
		return "critical", nil
	}
	return "", fmt.Errorf("unknown memory pressure level %d", level)
}

type reading struct{ h protocol.Host }

func (r reading) Apply(s *protocol.Snapshot) {
	h := r.h
	s.Host = &h
}
