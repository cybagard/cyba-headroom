package suggest

import (
	"slices"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/samples"
)

// Snapshot rebuilds the budget inputs of a recorded sample, so budget.Compute
// can replay it with other parameters: the headroom a new config would have
// shown at that moment. A section the sample lacks is "not running", unless
// the recorded budget listed its source as unknown.
func Snapshot(s samples.Sample) *protocol.Snapshot {
	out := &protocol.Snapshot{CollectedAt: s.T}
	var unknown []string
	if s.Budget != nil {
		unknown = s.Budget.Unknown
	}
	known := func(name string) bool { return !slices.Contains(unknown, name) }

	if h := s.Host; h != nil {
		out.Host = &protocol.Host{
			TotalBytes: h.TotalBytes, UsedBytes: h.UsedBytes,
			CompressorBytes: h.CompressorBytes, CompressedBytes: h.CompressedBytes,
			Pressure: h.Pressure, FreePercent: h.FreePercent, SwapUsedBytes: h.SwapUsedBytes,
		}
	}
	if known("docker") {
		out.Engines = []protocol.Engine{}
		for _, se := range s.Engines {
			e := protocol.Engine{Kind: se.Kind, Socket: se.Socket, Machine: se.Machine, Running: se.Running, VMRunning: se.VMRunning,
				VMLimitBytes: se.VMLimitBytes, VMFootprintBytes: se.VMFootprintBytes}
			if se.VMUnknown {
				e.VMError = "unknown when recorded"
			}
			for _, c := range se.Containers {
				e.Containers = append(e.Containers, protocol.Container{Name: c.Name, MemoryBytes: c.MemoryBytes})
			}
			out.Engines = append(out.Engines, e)
		}
	}
	// The podman and vms sections are absent while none runs, so, as in
	// budget.Compute, only a source never read makes them unknown.
	for _, name := range []string{"podman", "vms"} {
		if !known(name) {
			if out.Sources == nil {
				out.Sources = map[string]protocol.SourceStatus{}
			}
			out.Sources[name] = protocol.SourceStatus{}
		}
	}
	if len(s.Podman) > 0 {
		out.Podman = &protocol.Podman{}
		for _, vm := range s.Podman {
			out.Podman.VMs = append(out.Podman.VMs, protocol.PodmanVM{Machine: vm.Machine, PID: vm.PID, FootprintBytes: vm.FootprintBytes})
		}
	}
	if len(s.OtherVMs) > 0 {
		out.VMs = &protocol.VMs{}
		for _, vm := range s.OtherVMs {
			out.VMs.Other = append(out.VMs.Other, protocol.OtherVM{PID: vm.PID, Name: vm.Name, FootprintBytes: vm.FootprintBytes})
		}
	}
	if known("tart") {
		out.Tart = &protocol.Tart{Installed: true}
		for _, vm := range s.TartVMs {
			out.Tart.VMs = append(out.Tart.VMs, protocol.TartVM{Name: vm.Name, MemoryBytes: vm.MemoryBytes, FootprintBytes: vm.FootprintBytes})
		}
	}
	if known("lmstudio") {
		out.LMStudio = &protocol.LMStudio{}
		if l := s.LMStudio; l != nil {
			out.LMStudio = &protocol.LMStudio{Installed: true, Running: true, FootprintBytes: l.FootprintBytes}
			for _, m := range l.Models {
				out.LMStudio.Models = append(out.LMStudio.Models, protocol.LoadedModel{Key: m.Key, SizeBytes: m.SizeBytes})
			}
		}
	}
	if known("ollama") {
		out.Ollama = &protocol.Ollama{}
		if o := s.Ollama; o != nil {
			out.Ollama = &protocol.Ollama{Installed: true, Running: true, FootprintBytes: o.FootprintBytes}
			if o.ModelsUnknown {
				out.Ollama.ModelsError = "unknown when recorded"
			}
			for _, m := range o.Models {
				out.Ollama.Models = append(out.Ollama.Models, protocol.OllamaModel{Name: m.Name, SizeBytes: m.SizeBytes})
			}
		}
	}
	return out
}
