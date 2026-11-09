// Package budget derives reserved vs. used vs. headroom from a snapshot (R2).
// It is pure arithmetic: the collectors measure, this package adds up.
package budget

import (
	"cmp"
	"slices"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/units"
)

// Params are the budget inputs that are configured rather than measured.
type Params struct {
	// HostBaselineBytes is reserved for macOS, Orca, the agents' own processes
	// and everything else no component covers.
	HostBaselineBytes uint64
	// DockerOverheadBytes is the Docker VM's own cost beyond its containers:
	// guest kernel, dockerd and page cache (~1.6 GB idle in spike #9).
	DockerOverheadBytes uint64
	// PodmanOverheadBytes is a Podman machine's VM's own cost beyond its
	// containers (~1 GB idle for a 2 GiB machine in docs/spikes/podman.md).
	PodmanOverheadBytes uint64
	// LMStudioIdleBytes is LM Studio's footprint with no model loaded
	// (~620 MiB in #16).
	LMStudioIdleBytes uint64
	// OllamaIdleBytes is the Ollama server's footprint with no model loaded.
	OllamaIdleBytes uint64
	// DefaultTartBytes is reserved for a running Tart VM whose configured
	// memory and footprint are both unknown: the shim's cost for an unknown
	// tart run (policy.Config.DefaultTartBytes).
	DefaultTartBytes uint64
}

// Compute returns the budget for s. A source with no reading yet is listed
// in Unknown and counts as 0; a stale source's last good reading is used and
// the source is listed in Stale. Every sum saturates at units.MaxBytes, so a
// bogus size an engine or VM reports only shrinks headroom (#275).
func Compute(s *protocol.Snapshot, p Params) protocol.Budget {
	var b protocol.Budget
	// Each component's host cost is removed from host used to leave the
	// unaccounted rest; one unknown cost makes the rest unknown.
	footprintsKnown := true
	var footprints uint64
	vc := &vmCost{ratio: compressionRatio(s.Host)}
	add := func(c protocol.BudgetComponent) {
		b.Components = append(b.Components, c)
		b.ReservedBytes = units.AddSat(b.ReservedBytes, c.ReservedBytes)
		if c.UsedBytes == nil {
			footprintsKnown = false
		} else {
			footprints = units.AddSat(footprints, *c.UsedBytes)
		}
	}
	if s.Host == nil {
		b.Unknown = append(b.Unknown, "host")
	}
	if s.Engines != nil {
		for _, e := range s.Engines {
			c := docker(e, p, vc)
			c.Name = engineName(e, s.Engines)
			add(c)
		}
	} else {
		b.Unknown = append(b.Unknown, "docker")
		footprintsKnown = false
	}
	if s.Tart != nil {
		add(tart(s.Tart, p, vc))
	} else {
		b.Unknown = append(b.Unknown, "tart")
		footprintsKnown = false
	}
	// The podman section is absent while no machine runs, so only the
	// source's status tells "none running" from "no reading yet". A VM
	// whose engine is read is that engine's component's (#228).
	if s.Podman != nil {
		if c, ok := podman(s.Podman, s.Engines, vc); ok {
			add(c)
		}
	} else if st, ok := s.Sources["podman"]; ok && st.At.IsZero() {
		b.Unknown = append(b.Unknown, "podman")
		footprintsKnown = false
	}
	// Likewise the vms section is absent while every VM has an owner.
	if s.VMs != nil {
		add(otherVMs(s.VMs, vc))
	} else if st, ok := s.Sources["vms"]; ok && st.At.IsZero() {
		b.Unknown = append(b.Unknown, "vms")
		footprintsKnown = false
	}
	if s.LMStudio != nil {
		add(lmstudio(s.LMStudio, p))
	} else {
		b.Unknown = append(b.Unknown, "lmstudio")
		footprintsKnown = false
	}
	if s.Ollama != nil {
		add(ollama(s.Ollama, p))
	} else {
		b.Unknown = append(b.Unknown, "ollama")
		footprintsKnown = false
	}

	base := protocol.BudgetComponent{Name: "host_baseline", ReservedBytes: p.HostBaselineBytes}
	// Footprints count compressed pages at full size, host used at their
	// compressed size: swap the compressor's share for its full size first.
	// A VM's used memory counts its compressed share at compressed size
	// (#311), so that share stays compressed here too.
	if h := s.Host; h != nil && h.UsedBytes != nil && h.CompressorBytes != nil && h.CompressedBytes != nil && footprintsKnown {
		whole := int64(*h.UsedBytes) - int64(*h.CompressorBytes) + int64(*h.CompressedBytes) - int64(vc.credited)
		un := whole - int64(footprints)
		b.UnaccountedBytes = &un
		used := uint64(max(un, 0))
		base.UsedBytes = &used
	}
	b.Components = append(b.Components, base)
	b.ReservedBytes = units.AddSat(b.ReservedBytes, base.ReservedBytes)

	for _, name := range []string{"host", "docker", "tart", "podman", "vms", "lmstudio", "ollama"} {
		if s.Sources[name].Stale {
			b.Stale = append(b.Stale, name)
		}
	}

	// Without the host's total there is nothing to subtract from: headroom is
	// unknown, not -reserved.
	if s.Host != nil {
		b.TotalBytes = s.Host.TotalBytes
		headroom := int64(b.TotalBytes) - int64(b.ReservedBytes)
		b.HeadroomBytes = &headroom
	}
	return b
}

