// Package samples records the daemon's snapshots to disk as compact JSONL and
// reads them back (#55), so `headroom suggest` (#23) can learn thresholds
// from weeks of real use.
//
// A sample is a purpose-built record, not the snapshot: it leaves out
// container labels, process IDs and anything else that could carry secrets,
// and keeps the attribution keys (paths) that let samples be attributed to
// worktrees after the fact (#20).
package samples

import (
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// Version is the sample schema version. Bump it when a field changes meaning;
// readers skip versions they do not know.
const Version = 1

// Sample is one tick's record.
type Sample struct {
	V int       `json:"v"`
	T time.Time `json:"t"`

	Host   *Host            `json:"host,omitempty"`
	Budget *protocol.Budget `json:"budget,omitempty"`
	// Stale lists sources whose latest read failed; absent ones were fresh.
	Stale map[string]bool `json:"stale,omitempty"`

	Docker     *Docker     `json:"docker,omitempty"`
	Containers []Container `json:"containers,omitempty"`
	TartVMs    []TartVM    `json:"tart_vms,omitempty"`
	LMStudio   *LMStudio   `json:"lmstudio,omitempty"`

	OrcaAppBytes *uint64    `json:"orca_app_bytes,omitempty"`
	Worktrees    []Worktree `json:"worktrees,omitempty"`
}

// Host is the host's memory state.
type Host struct {
	TotalBytes      uint64   `json:"total_bytes"`
	UsedBytes       *uint64  `json:"used_bytes,omitempty"`
	CompressorBytes *uint64  `json:"compressor_bytes,omitempty"`
	CompressedBytes *uint64  `json:"compressed_bytes,omitempty"`
	Pressure        string   `json:"pressure"`
	FreePercent     int      `json:"free_percent"`
	SwapUsedBytes   uint64   `json:"swap_used_bytes"`
	SwapinsPerSec   *float64 `json:"swapins_per_sec,omitempty"`
	SwapoutsPerSec  *float64 `json:"swapouts_per_sec,omitempty"`
}

// Docker is the Docker VM's state.
type Docker struct {
	VMRunning        bool   `json:"vm_running"`
	VMLimitBytes     uint64 `json:"vm_limit_bytes"`
	VMFootprintBytes uint64 `json:"vm_footprint_bytes"`
}

// Container is one running container and its attribution keys.
type Container struct {
	Name        string   `json:"name"`
	Image       string   `json:"image"`
	MemoryBytes uint64   `json:"memory_bytes"`
	CPUPercent  *float64 `json:"cpu_percent,omitempty"`
	// ComposeDir is the compose project's working directory, if any.
	ComposeDir string   `json:"compose_dir,omitempty"`
	Mounts     []string `json:"mounts,omitempty"`
}

// TartVM is one running Tart VM and its attribution keys.
type TartVM struct {
	Name           string   `json:"name"`
	OS             string   `json:"os"`
	MemoryBytes    uint64   `json:"memory_bytes"`
	FootprintBytes *uint64  `json:"footprint_bytes,omitempty"`
	LaunchCwd      string   `json:"launch_cwd,omitempty"`
	SharedDirs     []string `json:"shared_dirs,omitempty"`
}

// LMStudio is LM Studio's footprint and loaded models.
type LMStudio struct {
	FootprintBytes *uint64 `json:"footprint_bytes,omitempty"`
	Models         []Model `json:"models,omitempty"`
}

// Model is one loaded model.
type Model struct {
	Key       string `json:"key"`
	SizeBytes uint64 `json:"size_bytes"`
	// TTL is the idle time after which LM Studio unloads the model; absent
	// when it stays loaded until unloaded by hand. With LastUsedAt, it lets
	// suggest (#23) spot models that sit loaded and unused. Both were added
	// to schema v1 later, so older lines lack them.
	TTL        *time.Duration `json:"ttl_ns,omitempty"`
	LastUsedAt *time.Time     `json:"last_used_at,omitempty"`
}

// Worktree is one Orca worktree, its agents' processes and their states.
type Worktree struct {
	ID          string  `json:"id"`
	Path        string  `json:"path"`
	Name        string  `json:"name"`
	MemoryBytes uint64  `json:"memory_bytes"`
	CPUPercent  float64 `json:"cpu_percent"`
	// Agents are the agents' states: working, waiting, done, ...
	Agents []string `json:"agents,omitempty"`
}

// FromSnapshot builds the sample for s.
func FromSnapshot(s *protocol.Snapshot) Sample {
	out := Sample{V: Version, T: s.CollectedAt, Budget: s.Budget}
	for name, st := range s.Sources {
		if st.Stale {
			if out.Stale == nil {
				out.Stale = map[string]bool{}
			}
			out.Stale[name] = true
		}
	}
	if h := s.Host; h != nil {
		out.Host = &Host{
			TotalBytes: h.TotalBytes, UsedBytes: h.UsedBytes,
			CompressorBytes: h.CompressorBytes, CompressedBytes: h.CompressedBytes,
			Pressure: h.Pressure, FreePercent: h.FreePercent, SwapUsedBytes: h.SwapUsedBytes,
			SwapinsPerSec: h.SwapinsPerSec, SwapoutsPerSec: h.SwapoutsPerSec,
		}
	}
	if d := s.Docker; d != nil && d.Running {
		out.Docker = &Docker{VMRunning: d.VMRunning, VMLimitBytes: d.VMLimitBytes, VMFootprintBytes: d.VMFootprintBytes}
		for _, c := range d.Containers {
			out.Containers = append(out.Containers, Container{
				Name: c.Name, Image: c.Image, MemoryBytes: c.MemoryBytes, CPUPercent: c.CPUPercent,
				ComposeDir: c.Labels[protocol.ComposeWorkingDirLabel], Mounts: c.Mounts,
			})
		}
	}
	if t := s.Tart; t != nil {
		for _, vm := range t.VMs {
			out.TartVMs = append(out.TartVMs, TartVM{
				Name: vm.Name, OS: vm.OS, MemoryBytes: vm.MemoryBytes, FootprintBytes: vm.FootprintBytes,
				LaunchCwd: vm.LaunchCwd, SharedDirs: vm.SharedDirs,
			})
		}
	}
	if l := s.LMStudio; l != nil && l.Running {
		out.LMStudio = &LMStudio{FootprintBytes: l.FootprintBytes}
		for _, m := range l.Models {
			out.LMStudio.Models = append(out.LMStudio.Models, Model{Key: m.Key, SizeBytes: m.SizeBytes, TTL: m.TTL, LastUsedAt: m.LastUsedAt})
		}
	}
	if o := s.Orca; o != nil && o.Running {
		app := o.AppMemoryBytes
		out.OrcaAppBytes = &app
		for _, w := range o.Worktrees {
			ws := Worktree{ID: w.ID, Path: w.Path, Name: w.Name, MemoryBytes: w.MemoryBytes, CPUPercent: w.CPUPercent}
			for _, a := range w.Agents {
				ws.Agents = append(ws.Agents, a.State)
			}
			out.Worktrees = append(out.Worktrees, ws)
		}
	}
	return out
}
