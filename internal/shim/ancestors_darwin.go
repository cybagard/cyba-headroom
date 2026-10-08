//go:build darwin

package shim

import (
	"os"

	"golang.org/x/sys/unix"
)

// maxAncestors bounds the walk up the process tree.
const maxAncestors = 32

// Ancestors returns this process's parent PIDs, nearest first, up to launchd
// (1). The daemon matches them against Orca's terminals to find the calling
// worktree (#28). A failed read ends the walk early.
func Ancestors() []int {
	var out []int
	for pid := os.Getppid(); pid > 0 && len(out) < maxAncestors; {
		out = append(out, pid)
		if pid == 1 {
			break
		}
		k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			break
		}
		pid = int(k.Eproc.Ppid)
	}
	return out
}
