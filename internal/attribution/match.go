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

// MatchKeys finds k's worktree among wts. To match many items against the
// same worktrees, build a Matcher once.
func MatchKeys(wts []Worktree, k Keys) Match { return NewMatcher(wts).Match(k) }

// Matcher matches items against a fixed set of worktrees, prepared once.
type Matcher struct {
	wts []prepared
}

// prepared is a worktree with its path in comparable form and its directory
// name as words, or nil words when the name must not be used.
type prepared struct {
	id    string
	path  string
	words []string
}

// NewMatcher prepares wts for matching.
func NewMatcher(wts []Worktree) *Matcher {
	m := &Matcher{}
	count := map[string]int{}
	for _, w := range wts {
		count[dirName(w.Path)]++
	}
	for _, w := range wts {
		p := prepared{id: w.ID, path: key(w.Path)}
		if name := dirName(w.Path); count[name] == 1 && specific(name) {
			p.words = split(name)
		}
		m.wts = append(m.wts, p)
	}
	return m
}

// Match finds k's worktree. Evidence is tried strongest first; the first
// kind that points anywhere decides. If it points at two worktrees, the item
// is ambiguous rather than given to either.
func (m *Matcher) Match(k Keys) Match {
	for _, ev := range []struct {
		by    string
		paths []string
	}{
		{ByComposeDir, nonEmpty(k.ComposeDir)},
		{ByLaunchCwd, nonEmpty(k.LaunchCwd)},
		{ByMount, m.notGit(k.Mounts)},
		{BySharedDir, k.SharedDirs},
	} {
		if r, ok := decide(ev.by, m.pathMatches(ev.paths)); ok {
			return r
		}
	}
	if r, ok := decide(ByVMName, m.nameMatches(k.VMName)); ok {
		return r
	}
	return Match{Reason: NoMatch}
}

// nameMatches returns the worktrees whose directory name appears in vm as
// whole words (split on - _ .), ignoring case. The longest such name wins,
// so a VM named after project-a is not also claimed by project.
func (m *Matcher) nameMatches(vm string) []string {
	if vm == "" {
		return nil
	}
	words := split(vm)
	var ids []string
	longest := 0
	for _, w := range m.wts {
		if !containsRun(words, w.words) {
			continue
		}
		switch {
		case len(w.words) > longest:
			ids, longest = []string{w.id}, len(w.words)
		case len(w.words) == longest:
			ids = append(ids, w.id)
		}
	}
	return ids
}

// genericNames say nothing about which worktree a VM belongs to.
var genericNames = map[string]bool{
	"main": true, "master": true, "trunk": true, "dev": true, "develop": true,
	"test": true, "tests": true, "ci": true, "build": true, "src": true,
	"app": true, "work": true, "repo": true, "tmp": true, "temp": true,
	"mac": true, "macos": true, "linux": true, "vm": true,
}

// specific reports whether a directory name may name a VM: at least four
// characters, and not a generic word.
func specific(name string) bool {
	return len(name) >= 4 && !genericNames[name]
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

// notGit is mounts without those in a worktree's .git, unless that leaves
// none that point at a worktree. A repo's .git is shared by every linked
// worktree of the repo: a container mounts the main checkout's so git
// works in a linked one, and that says nothing about which (#94). With
// nothing else, it still names its repo (a git daemon).
func (m *Matcher) notGit(mounts []string) []string {
	var rest []string
	for _, p := range mounts {
		if !m.inGitDir(p) {
			rest = append(rest, p)
		}
	}
	if len(m.pathMatches(rest)) == 0 {
		return mounts
	}
	return rest
}

// inGitDir reports whether p is a worktree's .git or lies inside it.
func (m *Matcher) inGitDir(p string) bool {
	p = key(p)
	return p != "" && slices.ContainsFunc(m.wts, func(w prepared) bool {
		return w.path != "" && (p == w.path+"/.git" || strings.HasPrefix(p, w.path+"/.git/"))
	})
}

// pathMatches returns the distinct worktrees the paths lie in, in order.
func (m *Matcher) pathMatches(paths []string) []string {
	var ids []string
	for _, p := range paths {
		if id := m.owner(p); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// owner is the deepest worktree that p equals or lies under, on whole path
// segments; "" if none. Relative paths say nothing about the host.
func (m *Matcher) owner(p string) string {
	p = key(p)
	if p == "" {
		return ""
	}
	best, bestLen := "", -1
	for _, w := range m.wts {
		if w.path == "" || len(w.path) <= bestLen {
			continue
		}
		if p == w.path || strings.HasPrefix(p, w.path+"/") {
			best, bestLen = w.id, len(w.path)
		}
	}
	return best
}

// key is p in comparable form: canonical and lower-case, since macOS
// volumes are case-insensitive by default and a shell's spelling of a path
// need not match Orca's.
func key(p string) string { return strings.ToLower(canon(p)) }

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
