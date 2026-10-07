// Package attribution assigns containers and Tart VMs to Orca worktrees (R3).
// The worktree path is the join key. Matching works on plain keys, so the
// same rules apply to live snapshots and to recorded samples (#23).
package attribution

import (
	"path/filepath"
	"slices"
	"strings"
)

// Worktree is a live worktree to match against.
type Worktree struct{ ID, Path string }

// Keys are what is known about one container or VM.
type Keys struct {
	// ComposeDir is the compose project's working dir (containers).
	ComposeDir string
	// LaunchCwd is the cwd of the process that started it (Tart: the parent
	// of `tart run`).
	LaunchCwd string
	// Mounts are bind-mount host paths (containers).
	Mounts []string
	// SharedDirs are --dir host paths (Tart).
	SharedDirs []string
	// VMName is the Tart VM name, for the naming rule.
	VMName string
}

// How a match was made, strongest evidence first.
const (
	ByComposeDir = "compose_dir"
	ByLaunchCwd  = "launch_cwd"
	ByMount      = "mount"
	BySharedDir  = "shared_dir"
	ByVMName     = "vm_name"
)

// Why something is unattributed.
const (
	NoMatch   = "no_match"
	Ambiguous = "ambiguous"
)

// Match is the outcome for one item: a worktree and the evidence, or a reason
// it stays unattributed.
type Match struct {
	WorktreeID string
	By         string
	Reason     string
}

// MatchKeys finds k's worktree. Evidence is tried strongest first; the first
// kind that points anywhere decides. If it points at two worktrees, the item
// is ambiguous rather than given to either.
func MatchKeys(wts []Worktree, k Keys) Match {
	for _, ev := range []struct {
		by    string
		paths []string
	}{
		{ByComposeDir, nonEmpty(k.ComposeDir)},
		{ByLaunchCwd, nonEmpty(k.LaunchCwd)},
		{ByMount, k.Mounts},
		{BySharedDir, k.SharedDirs},
	} {
		if m, ok := decide(ev.by, pathMatches(wts, ev.paths)); ok {
			return m
		}
	}
	if m, ok := decide(ByVMName, nameMatches(wts, k.VMName)); ok {
		return m
	}
	return Match{Reason: NoMatch}
}

// nameMatches returns the worktrees whose directory name appears in vm as
// whole words (split on - _ .), ignoring case. The longest such name wins,
// so a VM named after project-a is not also claimed by project. A directory
// name that several live worktrees share (main, say) says nothing and is
// skipped.
func nameMatches(wts []Worktree, vm string) []string {
	if vm == "" {
		return nil
	}
	count := map[string]int{}
	for _, w := range wts {
		count[dirName(w.Path)]++
	}
	words := split(vm)
	var ids []string
	longest := 0
	for _, w := range wts {
		name := dirName(w.Path)
		if name == "" || count[name] > 1 {
			continue
		}
		nw := split(name)
		if !containsRun(words, nw) {
			continue
		}
		switch {
		case len(nw) > longest:
			ids, longest = []string{w.ID}, len(nw)
		case len(nw) == longest:
			ids = append(ids, w.ID)
		}
	}
	return ids
}

func dirName(p string) string {
	if p = canon(p); p == "" || p == "/" {
		return ""
	}
	return strings.ToLower(filepath.Base(p))
}

func split(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == '-' || r == '_' || r == '.' })
}

// containsRun reports whether run occurs in words as consecutive elements.
func containsRun(words, run []string) bool {
	if len(run) == 0 {
		return false
	}
	for i := 0; i+len(run) <= len(words); i++ {
		if slices.Equal(words[i:i+len(run)], run) {
			return true
		}
	}
	return false
}

// decide turns the worktrees one kind of evidence points at into a match.
func decide(by string, ids []string) (Match, bool) {
	switch {
	case len(ids) == 0:
		return Match{}, false
	case len(ids) > 1:
		return Match{Reason: Ambiguous}, true
	}
	return Match{WorktreeID: ids[0], By: by}, true
}

// pathMatches returns the distinct worktrees the paths lie in, in order.
func pathMatches(wts []Worktree, paths []string) []string {
	var ids []string
	for _, p := range paths {
		if id := owner(wts, p); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// owner is the deepest worktree that p equals or lies under, on whole path
// segments; "" if none. Relative paths say nothing about the host.
func owner(wts []Worktree, p string) string {
	p = canon(p)
	if p == "" {
		return ""
	}
	best, bestLen := "", -1
	for _, w := range wts {
		wp := canon(w.Path)
		if wp == "" || len(wp) <= bestLen {
			continue
		}
		if p == wp || strings.HasPrefix(p, wp+"/") {
			best, bestLen = w.ID, len(wp)
		}
	}
	return best
}

// canon cleans an absolute path and folds macOS's /private aliases, so
// /private/tmp/x and /tmp/x compare equal. No filesystem access.
func canon(p string) string {
	if !filepath.IsAbs(p) {
		return ""
	}
	p = filepath.Clean(p)
	for _, d := range []string{"/tmp", "/var", "/etc"} {
		if p == "/private"+d || strings.HasPrefix(p, "/private"+d+"/") {
			return strings.TrimPrefix(p, "/private")
		}
	}
	return p
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}
