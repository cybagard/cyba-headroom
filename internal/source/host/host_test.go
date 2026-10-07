package host_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/source/host"
)

// fakeSysctl serves fixed values by name; tests change them between ticks.
type fakeSysctl struct {
	u32  map[string]uint32
	u64  map[string]uint64
	raw  map[string][]byte
	errs map[string]error // returned instead of a value when set
}

func (f *fakeSysctl) Uint32(name string) (uint32, error) {
	if v, ok := f.u32[name]; ok {
		return v, nil
	}
	return 0, fmt.Errorf("unknown oid %q: %w", name, syscall.ENOENT)
}

func (f *fakeSysctl) Uint64(name string) (uint64, error) {
	if err := f.errs[name]; err != nil {
		return 0, err
	}
	if v, ok := f.u64[name]; ok {
		return v, nil
	}
	return 0, fmt.Errorf("unknown oid %q: %w", name, syscall.ENOENT)
}

func (f *fakeSysctl) Raw(name string) ([]byte, error) {
	if v, ok := f.raw[name]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("unknown oid %q: %w", name, syscall.ENOENT)
}

// xswUsage encodes struct xsw_usage as the darwin kernel returns it.
func xswUsage(total, avail, used uint64) []byte {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint64(b[0:], total)
	binary.LittleEndian.PutUint64(b[8:], avail)
	binary.LittleEndian.PutUint64(b[16:], used)
	binary.LittleEndian.PutUint32(b[24:], 16384)
	binary.LittleEndian.PutUint32(b[28:], 1)
	return b
}

// healthyMac is a 64 GB Mac at normal pressure with no swap.
func healthyMac() *fakeSysctl {
	return &fakeSysctl{
		u32: map[string]uint32{
			"kern.memorystatus_vm_pressure_level": 1,
			"kern.memorystatus_level":             94,
		},
		u64: map[string]uint64{
			"hw.memsize":                           64 << 30,
			"vm.compressor.swapper.swapins_total":  0,
			"vm.compressor.swapper.swapouts_total": 0,
		},
		raw: map[string][]byte{
			"vm.swapusage": xswUsage(0, 0, 0),
			// 1000 internal − 100 purgeable + 200 wired pages of 16 KiB, plus
			// 1 MiB compressed. Widths as on macOS 27: some are 32-bit.
			"vm.page_pageable_internal_count": le32(1000),
			"vm.page_purgeable_count":         le64(100),
			"vm.page_wired_count":             le32(200),
			"vm.compressor_bytes_used":        le64(1 << 20),
			"hw.pagesize":                     le64(16384),
			// 300 pages held in the compressor, at their full size.
			"vm.compressor.pages_compressed_incore": le32(300),
		},
	}
}

// healthyUsed is the memory in use healthyMac reports.
const healthyUsed = 1100*16384 + 1<<20

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
func le64(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }

// clock is a manual clock for the source.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock                   { return &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)} }

// collect runs one collection and returns the host section it produces.
func collect(t *testing.T, src *host.Source) protocol.Host {
	t.Helper()
	r, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var snap protocol.Snapshot
	r.Apply(&snap)
	if snap.Host == nil {
		t.Fatal("reading did not fill Snapshot.Host")
	}
	return *snap.Host
}

func TestReportsPressureFreePercentAndTotal(t *testing.T) {
	sys := healthyMac()
	src := host.New(sys, 5*time.Minute, newClock().now)

	h := collect(t, src)
	if h.Pressure != "normal" || h.FreePercent != 94 || h.TotalBytes != 64<<30 {
		t.Fatalf("got pressure=%q free=%d total=%d, want normal 94 %d", h.Pressure, h.FreePercent, h.TotalBytes, uint64(64<<30))
	}

	for level, want := range map[uint32]string{2: "warn", 4: "critical"} {
		sys.u32["kern.memorystatus_vm_pressure_level"] = level
		if got := collect(t, src).Pressure; got != want {
			t.Errorf("level %d: pressure = %q, want %q", level, got, want)
		}
	}
}

