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

	Host     *Host     `json:"host,omitempty"`
	Docker   *Docker   `json:"docker,omitempty"`
	Tart     *Tart     `json:"tart,omitempty"`
	Orca     *Orca     `json:"orca,omitempty"`
	LMStudio *LMStudio `json:"lmstudio,omitempty"`

	// Budget is derived from the sections above once per tick (R2).
	Budget *Budget `json:"budget,omitempty"`
}

// Budget is reserved vs. used vs. headroom (R2). Reserved is what running
// workloads hold or may grow into; headroom is what is left to admit more.
type Budget struct {
	// TotalBytes is the host's physical memory.
	TotalBytes    uint64 `json:"total_bytes"`
	ReservedBytes uint64 `json:"reserved_bytes"`
	// HeadroomBytes is total minus reserved; negative when over-committed,
	// absent while the host's total is unknown. Used memory is Host.UsedBytes.
	HeadroomBytes *int64 `json:"headroom_bytes,omitempty"`
	// UnaccountedBytes is memory in use that no component explains: macOS,
	// Orca, the agents and everything else, which the host baseline reserves
	// for. It is what #23 learns host_baseline_gb from. Signed, because its
	// inputs are read at slightly different instants, and because host used
	// counts compressed memory at its compressed size while process
	// footprints may count it whole, so it reads low under compression (#23
	// checks this). Absent when host used or a component's footprint is
	// unknown.
	UnaccountedBytes *int64 `json:"unaccounted_bytes,omitempty"`
	// Components are docker, tart, lmstudio and host_baseline, in that order.
	Components []BudgetComponent `json:"components"`
	// Unknown names sources with no reading yet. They count as 0, so headroom
	// is then an upper bound.
	Unknown []string `json:"unknown,omitempty"`
}

// BudgetComponent is one part of the reservation.
type BudgetComponent struct {
	Name          string `json:"name"`
	ReservedBytes uint64 `json:"reserved_bytes"`
	// UsedBytes is what it costs the host now; absent when unknown.
	UsedBytes *uint64 `json:"used_bytes,omitempty"`
}

// LMStudio is LM Studio's loaded models and what they cost (R1).
type LMStudio struct {
	Installed bool `json:"installed"`
	// Running is false when LM Studio's backend is not running. headroom
	// never starts it: the lms CLI would wake it.
	Running bool          `json:"running"`
	Models  []LoadedModel `json:"models"`
	// FootprintBytes is what LM Studio's backend and every process under it
	// cost the host (phys_footprint). A loaded model's worker costs about its
	// file size plus context and runtime: the real figure, which LM Studio
	// itself does not report. Absent when it could not be read.
	FootprintBytes *uint64 `json:"footprint_bytes,omitempty"`
	FootprintError string  `json:"footprint_error,omitempty"`
}

