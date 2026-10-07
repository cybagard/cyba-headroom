package attribution

import "github.com/cybagard/cyba-headroom/internal/protocol"

// OrcaUnknown marks everything unattributed while Orca has no reading: with
// no worktrees to match, no match means nothing.
const OrcaUnknown = "orca_unknown"

// composeDirLabel is the label Docker Compose sets to the project directory.
const composeDirLabel = "com.docker.compose.project.working_dir"

// Attribute assigns s's containers and Tart VMs to its live worktrees.
func Attribute(s *protocol.Snapshot) protocol.Attribution {
	a := protocol.Attribution{Worktrees: []protocol.WorktreeUsage{}}
	var wts []Worktree
	index := map[string]int{}
	if s.Orca != nil {
		for i, w := range s.Orca.Worktrees {
			wts = append(wts, Worktree{ID: w.ID, Path: w.Path})
			index[w.ID] = i
			a.Worktrees = append(a.Worktrees, protocol.WorktreeUsage{
				ID: w.ID, Path: w.Path, Name: w.Name,
				AgentMemoryBytes: w.MemoryBytes, AgentCPUPercent: w.CPUPercent,
			})
		}
	}
	// usage is where a match lands: its worktree, or the unattributed set.
	usage := func(m Match) *protocol.Usage {
		if m.WorktreeID == "" {
			return &a.Unattributed
		}
		return &a.Worktrees[index[m.WorktreeID]].Usage
	}
	match := func(k Keys) Match {
		if s.Orca == nil {
			return Match{Reason: OrcaUnknown}
		}
		return MatchKeys(wts, k)
	}

	if s.Docker != nil {
		for _, c := range s.Docker.Containers {
			m := match(Keys{ComposeDir: c.Labels[composeDirLabel], Mounts: c.Mounts})
			addContainer(usage(m), protocol.AttributedContainer{
				ID: c.ID, Name: c.Name, MemoryBytes: c.MemoryBytes, CPUPercent: c.CPUPercent,
				By: m.By, Reason: m.Reason,
			})
		}
	}
	if s.Tart != nil {
		for _, vm := range s.Tart.VMs {
			m := match(Keys{LaunchCwd: vm.LaunchCwd, SharedDirs: vm.SharedDirs, VMName: vm.Name})
			addVM(usage(m), protocol.AttributedVM{
				Name: vm.Name, MemoryBytes: vm.MemoryBytes, FootprintBytes: vm.FootprintBytes,
				By: m.By, Reason: m.Reason,
			})
		}
	}
	return a
}

func addContainer(u *protocol.Usage, c protocol.AttributedContainer) {
	u.Containers = append(u.Containers, c)
	u.ContainerMemoryBytes += c.MemoryBytes
	if c.CPUPercent != nil {
		u.ContainerCPUPercent += *c.CPUPercent
	}
}

// addVM adds a VM. The footprint total stays known only while every VM's is.
func addVM(u *protocol.Usage, vm protocol.AttributedVM) {
	first := len(u.TartVMs) == 0
	u.TartVMs = append(u.TartVMs, vm)
	u.TartMemoryBytes += vm.MemoryBytes
	switch {
	case vm.FootprintBytes == nil:
		u.TartFootprintBytes = nil
	case first:
		fp := *vm.FootprintBytes
		u.TartFootprintBytes = &fp
	case u.TartFootprintBytes != nil:
		fp := *u.TartFootprintBytes + *vm.FootprintBytes
		u.TartFootprintBytes = &fp
	}
}
