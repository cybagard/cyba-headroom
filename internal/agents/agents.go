// Package agents finds the coding agents running in a worktree's terminals
// whose PATH does not put headroom's shims first (#297): their docker,
// podman and tart calls run ungated.
package agents

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cybagard/cyba-headroom/internal/binpath"
	"github.com/cybagard/cyba-headroom/internal/shim"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// Procs is the process access the detector needs; vmproc.Host on macOS.
type Procs interface {
	// Tree is a process and its descendants, with their arguments.
	Tree(root int) ([]vmproc.Process, error)
	// PathOf is a process's PATH, and no other variable.
	PathOf(pid int) (string, error)
}

// Agent is a running agent whose PATH puts real tools ahead of the shims.
type Agent struct {
	PID  int
	Name string
	// Tools are those it runs without the shims.
	Tools []string
}

// known are the agents' names.
var known = []string{"claude", "codex", "kilo", "opencode"}

// hosts run an agent's script, named in argv[1].
var hosts = []string{"node", "bun", "deno"}

// Name is the agent a process's arguments run, or "" for none. It reads
// argv[0], not the executable: claude's is a file named for its version.
func Name(args []string) string {
	if len(args) == 0 {
		return ""
	}
	n := filepath.Base(args[0])
	if slices.Contains(hosts, n) {
		if len(args) < 2 {
			return ""
		}
		n = filepath.Base(args[1])
		for _, ext := range []string{".js", ".mjs", ".cjs"} {
			n = strings.TrimSuffix(n, ext)
		}
	}
	if slices.Contains(known, n) {
		return n
	}
	return ""
}

// Unshimmed lists the agents among the descendants of sessions (a
// worktree's terminals) that run any of tools without the shims in
// shimDir: the first on their PATH is a real binary. A tool missing from
// that PATH does not count. A process that exits or cannot be read is
// skipped, and the walk stops when ctx ends. Only PATH is read from an
// environment, and it is not kept.
func Unshimmed(ctx context.Context, procs Procs, sessions []int, shimDir string, tools []string) []Agent {
	var out []Agent
	seen := map[int]bool{}
	for _, s := range sessions {
		if ctx.Err() != nil {
			return out
		}
		tree, err := procs.Tree(s)
		if err != nil {
			continue
		}
		for _, p := range tree {
			name := Name(p.Args)
			if name == "" || seen[p.PID] {
				continue
			}
			seen[p.PID] = true
			if ctx.Err() != nil {
				return out
			}
			path, err := procs.PathOf(p.PID)
			if err != nil {
				continue
			}
			if bypassed := bypassing(path, shimDir, tools); len(bypassed) > 0 {
				out = append(out, Agent{PID: p.PID, Name: name, Tools: bypassed})
			}
		}
	}
	return out
}

// bypassing are the tools whose first hit on path is a real binary, not
// a shim.
func bypassing(path, shimDir string, tools []string) []string {
	getenv := func(k string) string {
		if k == "PATH" {
			return path
		}
		return ""
	}
	var out []string
	for _, t := range tools {
		first := binpath.Search(t, getenv, nil, nil)
		if first == "" || shim.LeadsToHeadroom(first) || filepath.Dir(first) == filepath.Clean(shimDir) {
			continue
		}
		out = append(out, t)
	}
	return out
}
