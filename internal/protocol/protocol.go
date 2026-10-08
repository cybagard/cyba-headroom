// Package protocol defines the wire format between the headroom daemon and its
// clients: one JSON line in, one JSON line out, then the connection closes.
// The shim imports this package on every docker/podman/tart call, so it must
// stay dependency-free (ADR 0001).
package protocol

import "time"

// Version is the protocol version every request and reply carries.
const Version = 1

// Ops understood by the daemon.
const (
	OpPing   = "ping"
	OpStatus = "status"
	// OpCheck asks the policy (#24) whether a resource-creating call may go
	// ahead.
	OpCheck = "check"
	// OpRelease ends a check's lease when its call did not start (#28).
	OpRelease = "release"
)

// MaxLine bounds a single request or reply line.
const MaxLine = 1 << 20

// Request is one client call.
type Request struct {
	V  int    `json:"v"`
	Op string `json:"op"`
	// Check is the call to decide, for OpCheck.
	Check *CheckRequest `json:"check,omitempty"`
	// Release is the lease to end, for OpRelease.
	Release string `json:"release,omitempty"`
}

// CheckRequest describes a resource-creating call (#24).
type CheckRequest struct {
	// Worktree is the Orca worktree ID; empty for a manual call.
	Worktree string `json:"worktree,omitempty"`
	// Kind is container, compose or tart.
	Kind    string `json:"kind"`
	Command string `json:"command"`
	// CostBytes is the caller's estimate; 0 means the policy's default.
	CostBytes uint64 `json:"cost_bytes,omitempty"`
	// Cwd and Ancestors identify the caller when Worktree is empty (#28):
	// its working directory, and its parent PIDs, nearest first.
	Cwd       string `json:"cwd,omitempty"`
	Ancestors []int  `json:"ancestors,omitempty"`
	// MacOS is set for a tart run of a macOS VM, which takes one of the
	// macOS VM slots (R6, #29).
	MacOS bool `json:"macos,omitempty"`
	// VMUnknown is set when the VM's config was not found: it is counted as
	// macOS rather than let a third macOS VM by.
	VMUnknown bool `json:"vm_unknown,omitempty"`
	// PID is the calling process, for tart run: it becomes tart, so its exit
	// before its VM appears means the run failed (#29).
	PID int `json:"pid,omitempty"`
	// RealCwd is Cwd with symlinks resolved, when that differs: a worktree
	// may be known by either spelling.
	RealCwd string `json:"real_cwd,omitempty"`
	// Target and Name are what the call starts: the image, container,
	// compose project or VM, and a run's --name. The lease uses them to
	// bind its own container (#33); they are never shown or logged.
	Target string `json:"target,omitempty"`
	Name   string `json:"name,omitempty"`
	// Labelled is set when the shim adds the lease's ID to the container
	// as LeaseLabel (a run or create): its lease binds that container only.
	Labelled bool `json:"labelled,omitempty"`
	// Op is the call's subcommand (run, create, start, restart, up, ...).
	Op string `json:"op,omitempty"`
	// Engine is the endpoint a docker call talks to, as its CLI resolves
	// it (DOCKER_HOST, else the context's): the daemon looks a start's
	// container up only when that is its own socket. "" when unknown.
	Engine string `json:"engine,omitempty"`
	// MultiTarget is set for a start or restart of several containers.
	MultiTarget bool `json:"multi_target,omitempty"`
	// Targets are all the containers a start or restart names, as given.
	Targets []string `json:"targets,omitempty"`
}

// LeaseLabel carries a gated run's lease ID on its container, so the
// daemon tells it from one started past the shim (#33).
const LeaseLabel = "dev.headroom.lease"

// How a check's worktree was found (Decision.IdentifiedBy).
const (
	IdentifiedByCaller  = "caller"  // named by the caller: HEADROOM_WORKTREE, ORCA_WORKTREE_ID or --worktree
	IdentifiedByCwd     = "cwd"     // the working directory is in the worktree
	IdentifiedByProcess = "process" // the caller descends from its terminal
)