// LoadedModel is one model loaded in LM Studio, as lms ps reports it.
type LoadedModel struct {
	Key  string `json:"key"`
	Type string `json:"type"` // llm or embedding
	// Format is the weights format: gguf (llama.cpp) or safetensors (MLX).
	Format string `json:"format"`
	// SizeBytes is the model's file size; it stays reserved while loaded,
	// even when idle (R2).
	SizeBytes     uint64     `json:"size_bytes"`
	ContextLength int        `json:"context_length"`
	Status        string     `json:"status"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
	// TTL is the idle time after which LM Studio unloads the model; absent
	// when it stays loaded until unloaded by hand.
	TTL *time.Duration `json:"ttl_ns,omitempty"`
}

// Orca is the state of Orca's worktrees and agents (R1).
type Orca struct {
	// Installed is false when no orca CLI was found.
	Installed bool `json:"installed"`
	// Running is false when the Orca app is not running.
	Running bool `json:"running"`
	// AppMemoryBytes is Orca's own memory (main, renderer, helpers), RSS.
	AppMemoryBytes uint64     `json:"app_memory_bytes"`
	Worktrees      []Worktree `json:"worktrees"`
	// MemoryError is set when Orca's memory diagnostics could not be read;
	// memory, CPU and session PIDs are then unknown, not zero.
	MemoryError string `json:"memory_error,omitempty"`
}

// Worktree is one local, unarchived Orca worktree.
type Worktree struct {
	// ID is <repoId>::<path>, the same value as ORCA_WORKTREE_ID.
	ID     string `json:"id"`
	Path   string `json:"path"`
	Name   string `json:"name"`
	Branch string `json:"branch"`
	// Status is Orca's worktree status: working, active or inactive.
	Status         string    `json:"status"`
	LiveTerminals  int       `json:"live_terminals"`
	LastActivityAt time.Time `json:"last_activity_at"`
	Agents         []Agent   `json:"agents"`

	// The agents' own processes in this worktree, from Orca's memory
	// diagnostics. Memory is RSS, which overcounts shared pages.
	MemoryBytes uint64  `json:"memory_bytes"`
	CPUPercent  float64 `json:"cpu_percent"`
	// SessionPIDs are the root processes of the worktree's terminals, for
	// attribution by process tree (R3).
	SessionPIDs []int `json:"session_pids,omitempty"`
}

// Agent is one coding agent in a worktree. State is set by Orca's agent
// status hooks: working, waiting, done, ...
type Agent struct {
	PaneKey    string    `json:"pane_key"`
	Type       string    `json:"type"`
	State      string    `json:"state"`
	StateSince time.Time `json:"state_since"`
}

// Tart is the state of Tart VMs (R1).
type Tart struct {
	// Installed is false when no tart binary was found.
	Installed bool `json:"installed"`
	// VMs are the running VMs.
	VMs []TartVM `json:"vms"`
	// MacOSRunning counts running macOS VMs: Apple's licence allows two (R6).
	MacOSRunning int `json:"macos_running"`
	// VMError is set when VM processes could not be read; footprints are
	// then unknown, not zero.
	VMError string `json:"vm_error,omitempty"`
	// LaunchError is set when tart run processes could not be read; launch
	// details (attribution only) are then missing.
	LaunchError string `json:"launch_error,omitempty"`
}

// TartVM is one running Tart VM.
type TartVM struct {
	Name string `json:"name"`
	// OS is darwin or linux.
	OS   string `json:"os"`
	CPUs int    `json:"cpus"`
	// MemoryBytes is the configured memory: what the VM reserves (R2).
	MemoryBytes uint64 `json:"memory_bytes"`
	// FootprintBytes is what the VM process costs the host now
	// (phys_footprint); absent if its process was not found (e.g. booting),
	// which means unknown, not free.
	FootprintBytes *uint64 `json:"footprint_bytes,omitempty"`

	// How the VM was launched, for attribution (R3). tart run changes its own
	// cwd to the VM bundle, so the launching shell is its parent.
	RunPID int `json:"run_pid,omitempty"`
	// LaunchCwd is the cwd of tart run's parent; empty if it has exited.
	LaunchCwd string `json:"launch_cwd,omitempty"`
	// SharedDirs are the host paths of --dir shares.
	SharedDirs []string `json:"shared_dirs,omitempty"`
}

// Docker is Docker Desktop's state (R1).
type Docker struct {
	// Running is false when nothing answers on the Docker socket.
	Running bool `json:"running"`
	// VMLimitBytes is the VM's configured memory (/info MemTotal): a ceiling,
	// not a cost (spike #9).
	VMLimitBytes uint64 `json:"vm_limit_bytes"`
	// VMRunning is false while Resource Saver has stopped the VM, even though
	// the API still answers.
	VMRunning bool `json:"vm_running"`
	// VMFootprintBytes is what the VM process costs the host (phys_footprint):
	// overhead plus the guest's high-water mark, which it does not give back.
	VMFootprintBytes uint64 `json:"vm_footprint_bytes"`
	// VMError is set when the VM process could not be read; VMRunning and
	// VMFootprintBytes are then unknown, not zero.
	VMError    string      `json:"vm_error,omitempty"`
	Containers []Container `json:"containers"`
}

// Container is one running container.
type Container struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Image  string            `json:"image"`
	Labels map[string]string `json:"labels,omitempty"`
	// Mounts are the host paths of bind mounts, for worktree attribution (R3).
	Mounts []string `json:"mounts,omitempty"`
	// MemoryBytes is usage minus inactive file cache, as docker stats shows.
	MemoryBytes uint64 `json:"memory_bytes"`
	// CPUPercent is CPU use since the previous tick, 100 = one core, as
	// docker stats shows; absent on a container's first tick.
	CPUPercent *float64 `json:"cpu_percent,omitempty"`
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
	// UsedBytes is Activity Monitor's "Memory Used": app memory, wired and
	// compressed. Absent where the kernel lacks one of its counters.
	UsedBytes *uint64 `json:"used_bytes,omitempty"`

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