// vmCost credits VMs with what macOS saves by compressing them (#311). A
// VM's compressed share, its swapped bytes, costs the host its full size
// divided by the compressor's ratio, compressed ÷ compressor, host-wide.
type vmCost struct {
	// ratio is at least 1; 1 when unknown, which credits nothing.
	ratio float64
	// credited is what every VM's used memory was credited, in all.
	credited uint64
}

// compressionRatio is the compressor's ratio, compressed ÷ compressor; 1
// when either is unknown or the compressor is empty, and at least 1.
func compressionRatio(h *protocol.Host) float64 {
	if h == nil || h.CompressorBytes == nil || h.CompressedBytes == nil || *h.CompressorBytes == 0 {
		return 1
	}
	return max(float64(*h.CompressedBytes)/float64(*h.CompressorBytes), 1)
}

// credit returns what compression saves of a VM whose footprint is fp:
// its swapped share (nil = unknown, no credit), at most fp, less that share
// at the compressor's ratio. It counts toward the credited total.
func (v *vmCost) credit(swapped *uint64, fp uint64) uint64 {
	if swapped == nil || v.ratio <= 1 {
		return 0
	}
	sw := min(*swapped, fp)
	c := sw - uint64(float64(sw)/v.ratio)
	v.credited = units.AddSat(v.credited, c)
	return c
}

// tart reserves each running VM's configured memory (R2), which the VM grows
// into and does not give back, or its footprint when the hypervisor's
// overhead puts it above that. One of unknown config reserves at least the
// default (protocol.TartVM.SizeBytes, #277). Its compressed share is
// credited (vmCost).
func tart(t *protocol.Tart, p Params, vc *vmCost) protocol.BudgetComponent {
	c := protocol.BudgetComponent{Name: "tart"}
	var used uint64
	known := true
	for _, vm := range t.VMs {
		r := min(vm.SizeBytes(p.DefaultTartBytes), units.MaxBytes)
		if vm.FootprintBytes == nil {
			known = false
		} else {
			fp := min(*vm.FootprintBytes, units.MaxBytes)
			saved := vc.credit(vm.SwappedBytes, fp)
			used = units.AddSat(used, fp-saved)
			r = max(r, fp) - saved
		}
		c.ReservedBytes = units.AddSat(c.ReservedBytes, r)
	}
	if known {
		c.UsedBytes = &used
	}
	return c
}

// podman reserves what the Podman machines' VMs cost the host now. Their
// containers live inside that footprint, so it is the whole cost; the small
// vfkit and gvproxy processes beside each VM stay unaccounted. A VM whose
// machine's engine runs and has read that VM is left to the engine's
// component (#228); with none left, there is no component. One the engine
// could not read (vm_error) stays here too, as one with no engine does: for
// that tick it counts twice, which errs toward a deny and never reserves
// too little (#228, plan amendment 1). Their compressed share is credited
// (vmCost).
func podman(p *protocol.Podman, engines []protocol.Engine, vc *vmCost) (protocol.BudgetComponent, bool) {
	var fp uint64
	n := 0
	for _, vm := range p.VMs {
		if slices.ContainsFunc(engines, func(e protocol.Engine) bool {
			return e.Running && e.Machine == vm.Machine && e.VMError == "" && e.VMRunning
		}) {
			continue
		}
		fp = units.AddSat(fp, vm.FootprintBytes-vc.credit(vm.SwappedBytes, vm.FootprintBytes))
		n++
	}
	return protocol.BudgetComponent{Name: "podman", ReservedBytes: fp, UsedBytes: &fp}, n > 0
}