// Decision is the policy's answer to a CheckRequest.
type Decision struct {
	Allow bool `json:"allow"`
	// Worktree is the worktree the call was decided for, "" for a manual
	// call, and IdentifiedBy how it was found.
	Worktree     string `json:"worktree,omitempty"`
	IdentifiedBy string `json:"identified_by,omitempty"`
	// Retry is true when waiting may let the call through.
	Retry   bool     `json:"retry,omitempty"`
	Reasons []Reason `json:"reasons,omitempty"`
	// Message explains the decision to an agent.
	Message       string `json:"message"`
	HeadroomBytes *int64 `json:"headroom_bytes,omitempty"`
	CostBytes     uint64 `json:"cost_bytes"`
	// Holding is the worktree's own containers and VMs.
	Holding []Held `json:"holding,omitempty"`
	// LeaseID names the reservation an allow made (#25).
	LeaseID string `json:"lease_id,omitempty"`
	// LeasedBytes is what earlier allows had reserved, and not yet shown up,
	// when this decision was made.
	LeasedBytes uint64 `json:"leased_bytes,omitempty"`
}

// Lease reserves an allowed call's cost until its container or VM appears,
// or until it expires (R10, #25).
type Lease struct {
	ID       string    `json:"id"`
	Worktree string    `json:"worktree,omitempty"`
	Kind     string    `json:"kind"`
	Command  string    `json:"command"`
	Bytes    uint64    `json:"bytes"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
}

// Ungated is a container or VM that appeared without a check through the
// shim, so no lease was taken for it (R4, #33): started through the socket
// or an SDK, by a login shell that put the real binary first, by an agent
// not launched through headroom run, or while the daemon was down.
type Ungated struct {
	Key  string `json:"key"` // container:<id> or vm:<name>
	Name string `json:"name"`
	Kind string `json:"kind"` // container, compose or vm
	// Worktree is the worktree it is attributed to; "" when none.
	Worktree string    `json:"worktree,omitempty"`
	Since    time.Time `json:"since"`
}

// Reason is one policy rule's verdict.
type Reason struct {
	Code  string `json:"code"`
	Text  string `json:"text"`
	Retry bool   `json:"retry,omitempty"`
}

// Held is one of a worktree's containers or VMs.
type Held struct {
	Name  string `json:"name"`
	Bytes uint64 `json:"bytes"`
}

// Reply is the daemon's answer to one Request.
type Reply struct {
	V        int       `json:"v"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
	// Decision answers OpCheck.
	Decision *Decision `json:"decision,omitempty"`
	// PID is the daemon's process ID, on ping: install uses it to tell the
	// launchd daemon from one started in a terminal.
	PID int `json:"pid,omitempty"`
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
	Ollama   *Ollama   `json:"ollama,omitempty"`

	// Budget is derived from the sections above once per tick (R2).
	Budget *Budget `json:"budget,omitempty"`
	// Attribution assigns containers and Tart VMs to worktrees, derived once
	// per tick (R3).
	Attribution *Attribution `json:"attribution,omitempty"`
	// Leases are the open reservations, oldest first (#25).
	Leases []Lease `json:"leases,omitempty"`
	// Ungated are containers and VMs that appeared without a check (R4,
	// #33), oldest first.
	Ungated []Ungated `json:"ungated,omitempty"`
}

// Attribution is what each worktree runs (R3).
type Attribution struct {
	// Worktrees are Orca's live worktrees, in Orca's order, including those
	// that run nothing.
	Worktrees []WorktreeUsage `json:"worktrees"`
	// Unattributed holds what matched no live worktree, or several.
	Unattributed Usage `json:"unattributed"`
	// OrcaStale means Orca's latest read failed: worktrees are the last
	// ones seen, and may have changed since.
	OrcaStale bool `json:"orca_stale,omitempty"`
}

// ComposeWorkingDirLabel is the label Docker Compose sets on a container to
// its project directory.
const ComposeWorkingDirLabel = "com.docker.compose.project.working_dir"

// ComposeProjectLabel is the label Docker Compose sets on a container to its
// project name.
const ComposeProjectLabel = "com.docker.compose.project"

// WorktreeUsage is one worktree and what it runs.
type WorktreeUsage struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Name string `json:"name"`
	Usage
	// The agents' own processes, from Orca (RSS); absent when Orca's memory
	// diagnostics failed.
	AgentMemoryBytes *uint64  `json:"agent_memory_bytes,omitempty"`
	AgentCPUPercent  *float64 `json:"agent_cpu_percent,omitempty"`
}

