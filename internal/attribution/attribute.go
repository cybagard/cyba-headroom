package attribution

import "github.com/cybagard/cyba-headroom/internal/protocol"

// OrcaUnknown marks everything unattributed while Orca's worktrees are
// unknown (no reading, or Orca not running): no match would mean nothing.
const OrcaUnknown = "orca_unknown"

// Attribute assigns s's containers and Tart VMs to its live worktrees.
func Attribute(s *protocol.Snapshot) protocol.Attribution { return attribute(s, nil) }

// AttributeLeased is Attribute, but an item the evidence matches to no
// worktree goes to the worktree leased names for its key (container:<ID>,
// vm:<name>), the worktree of the lease it bound, if that one is live (#74).
func AttributeLeased(s *protocol.Snapshot, leased map[string]string) protocol.Attribution {
	return attribute(s, leased)
}

func attribute(s *protocol.Snapshot, leased map[string]string) protocol.Attribution {
	a := protocol.Attribution{Worktrees: []protocol.WorktreeUsage{}, OrcaStale: s.Sources["orca"].Stale}
	known := s.Orca != nil && s.Orca.Running
	var wts []Worktree
	index := map[string]int{}
	if known {
		for i, w := range s.Orca.Worktrees {
			wts = append(wts, Worktree{ID: w.ID, Path: w.Path})
			index[w.ID] = i
			wu := protocol.WorktreeUsage{ID: w.ID, Path: w.Path, Name: w.Name}
			if s.Orca.MemoryError == "" {
				mem, cpu := w.MemoryBytes, w.CPUPercent
				wu.AgentMemoryBytes, wu.AgentCPUPercent = &mem, &cpu
			}
			a.Worktrees = append(a.Worktrees, wu)
		}
	}
	m := NewMatcher(wts)
	match := func(key string, k Keys) Match {
		if !known {
			return Match{Reason: OrcaUnknown}
		}
		r := m.Match(k)
		if _, live := index[leased[key]]; r.Reason == NoMatch && live {
			return Match{WorktreeID: leased[key], By: ByLease}
		}
		return r
	}
	// usage is where a match lands: its worktree, or the unattributed set.
	usage := func(m Match) *protocol.Usage {
		if m.WorktreeID == "" {
			return &a.Unattributed
		}
		return &a.Worktrees[index[m.WorktreeID]].Usage
	}

	if s.Docker != nil {
		for _, c := range s.Docker.Containers {
			m := match("container:"+c.ID, Keys{ComposeDir: c.Labels[protocol.ComposeWorkingDirLabel], Mounts: c.Mounts})
			u := usage(m)
			u.Containers = append(u.Containers, protocol.AttributedContainer{
				ID: c.ID, Name: c.Name, MemoryBytes: c.MemoryBytes, CPUPercent: c.CPUPercent,
				By: m.By, Reason: m.Reason,
			})
		}
	}
	if s.Tart != nil {
		for _, vm := range s.Tart.VMs {
			m := match("vm:"+vm.Name, Keys{LaunchCwd: vm.LaunchCwd, SharedDirs: vm.SharedDirs, VMName: vm.Name})
			u := usage(m)
			u.TartVMs = append(u.TartVMs, protocol.AttributedVM{
				Name: vm.Name, MemoryBytes: vm.MemoryBytes, FootprintBytes: vm.FootprintBytes,
				By: m.By, Reason: m.Reason,
			})
		}
	}
	for i := range a.Worktrees {
		total(&a.Worktrees[i].Usage)
	}
	total(&a.Unattributed)
	return a
}

// total fills u's totals. A CPU or footprint total is known only if every
// part is. Lists are empty rather than null on the wire.
func total(u *protocol.Usage) {
	if u.Containers == nil {
		u.Containers = []protocol.AttributedContainer{}
	}
	if u.TartVMs == nil {
		u.TartVMs = []protocol.AttributedVM{}
	}
	cpu, cpuKnown := 0.0, true
	for _, c := range u.Containers {
		u.ContainerMemoryBytes += c.MemoryBytes
		if c.CPUPercent == nil {
			cpuKnown = false
		} else {
			cpu += *c.CPUPercent
		}
	}
	if cpuKnown {
		u.ContainerCPUPercent = &cpu
	}
	var fp uint64
	fpKnown := true
	for _, vm := range u.TartVMs {
		u.TartMemoryBytes += vm.MemoryBytes
		if vm.FootprintBytes == nil {
			fpKnown = false
		} else {
			fp += *vm.FootprintBytes
		}
	}
	if fpKnown {
		u.TartFootprintBytes = &fp
	}
}
