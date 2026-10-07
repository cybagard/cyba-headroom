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
}

// Compute returns the budget for s. A source with no reading yet is listed
// in Unknown and counts as 0; a stale source's last good reading is used.
func Compute(s *protocol.Snapshot, p Params) protocol.Budget {
	var b protocol.Budget
	// footprints sums the components' host cost; nil once one is unknown.
	footprints := new(uint64)
	add := func(name string, present bool, c func() protocol.BudgetComponent) {
		if !present {
			b.Unknown = append(b.Unknown, name)
			footprints = nil
			return
		}
		comp := c()
		b.Components = append(b.Components, comp)
		b.ReservedBytes += comp.ReservedBytes
		if comp.UsedBytes == nil {
			footprints = nil
		} else if footprints != nil {
			*footprints += *comp.UsedBytes
		}
	}
	if s.Host == nil {
		b.Unknown = append(b.Unknown, "host")
	}
	add("docker", s.Docker != nil, func() protocol.BudgetComponent { return docker(s.Docker, p) })
	add("tart", s.Tart != nil, func() protocol.BudgetComponent { return tart(s.Tart) })
	add("lmstudio", s.LMStudio != nil, func() protocol.BudgetComponent { return lmstudio(s.LMStudio, p) })

	base := protocol.BudgetComponent{Name: "host_baseline", ReservedBytes: p.HostBaselineBytes}
	if s.Host != nil {
		b.TotalBytes = s.Host.TotalBytes
		b.UsedBytes = s.Host.UsedBytes
		if b.UsedBytes != nil && footprints != nil {
			un := int64(*b.UsedBytes) - int64(*footprints)
			b.UnaccountedBytes = &un
			used := uint64(max(un, 0))
			base.UsedBytes = &used
		}
	}
	b.Components = append(b.Components, base)
	b.ReservedBytes += base.ReservedBytes
	b.HeadroomBytes = int64(b.TotalBytes) - int64(b.ReservedBytes)
	return b
}

// tart reserves each running VM's configured memory (R2): a VM grows into it
// and does not give it back.
func tart(t *protocol.Tart) protocol.BudgetComponent {
	c := protocol.BudgetComponent{Name: "tart"}
	used := new(uint64)
	for _, vm := range t.VMs {
		c.ReservedBytes += vm.MemoryBytes
		if vm.FootprintBytes == nil {
			used = nil
		} else if used != nil {
			*used += *vm.FootprintBytes
		}
	}
	c.UsedBytes = used
	return c
}

// docker reserves what the VM already holds or what its containers need plus
// the VM's overhead, whichever is larger. Not the VM's memory limit: that is a
// ceiling it rarely reaches, and reserving it would leave no headroom (spike
// #9). The VM keeps its high-water mark until Resource Saver stops it, so that
// memory counts as spent.
func docker(d *protocol.Docker, p Params) protocol.BudgetComponent {
	c := protocol.BudgetComponent{Name: "docker"}
	if !d.Running {
		c.UsedBytes = new(uint64)
		return c
	}
	if d.VMError == "" {
		if !d.VMRunning {
			c.UsedBytes = new(uint64)
			return c
		}
		used := d.VMFootprintBytes
		c.UsedBytes = &used
	}
	need := p.DockerOverheadBytes
	for _, ct := range d.Containers {
		need += ct.MemoryBytes
	}
	c.ReservedBytes = need
	if c.UsedBytes != nil {
		c.ReservedBytes = max(need, *c.UsedBytes)
	}
	return c
}

// lmstudio reserves loaded models even when idle (R2): their file size, or
// what LM Studio costs beyond its idle self when context and runtime add more.
func lmstudio(l *protocol.LMStudio, p Params) protocol.BudgetComponent {
	c := protocol.BudgetComponent{Name: "lmstudio"}
	if !l.Running {
		c.UsedBytes = new(uint64)
		return c
	}
	c.UsedBytes = l.FootprintBytes
	if len(l.Models) == 0 {
		return c
	}
	for _, m := range l.Models {
		c.ReservedBytes += m.SizeBytes
	}
	if fp := l.FootprintBytes; fp != nil && *fp > p.LMStudioIdleBytes {
		c.ReservedBytes = max(c.ReservedBytes, *fp-p.LMStudioIdleBytes)
	}
	return c
}
