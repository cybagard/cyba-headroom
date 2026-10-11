// Package samples records the daemon's snapshots to disk as compact JSONL and
// reads them back (#55), so `headroom suggest` (#23) can learn thresholds
// from weeks of real use.
//
// A sample is a purpose-built record, not the snapshot: it leaves out
// container labels, agents' processes and anything else that could carry
// secrets (it keeps the PIDs of VM processes, which name no one), and keeps the attribution keys (paths) that let samples be attributed to
// worktrees after the fact (#20).
package samples

import (
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// Version is the sample schema version. Bump it when a field changes meaning;
// readers skip versions they do not know. Version 2 records every engine and
// the VMs beside them in place of version 1's one Docker (#229).
const Version = 2

// Sample is one tick's record.
type Sample struct {
	V int       `json:"v"`
	T time.Time `json:"t"`

	Host   *Host            `json:"host,omitempty"`
	Budget *protocol.Budget `json:"budget,omitempty"`
	// Stale lists sources whose latest read failed; absent ones were fresh.
	Stale map[string]bool `json:"stale,omitempty"`

	// Engines are the snapshot's engines, in its order, running or not.
	Engines []Engine `json:"engines,omitempty"`
	// Podman is the running Podman machines' VMs; OtherVMs the VMs no known
	// owner claims.
	Podman   []PodmanVM `json:"podman,omitempty"`
	OtherVMs []OtherVM  `json:"other_vms,omitempty"`
	TartVMs  []TartVM   `json:"tart_vms,omitempty"`
	LMStudio *LMStudio  `json:"lmstudio,omitempty"`
	// Ollama is absent when it was not running. Added to schema v1 later.
	Ollama *Ollama `json:"ollama,omitempty"`

	// Ungated are the containers and VMs that appeared without a check
	// (#33), for the gate rollout (#34). Added to schema v1 later.
	Ungated []Ungated `json:"ungated,omitempty"`

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

// Engine is one container engine's state and its containers.
type Engine struct {
	Kind   string `json:"kind,omitempty"`
	Socket string `json:"socket"`
	// Machine is the Podman machine whose VM the engine runs in.
	Machine          string `json:"machine,omitempty"`
	Running          bool   `json:"running"`
	VMRunning        bool   `json:"vm_running"`
	VMLimitBytes     uint64 `json:"vm_limit_bytes"`
	VMFootprintBytes uint64 `json:"vm_footprint_bytes"`
	// VMUnknown means the VM process could not be read: VMRunning and the
	// footprint are unknown, not zero.
	VMUnknown  bool        `json:"vm_unknown,omitempty"`
	Containers []Container `json:"containers,omitempty"`
}

// PodmanVM is one running Podman machine's VM.
type PodmanVM struct {
	Machine        string `json:"machine"`
	PID            int    `json:"pid"`
	FootprintBytes uint64 `json:"footprint_bytes"`
}

// OtherVM is one VM process no known owner claims.
type OtherVM struct {
	PID            int    `json:"pid"`
	Name           string `json:"name"`
	FootprintBytes uint64 `json:"footprint_bytes"`
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
	// Status, TTL and LastUsedAt are protocol.LoadedModel's. They let suggest
	// (#23) spot models that sit loaded and unused; Status tells a model busy
	// generating from an idle one. Added to schema v1 later, so older lines
	// lack them.
	Status     string         `json:"status,omitempty"`
	TTL        *time.Duration `json:"ttl_ns,omitempty"`
	LastUsedAt *time.Time     `json:"last_used_at,omitempty"`
}

// Ollama is the Ollama server's footprint and loaded models.
type Ollama struct {
	FootprintBytes *uint64       `json:"footprint_bytes,omitempty"`
	Models         []OllamaModel `json:"models,omitempty"`
	// ModelsUnknown means the model list could not be read (the API failed
	// or is not on this Mac): no models is not the same as none known.
	ModelsUnknown bool `json:"models_unknown,omitempty"`
}

// OllamaModel is one loaded Ollama model.
type OllamaModel struct {
	Name      string     `json:"name"`
	SizeBytes uint64     `json:"size_bytes"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Ungated is a container or VM that appeared without a check.
type Ungated struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Worktree string `json:"worktree,omitempty"`
}

// Worktree is one Orca worktree, its agents' processes and their states.
type Worktree struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Name string `json:"name"`
	// Project is Orca's repo name; absent in samples written before #102.
	Project     string  `json:"project,omitempty"`
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
	for _, e := range s.Engines {
		se := Engine{Kind: e.Kind, Socket: e.Socket, Machine: e.Machine, Running: e.Running, VMRunning: e.VMRunning,
			VMLimitBytes: e.VMLimitBytes, VMFootprintBytes: e.VMFootprintBytes, VMUnknown: e.VMError != ""}
		for _, c := range e.Containers {
			se.Containers = append(se.Containers, Container{
				Name: c.Name, Image: c.Image, MemoryBytes: c.MemoryBytes, CPUPercent: c.CPUPercent,
				ComposeDir: c.Labels[protocol.ComposeWorkingDirLabel], Mounts: c.Mounts,
			})
		}
		out.Engines = append(out.Engines, se)
	}
	if p := s.Podman; p != nil {
		for _, vm := range p.VMs {
			out.Podman = append(out.Podman, PodmanVM{Machine: vm.Machine, PID: vm.PID, FootprintBytes: vm.FootprintBytes})
		}
	}
	if v := s.VMs; v != nil {
		for _, vm := range v.Other {
			out.OtherVMs = append(out.OtherVMs, OtherVM{PID: vm.PID, Name: vm.Name, FootprintBytes: vm.FootprintBytes})
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
			out.LMStudio.Models = append(out.LMStudio.Models, Model{
				Key: m.Key, SizeBytes: m.SizeBytes, Status: m.Status,
				TTL: clone(m.TTL), LastUsedAt: clone(m.LastUsedAt),
			})
		}
	}
	if o := s.Ollama; o != nil && o.Running {
		out.Ollama = &Ollama{FootprintBytes: clone(o.FootprintBytes), ModelsUnknown: o.ModelsError != ""}
		for _, m := range o.Models {
			out.Ollama.Models = append(out.Ollama.Models, OllamaModel{Name: m.Name, SizeBytes: m.SizeBytes, ExpiresAt: clone(m.ExpiresAt)})
		}
	}
	for _, u := range s.Ungated {
		out.Ungated = append(out.Ungated, Ungated{Name: u.Name, Kind: u.Kind, Worktree: u.Worktree})
	}
	if o := s.Orca; o != nil && o.Running {
		app := o.AppMemoryBytes
		out.OrcaAppBytes = &app
		for _, w := range o.Worktrees {
			ws := Worktree{ID: w.ID, Path: w.Path, Name: w.Name, Project: w.Project, MemoryBytes: w.MemoryBytes, CPUPercent: w.CPUPercent}
			for _, a := range w.Agents {
				ws.Agents = append(ws.Agents, a.State)
			}
			out.Worktrees = append(out.Worktrees, ws)
		}
	}
	return out
}

// clone copies *p, so a sample never shares a pointer with the snapshot.
func clone[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