// Usage is a set of containers and Tart VMs, with totals.
type Usage struct {
	Containers           []AttributedContainer `json:"containers"`
	ContainerMemoryBytes uint64                `json:"container_memory_bytes"`
	// ContainerCPUPercent is absent while any container's CPU is unknown
	// (its first tick).
	ContainerCPUPercent *float64       `json:"container_cpu_percent,omitempty"`
	TartVMs             []AttributedVM `json:"tart_vms"`
	// TartMemoryBytes is the VMs' configured memory, what they reserve.
	TartMemoryBytes uint64 `json:"tart_memory_bytes"`
	// TartFootprintBytes is absent when a VM's footprint is unknown.
	TartFootprintBytes *uint64 `json:"tart_footprint_bytes,omitempty"`
}

// AttributedContainer is a container and how it was matched (By), or why it
// was not (Reason).
type AttributedContainer struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	MemoryBytes uint64   `json:"memory_bytes"`
	CPUPercent  *float64 `json:"cpu_percent,omitempty"`
	By          string   `json:"by,omitempty"`
	Reason      string   `json:"reason,omitempty"`
}

// AttributedVM is a Tart VM and how it was matched (By), or why it was not
// (Reason).
type AttributedVM struct {
	Name           string  `json:"name"`
	MemoryBytes    uint64  `json:"memory_bytes"`
	FootprintBytes *uint64 `json:"footprint_bytes,omitempty"`
	By             string  `json:"by,omitempty"`
	Reason         string  `json:"reason,omitempty"`
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
	// inputs are read at slightly different instants. Compressed memory is
	// counted at full size on both sides, as footprints count it, so the
	// figure holds under compression; pages swapped out to disk are not
	// (#23). Absent when host used, compressed or a component's footprint is
	// unknown.
	UnaccountedBytes *int64 `json:"unaccounted_bytes,omitempty"`
	// Components are docker, tart, lmstudio, ollama and host_baseline, in that order.
	Components []BudgetComponent `json:"components"`
	// Unknown names sources with no reading yet. They count as 0, so headroom
	// is then an upper bound.
	Unknown []string `json:"unknown,omitempty"`
	// Stale names budget inputs whose latest read failed; their last good
	// reading is used, so the budget may lag what is running now.
	Stale []string `json:"stale,omitempty"`
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

// Ollama is Ollama's loaded models and what they cost (R1).
type Ollama struct {
	// Installed is true when the ollama binary or Ollama.app was found.
	Installed bool `json:"installed"`
	// Running is false when no Ollama server is running. headroom never
	// starts it, and asks its API only once it runs.
	Running bool          `json:"running"`
	Models  []OllamaModel `json:"models"`
	// ModelsError says why the model list is unknown while the server runs.
	ModelsError string `json:"models_error,omitempty"`
	// FootprintBytes is what the Ollama server and its runners (one per
	// loaded model) cost the host (phys_footprint). Absent when it could not
	// be read.
	FootprintBytes *uint64 `json:"footprint_bytes,omitempty"`
	FootprintError string  `json:"footprint_error,omitempty"`
}

// OllamaModel is one model loaded in Ollama, as /api/ps reports it.
type OllamaModel struct {
	Name string `json:"name"`
	// SizeBytes is the model's size in memory, weights and context; it stays
	// reserved while loaded, even when idle (R2).
	SizeBytes uint64 `json:"size_bytes"`
	// VRAMBytes is the part of SizeBytes on the GPU.
	VRAMBytes     uint64 `json:"vram_bytes"`
	ContextLength int    `json:"context_length"`
	// ExpiresAt is when Ollama unloads the model if it stays idle; absent
	// when it stays loaded until unloaded by hand (keep_alive < 0).
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
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
	// CompressorBytes is what the compressor occupies, a part of UsedBytes.
	CompressorBytes *uint64 `json:"compressor_bytes,omitempty"`
	// CompressedBytes is what the compressor holds, at its uncompressed size:
	// the size process footprints count compressed pages at. Absent on
	// macOS 15.
	CompressedBytes *uint64 `json:"compressed_bytes,omitempty"`

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