func TestReportsSwapUsage(t *testing.T) {
	sys := healthyMac()
	sys.raw["vm.swapusage"] = xswUsage(4<<30, 3<<30, 1<<30)
	h := collect(t, host.New(sys, 5*time.Minute, newClock().now))
	if h.SwapTotalBytes != 4<<30 || h.SwapUsedBytes != 1<<30 {
		t.Fatalf("swap total=%d used=%d, want %d %d", h.SwapTotalBytes, h.SwapUsedBytes, uint64(4<<30), uint64(1<<30))
	}
}

func TestBadKernelValuesAreErrors(t *testing.T) {
	cases := map[string]func(*fakeSysctl){
		"short xsw_usage": func(f *fakeSysctl) { f.raw["vm.swapusage"] = make([]byte, 24) },
		"missing oid":     func(f *fakeSysctl) { delete(f.u64, "hw.memsize") },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			sys := healthyMac()
			breakIt(sys)
			if _, err := host.New(sys, 5*time.Minute, newClock().now).Collect(context.Background()); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestSwapRatesFromCounterDeltas(t *testing.T) {
	sys := healthyMac()
	clk := newClock()
	src := host.New(sys, 5*time.Minute, clk.now)
	sys.u64["vm.compressor.swapper.swapins_total"] = 1000
	sys.u64["vm.compressor.swapper.swapouts_total"] = 500

	if h := collect(t, src); h.SwapinsPerSec != nil || h.SwapoutsPerSec != nil {
		t.Fatalf("first sample has rates %v %v, want none", h.SwapinsPerSec, h.SwapoutsPerSec)
	}

	clk.advance(10 * time.Second)
	sys.u64["vm.compressor.swapper.swapins_total"] = 1050
	sys.u64["vm.compressor.swapper.swapouts_total"] = 520
	h := collect(t, src)
	if h.SwapinsPerSec == nil || *h.SwapinsPerSec != 5 || h.SwapoutsPerSec == nil || *h.SwapoutsPerSec != 2 {
		t.Fatalf("rates = %v %v, want 5/s in, 2/s out", h.SwapinsPerSec, h.SwapoutsPerSec)
	}

	// Counters reset (e.g. sleep/wake quirk): never a negative rate.
	clk.advance(10 * time.Second)
	sys.u64["vm.compressor.swapper.swapins_total"] = 3
	sys.u64["vm.compressor.swapper.swapouts_total"] = 0
	h = collect(t, src)
	if *h.SwapinsPerSec != 0 || *h.SwapoutsPerSec != 0 {
		t.Fatalf("rates after reset = %v %v, want 0 0", *h.SwapinsPerSec, *h.SwapoutsPerSec)
	}
}

// sample sets the pressure level and free % the next collection reads.
func (f *fakeSysctl) sample(level, free uint32) {
	f.u32["kern.memorystatus_vm_pressure_level"] = level
	f.u32["kern.memorystatus_level"] = free
}

func TestTrendOverWindow(t *testing.T) {
	sys := healthyMac()
	clk := newClock()
	src := host.New(sys, 5*time.Minute, clk.now)

	// One sample a minute: normal → warn → critical → warn → normal.
	var h protocol.Host
	for i, s := range []struct{ level, free uint32 }{{1, 90}, {2, 60}, {4, 30}, {2, 50}, {1, 80}} {
		if i > 0 {
			clk.advance(time.Minute)
		}
		sys.sample(s.level, s.free)
		h = collect(t, src)
	}
	tr := h.Trend
	if tr.Samples != 5 || tr.Worst != "critical" || tr.MinFreePercent != 30 {
		t.Fatalf("trend = %+v, want 5 samples, worst critical, min free 30", tr)
	}
	// Each level holds until the next sample: warn 1m + 1m, critical 1m.
	if tr.WarnSeconds != 120 || tr.CriticalSeconds != 60 {
		t.Fatalf("warn=%vs critical=%vs, want 120 and 60", tr.WarnSeconds, tr.CriticalSeconds)
	}

	// Six more minutes at normal: the episode leaves the 5-minute window.
	for range 6 {
		clk.advance(time.Minute)
		sys.sample(1, 85)
		h = collect(t, src)
	}
	tr = h.Trend
	if tr.Worst != "normal" || tr.WarnSeconds != 0 || tr.CriticalSeconds != 0 || tr.MinFreePercent != 85 || tr.Samples != 5 {
		t.Fatalf("after window passed: %+v, want 5 normal samples at 85%%", tr)
	}
}

func TestTrendDirection(t *testing.T) {
	cases := []struct {
		name      string
		free      []uint32 // one sample a minute
		slope     float64  // least-squares %/min, worked by hand
		direction string   // of pressure: rising when free memory falls
	}{
		{"single sample", []uint32{90}, 0, "unknown"},
		{"free memory draining", []uint32{90, 90, 84}, -3, "rising"},
		{"free memory recovering", []uint32{50, 60, 70}, 10, "falling"},
		{"flat", []uint32{80, 80, 80, 80}, 0, "steady"},
		{"noise under 1%/min", []uint32{80, 81, 80, 81}, 0.2, "steady"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sys := healthyMac()
			clk := newClock()
			src := host.New(sys, 5*time.Minute, clk.now)
			var h protocol.Host
			for i, f := range tc.free {
				if i > 0 {
					clk.advance(time.Minute)
				}
				sys.sample(1, f)
				h = collect(t, src)
			}
			if d := h.Trend.FreeSlopePerMin - tc.slope; d > 1e-9 || d < -1e-9 {
				t.Errorf("slope = %v, want %v", h.Trend.FreeSlopePerMin, tc.slope)
			}
			if h.Trend.Direction != tc.direction {
				t.Errorf("direction = %q, want %q", h.Trend.Direction, tc.direction)
			}
		})
	}
}

// macOS 15 has no vm.compressor.swapper.* totals (added in later releases).
func TestSwapRatesAbsentWhenKernelLacksCounters(t *testing.T) {
	sys := healthyMac()
	delete(sys.u64, "vm.compressor.swapper.swapins_total")
	delete(sys.u64, "vm.compressor.swapper.swapouts_total")
	clk := newClock()
	src := host.New(sys, 5*time.Minute, clk.now)

	collect(t, src)
	clk.advance(10 * time.Second)
	h := collect(t, src)
	if h.SwapinsPerSec != nil || h.SwapoutsPerSec != nil {
		t.Fatalf("rates = %v %v, want none", h.SwapinsPerSec, h.SwapoutsPerSec)
	}
	if h.Pressure != "normal" || h.Trend.Samples != 2 {
		t.Fatalf("rest of the reading missing: %+v", h)
	}
}

func TestUnknownPressureLevelKeepsRestOfReading(t *testing.T) {
	sys := healthyMac()
	sys.sample(8, 70) // a level this build does not know, e.g. from a newer macOS
	h := collect(t, host.New(sys, 5*time.Minute, newClock().now))
	if h.Pressure != "unknown" || h.FreePercent != 70 || h.TotalBytes != 64<<30 {
		t.Fatalf("got %+v, want pressure unknown with free %% and total still read", h)
	}
}

func TestSwapRatesAbsentOnAnyCounterError(t *testing.T) {
	sys := healthyMac()
	// x/sys returns EIO when a sysctl's size is not 8 bytes.
	sys.errs = map[string]error{"vm.compressor.swapper.swapins_total": syscall.EIO}
	clk := newClock()
	src := host.New(sys, 5*time.Minute, clk.now)
	collect(t, src)
	clk.advance(10 * time.Second)
	if h := collect(t, src); h.SwapinsPerSec != nil || h.Pressure != "normal" {
		t.Fatalf("got %+v, want reading without rates", h)
	}
}

func TestNewestSampleCountsTowardTimeAtLevel(t *testing.T) {
	sys := healthyMac()
	clk := newClock()
	src := host.New(sys, 5*time.Minute, clk.now)
	collect(t, src)
	clk.advance(5 * time.Second)
	sys.sample(4, 20)
	// Pressure just went critical: the guard must see it on this tick.
	if tr := collect(t, src).Trend; tr.Worst != "critical" || tr.CriticalSeconds != 5 {
		t.Fatalf("trend = %+v, want worst critical with 5s at critical", tr)
	}
}

func TestDirectionNeedsAMinuteOfHistory(t *testing.T) {
	sys := healthyMac()
	clk := newClock()
	src := host.New(sys, 5*time.Minute, clk.now)
	sys.sample(1, 80)
	collect(t, src)
	clk.advance(5 * time.Second)
	sys.sample(1, 79) // 1 point in 5 s would read as -12 %/min
	if tr := collect(t, src).Trend; tr.Direction != "unknown" {
		t.Fatalf("direction after 5 s of history = %q, want unknown", tr.Direction)
	}
}

func TestReportsMemoryUsed(t *testing.T) {
	h := collect(t, host.New(healthyMac(), 5*time.Minute, newClock().now))
	if h.UsedBytes == nil || *h.UsedBytes != healthyUsed {
		t.Fatalf("used = %v, want %d", h.UsedBytes, healthyUsed)
	}
}

func TestMemoryUsedAcceptsEitherWidth(t *testing.T) {
	sys := healthyMac()
	sys.raw["vm.page_pageable_internal_count"] = le64(1000)
	sys.raw["vm.page_purgeable_count"] = le32(100)
	h := collect(t, host.New(sys, 5*time.Minute, newClock().now))
	if h.UsedBytes == nil || *h.UsedBytes != healthyUsed {
		t.Fatalf("used = %v, want %d", h.UsedBytes, healthyUsed)
	}
}

func TestMemoryUsedIsOptional(t *testing.T) {
	for _, oid := range []string{"vm.page_wired_count", "vm.compressor_bytes_used", "hw.pagesize"} {
		t.Run(oid, func(t *testing.T) {
			sys := healthyMac()
			delete(sys.raw, oid)
			h := collect(t, host.New(sys, 5*time.Minute, newClock().now))
			if h.UsedBytes != nil {
				t.Fatalf("used = %d, want unknown", *h.UsedBytes)
			}
			if h.TotalBytes != 64<<30 {
				t.Fatalf("rest of the reading lost: %+v", h)
			}
		})
	}
	sys := healthyMac()
	sys.raw["vm.page_wired_count"] = []byte{1, 2}
	if h := collect(t, host.New(sys, 5*time.Minute, newClock().now)); h.UsedBytes != nil {
		t.Fatalf("odd width: used = %d, want unknown", *h.UsedBytes)
	}
}

func TestReportsCompressor(t *testing.T) {
	h := collect(t, host.New(healthyMac(), 5*time.Minute, newClock().now))
	if h.CompressorBytes == nil || *h.CompressorBytes != 1<<20 {
		t.Errorf("compressor = %v, want %d", h.CompressorBytes, 1<<20)
	}
	if h.CompressedBytes == nil || *h.CompressedBytes != 300*16384 {
		t.Errorf("compressed = %v, want %d", h.CompressedBytes, 300*16384)
	}
}

func TestCompressedIsOptional(t *testing.T) {
	sys := healthyMac()
	delete(sys.raw, "vm.compressor.pages_compressed_incore") // macOS 15
	h := collect(t, host.New(sys, 5*time.Minute, newClock().now))
	if h.CompressedBytes != nil {
		t.Fatalf("compressed = %d, want unknown", *h.CompressedBytes)
	}
	if h.UsedBytes == nil || h.CompressorBytes == nil {
		t.Fatal("used and compressor must not depend on it")
	}
}
