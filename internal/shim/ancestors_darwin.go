//go:build darwin

package shim

import (
	"fmt"
	"os"
	"strconv"

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

// Self names this process for HEADROOM_SHIM_CHECKED: its PID and start
// time, which an exec keeps and a reused PID does not.
func Self() string { return selfOf(os.Getpid()) }

// ParentSelf is the parent's Self: a provider that podman runs as its
// child carries the marker of the shim that exec'd into podman (#354).
func ParentSelf() string { return selfOf(os.Getppid()) }

// selfOf names process pid as Self does, or by its PID alone if its start
// time cannot be read.
func selfOf(pid int) string {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return strconv.Itoa(pid)
	}
	return fmt.Sprintf("%d@%d.%06d", pid, k.Proc.P_starttime.Sec, k.Proc.P_starttime.Usec)
}