// otherVMs reserves what the VMs no known owner claims cost the host now:
// nothing tells how far they will grow. Owned VMs are never among them.
// Their compressed share is credited (vmCost).
func otherVMs(v *protocol.VMs, vc *vmCost) protocol.BudgetComponent {
	var fp uint64
	for _, vm := range v.Other {
		fp = units.AddSat(fp, vm.FootprintBytes-vc.credit(vm.SwappedBytes, vm.FootprintBytes))
	}
	return protocol.BudgetComponent{Name: "other_vms", ReservedBytes: fp, UsedBytes: &fp}
}

// engineName names an engine's component by its kind, and its socket too
// when another engine is of that kind (#227).
func engineName(e protocol.Engine, engines []protocol.Engine) string {
	kind := func(e protocol.Engine) string { return cmp.Or(e.Kind, protocol.EngineDocker) }
	n := 0
	for _, o := range engines {
		if kind(o) == kind(e) {
			n++
		}
	}
	if n > 1 {
		return kind(e) + " " + e.Socket
	}
	return kind(e)
}

// docker reserves the largest of what an engine's VM already holds, what its
// containers need plus the VM's overhead, and the VM's memory limit (/info
// MemTotal): a fresh VM grows to its limit within the hour (#311). The VM
// keeps its high-water mark until Resource Saver stops it, so that memory
// counts as spent; its compressed share is credited (vmCost). An engine with
// no VM costs nothing only while no container runs: one that runs some has a
// VM headroom did not find (#268).
func docker(d protocol.Engine, p Params, vc *vmCost) protocol.BudgetComponent {
	var c protocol.BudgetComponent
	if !d.Running || (d.VMError == "" && !d.VMRunning && len(d.Containers) == 0) {
		c.UsedBytes = new(uint64)
		return c
	}
	need := p.DockerOverheadBytes
	if d.Kind == protocol.EnginePodman {
		need = p.PodmanOverheadBytes
	}
	for _, ct := range d.Containers {
		need = units.AddSat(need, ct.MemoryBytes)
	}
	c.ReservedBytes = max(need, min(d.VMLimitBytes, units.MaxBytes))
	if d.VMError == "" && d.VMRunning {
		fp := min(d.VMFootprintBytes, units.MaxBytes)
		saved := vc.credit(d.VMSwappedBytes, fp)
		used := fp - saved
		c.UsedBytes = &used
		c.ReservedBytes = max(c.ReservedBytes, fp) - saved
	}
	return c
}

// lmstudio reserves LM Studio itself plus its loaded models, even when idle
// (R2): their file sizes, or its whole footprint when context and runtime add
// more, or while a model loads before lms ps lists it.
func lmstudio(l *protocol.LMStudio, p Params) protocol.BudgetComponent {
	var sizes uint64
	for _, m := range l.Models {
		sizes = units.AddSat(sizes, m.SizeBytes)
	}
	return modelServer("lmstudio", l.Running, l.FootprintBytes, p.LMStudioIdleBytes, sizes)
}

// ollama reserves the Ollama server plus its loaded models, as lmstudio
// does, and its whole footprint while the model list is unknown.
func ollama(o *protocol.Ollama, p Params) protocol.BudgetComponent {
	var sizes uint64
	for _, m := range o.Models {
		sizes = units.AddSat(sizes, m.SizeBytes)
	}
	return modelServer("ollama", o.Running, o.FootprintBytes, p.OllamaIdleBytes, sizes)
}

// modelServer reserves a model server's idle size plus its loaded models'
// sizes, or its footprint when that is more. Stopped, it costs nothing.
func modelServer(name string, running bool, footprint *uint64, idle, sizes uint64) protocol.BudgetComponent {
	c := protocol.BudgetComponent{Name: name}
	if !running {
		c.UsedBytes = new(uint64)
		return c
	}
	c.UsedBytes = footprint
	c.ReservedBytes = idle + sizes
	if footprint != nil {
		c.ReservedBytes = max(c.ReservedBytes, *footprint)
	}
	return c
}
