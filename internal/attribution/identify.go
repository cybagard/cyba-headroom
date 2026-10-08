package attribution

import (
	"slices"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// Caller is what the shim knows about who makes a call (R5, #28).
type Caller struct {
	// Worktree is the ID from the environment: HEADROOM_WORKTREE, else
	// ORCA_WORKTREE_ID.
	Worktree string
	// Cwd is the call's working directory, and RealCwd the same with
	// symlinks resolved ("" if no different).
	Cwd, RealCwd string
	// Ancestors are the call's parent PIDs, nearest first.
	Ancestors []int
}

// Identify finds the Orca worktree making a call: the environment's word,
// else the worktree holding the working directory (its git toplevel is the
// worktree's path), else the worktree whose terminal the call descends from.
// "" means a manual call. by says which of those decided.
func Identify(s *protocol.Snapshot, c Caller) (id, by string) {
	if c.Worktree != "" {
		return c.Worktree, protocol.IdentifiedByEnv
	}
	if s == nil || s.Orca == nil {
		return "", ""
	}
	wts := make([]Worktree, len(s.Orca.Worktrees))
	for i, w := range s.Orca.Worktrees {
		wts[i] = Worktree{ID: w.ID, Path: w.Path}
	}
	m := NewMatcher(wts)
	for _, p := range []string{c.Cwd, c.RealCwd} {
		if id := m.owner(p); id != "" {
			return id, protocol.IdentifiedByCwd
		}
	}
	for _, pid := range c.Ancestors {
		for _, w := range s.Orca.Worktrees {
			if slices.Contains(w.SessionPIDs, pid) {
				return w.ID, protocol.IdentifiedByProcess
			}
		}
	}
	return "", ""
}
