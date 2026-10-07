// Package protocol defines the wire format between the headroom daemon and its
// clients: one JSON line in, one JSON line out, then the connection closes.
// The shim imports this package on every docker/podman/tart call, so it must
// stay dependency-free (ADR 0001).
package protocol

import "time"

// Version is the protocol version every request and reply carries.
const Version = 1

// Ops understood by the daemon. check and lease arrive with #24 and #25.
const (
	OpPing   = "ping"
	OpStatus = "status"
)

// MaxLine bounds a single request or reply line.
const MaxLine = 1 << 20

// Request is one client call.
type Request struct {
	V  int    `json:"v"`
	Op string `json:"op"`
}

// Reply is the daemon's answer to one Request.
type Reply struct {
	V        int       `json:"v"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
}

// Snapshot is the daemon's current view of every source. It is immutable once
// published. Collectors (#14–#18) add their typed sections here.
type Snapshot struct {
	// Seq counts completed collection ticks; 0 means nothing collected yet.
	Seq         uint64                  `json:"seq"`
	CollectedAt time.Time               `json:"collected_at"`
	Sources     map[string]SourceStatus `json:"sources"`

	Host *Host `json:"host,omitempty"`
}

// Host is the host's memory state (R1, R4 top line).
type Host struct {
	TotalBytes uint64 `json:"total_bytes"`
	// Pressure is the kernel's level: normal, warn or critical (Activity
	// Monitor's green, yellow, red), or unknown for a level this build does
	// not recognise.
	Pressure string `json:"pressure"`
	// FreePercent is the kernel's free-memory percentage, the signal
	// `memory_pressure` reports.
	FreePercent int `json:"free_percent"`

	SwapTotalBytes uint64 `json:"swap_total_bytes"`
	SwapUsedBytes  uint64 `json:"swap_used_bytes"`
	// Swap-in/out events per second since the previous sample, in the unit
	// the kernel's vm.compressor.swapper totals count (unverified; see #23).
	// Absent on the first sample and where the kernel lacks the counters.
	SwapinsPerSec  *float64 `json:"swapins_per_sec,omitempty"`
	SwapoutsPerSec *float64 `json:"swapouts_per_sec,omitempty"`

	Trend Trend `json:"trend"`
}

// Trend summarises pressure over the trailing window (5 minutes by default).
type Trend struct {
	Samples int `json:"samples"`
	// Worst is the highest pressure level seen in the window.
	Worst string `json:"worst"`
	// Seconds spent at warn and at critical; each sample's level covers the
	// interval leading up to it.
	WarnSeconds     float64 `json:"warn_seconds"`
	CriticalSeconds float64 `json:"critical_seconds"`
	MinFreePercent  int     `json:"min_free_percent"`
	// FreeSlopePerMin is the least-squares slope of free % per minute.
	FreeSlopePerMin float64 `json:"free_slope_per_min"`
	// Direction of pressure: rising (free memory falling), steady, falling,
	// or unknown until a minute of history exists.
	Direction string `json:"direction"`
}

// SourceStatus is the health of one source as of the latest tick.
type SourceStatus struct {
	// At is when the source last succeeded; zero if it never has.
	At time.Time `json:"at"`
	// Took is how long the latest attempt ran.
	Took time.Duration `json:"took_ns"`
	// Err is the latest attempt's error, if it failed.
	Err string `json:"error,omitempty"`
	// Stale means the latest attempt failed and the snapshot holds the last
	// good reading, if any.
	Stale bool `json:"stale"`
}
