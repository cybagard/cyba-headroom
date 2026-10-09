// Package budget derives reserved vs. used vs. headroom from a snapshot (R2).
// It is pure arithmetic: the collectors measure, this package adds up.
package budget

import "github.com/cybagard/cyba-headroom/internal/protocol"

// Params are the budget inputs that are configured rather than measured.
type Params struct {
	// HostBaselineBytes is reserved for macOS, Orca, the agents' own processes
	// and everything else no component covers.
	HostBaselineBytes uint64
	// DockerOverheadBytes is the Docker VM's own cost beyond its containers:
	// guest kernel, dockerd and page cache (~1.6 GB idle in spike #9).
	DockerOverheadBytes uint64
	// LMStudioIdleBytes is LM Studio's footprint with no model loaded
	// (~620 MiB in #16).
	LMStudioIdleBytes uint64
	// OllamaIdleBytes is the Ollama server's footprint with no model loaded.
	OllamaIdleBytes uint64
}

// Compute returns the budget for s. A source with no reading yet is listed
// in Unknown and counts as 0; a stale source's last good reading is used and
// the source is listed in Stale.
func Compute(s *protocol.Snapshot, p Params) protocol.Budget {
	var b protocol.Budget
	// Each component's host cost is removed from host used to leave the
	// unaccounted rest; one unknown cost makes the rest unknown.
	footprintsKnown := true
	var footprints uint64
	add := func(c protocol.BudgetComponent) {
		b.Components = append(b.Components, c)
		b.ReservedBytes += c.ReservedBytes
		if c.UsedBytes == nil {
			footprintsKnown = false
		} else {
			footprints += *c.UsedBytes
		}
	}
	if s.Host == nil {
		b.Unknown = append(b.Unknown, "host")
	}
	if s.Docker != nil {
		add(docker(s.Docker, p))
	} else {
		b.Unknown = append(b.Unknown, "docker")
		footprintsKnown = false
	}
	if s.Tart != nil {
		add(tart(s.Tart))
	} else {
		b.Unknown = append(b.Unknown, "tart")
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
	if h := s.Host; h != nil && h.UsedBytes != nil && h.CompressorBytes != nil && h.CompressedBytes != nil && footprintsKnown {
		whole := int64(*h.UsedBytes) - int64(*h.CompressorBytes) + int64(*h.CompressedBytes)
		un := whole - int64(footprints)
		b.UnaccountedBytes = &un
		used := uint64(max(un, 0))
		base.UsedBytes = &used
	}
	b.Components = append(b.Components, base)
	b.ReservedBytes += base.ReservedBytes

	for _, name := range []string{"host", "docker", "tart", "lmstudio", "ollama"} {
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

// tart reserves each running VM's configured memory (R2), which the VM grows
// into and does not give back, or its footprint when the hypervisor's
// overhead puts it above that.
func tart(t *protocol.Tart) protocol.BudgetComponent {
	c := protocol.BudgetComponent{Name: "tart"}
	var used uint64
	known := true
	for _, vm := range t.VMs {
		r := vm.MemoryBytes
		if vm.FootprintBytes == nil {
			known = false
		} else {
			used += *vm.FootprintBytes
			r = max(r, *vm.FootprintBytes)
		}
		c.ReservedBytes += r
	}
	if known {
		c.UsedBytes = &used
	}
	return c
}

// docker reserves what the VM already holds or what its containers need plus
// the VM's overhead, whichever is larger. Not the VM's memory limit: that is a
// ceiling it rarely reaches, and reserving it would leave no headroom (spike
// #9). The VM keeps its high-water mark until Resource Saver stops it, so that
// memory counts as spent.
func docker(d *protocol.Docker, p Params) protocol.BudgetComponent {
	c := protocol.BudgetComponent{Name: "docker"}
	if !d.Running || (d.VMError == "" && !d.VMRunning) {
		c.UsedBytes = new(uint64)
		return c
	}
	need := p.DockerOverheadBytes
	for _, ct := range d.Containers {
		need += ct.MemoryBytes
	}
	c.ReservedBytes = need
	if d.VMError == "" {
		used := d.VMFootprintBytes
		c.UsedBytes = &used
		c.ReservedBytes = max(need, used)
	}
	return c
}

// lmstudio reserves LM Studio itself plus its loaded models, even when idle
// (R2): their file sizes, or its whole footprint when context and runtime add
// more, or while a model loads before lms ps lists it.
func lmstudio(l *protocol.LMStudio, p Params) protocol.BudgetComponent {
	var sizes uint64
	for _, m := range l.Models {
		sizes = addSize(sizes, m.SizeBytes)
	}
	return modelServer("lmstudio", l.Running, l.FootprintBytes, p.LMStudioIdleBytes, sizes)
}

// ollama reserves the Ollama server plus its loaded models, as lmstudio
// does, and its whole footprint while the model list is unknown.
func ollama(o *protocol.Ollama, p Params) protocol.BudgetComponent {
	var sizes uint64
	for _, m := range o.Models {
		sizes = addSize(sizes, m.SizeBytes)
	}
	return modelServer("ollama", o.Running, o.FootprintBytes, p.OllamaIdleBytes, sizes)
}

// maxSizes bounds a model server's summed model sizes, as policy's maxBytes
// bounds a request: far above any Mac's memory, so a bogus size (another
// user's process may answer /api/ps) only shrinks headroom and the budget's
// arithmetic cannot wrap.
const maxSizes = uint64(1) << 42

// addSize adds a model's size to sizes (at most maxSizes), saturating at
// maxSizes.
func addSize(sizes, size uint64) uint64 {
	if size >= maxSizes-sizes {
		return maxSizes
	}
	return sizes + size
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
