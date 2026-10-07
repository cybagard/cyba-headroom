//go:build darwin

package vmproc

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
	"strconv"
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
		if path, _, err := procArgs(pid); err == nil && strings.HasSuffix(path, vmExec) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// ProcessesNamed lists processes whose kernel name is comm, with their
// arguments. Processes whose arguments cannot be read (other users') are
// left out.
func (Host) ProcessesNamed(comm string) ([]Process, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, p := range procs {
		c, _, _ := bytes.Cut(p.Proc.P_comm[:], []byte{0})
		if string(c) != comm {
			continue
		}
		pid := int(p.Proc.P_pid)
		_, args, err := procArgs(pid)
		if err != nil {
			continue
		}
		out = append(out, Process{PID: pid, PPID: int(p.Eproc.Ppid), Comm: comm, Args: args})
	}
	return out, nil
}

// Cwd returns a process's working directory via lsof (no sudo for the
// user's own processes).
func (h Host) Cwd(ctx context.Context, pid int) (string, error) {
	out, err := h.Run(ctx, lsofPath, "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, "n"); ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("lsof: no cwd for %d", pid)
}

// procArgs reads kern.procargs2: a 4-byte argc, the NUL-terminated exec
// path, NUL padding, then argc NUL-terminated arguments (then the
// environment, ignored). It fails for other users' processes.
func procArgs(pid int) (exec string, args []string, err error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", nil, err
	}
	if len(b) < 4 {
		return "", nil, fmt.Errorf("kern.procargs2 %d: %d bytes", pid, len(b))
	}
	argc := int(binary.LittleEndian.Uint32(b))
	rest := b[4:]
	path, rest, _ := bytes.Cut(rest, []byte{0})
	rest = bytes.TrimLeft(rest, "\x00")
	for range argc {
		var a []byte
		var ok bool
		if a, rest, ok = bytes.Cut(rest, []byte{0}); !ok {
			break
		}
		args = append(args, string(a))
	}
	return string(path), args, nil
}

// Run implements System.
func (Host) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
