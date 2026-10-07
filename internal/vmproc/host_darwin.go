package vmproc

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

// vmComm is the VM process name as the kernel stores it: p_comm keeps only
// the first MAXCOMLEN (16) bytes of com.apple.Virtualization.VirtualMachine,
// which other processes share (e.g. com.apple.Virtualization.AppleVirtual-
// PlatformIdentity inside a macOS guest). vmExec confirms the full path.
const (
	vmComm = "com.apple.Virtua"
	vmExec = "/com.apple.Virtualization.VirtualMachine"
)

// Host is the running macOS system.
type Host struct{}

// VMPIDs implements System using the kern.proc.all sysctl (no fork, no cgo).
func (Host) VMPIDs() ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, p := range procs {
		comm, _, _ := bytes.Cut(p.Proc.P_comm[:], []byte{0})
		if string(comm) != vmComm {
			continue
		}
		pid := int(p.Proc.P_pid)
		if path, err := execPath(pid); err == nil && strings.HasSuffix(path, vmExec) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// execPath reads a process's executable path from kern.procargs2: a 4-byte
// argc, then the NUL-terminated path. It fails for other users' processes,
// which cannot be our VMs anyway.
func execPath(pid int) (string, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", err
	}
	if len(b) < 4 {
		return "", fmt.Errorf("kern.procargs2 %d: %d bytes", pid, len(b))
	}
	path, _, _ := bytes.Cut(b[4:], []byte{0})
	return string(path), nil
}

// Run implements System.
func (Host) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
