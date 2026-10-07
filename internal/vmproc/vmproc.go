// Package vmproc finds the host processes of Apple Virtualization VMs, tells
// Docker Desktop's VM from Tart VMs, and reads what each costs the host.
//
// Every VM runs as a com.apple.Virtualization.VirtualMachine XPC service with
// parent PID 1, so neither name nor parent tells them apart (spike #9). The
// files each one holds open do: Docker's VM has Docker.app's linuxkit image and
// com.docker.docker data open, a Tart VM its ~/.tart/vms/<name>/disk.img.
package vmproc

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Kind is who owns a VM process.
type Kind string

// VM kinds.
const (
	Docker  Kind = "docker"
	Tart    Kind = "tart"
	Unknown Kind = "unknown"
)

// VM is one running VM process.
type VM struct {
	PID  int
	Kind Kind
	// Name is the Tart VM name; empty for other kinds.
	Name string
	// FootprintBytes is phys_footprint: what the host pays, as Activity
	// Monitor shows it.
	FootprintBytes uint64
}

// Process is a process with its full argument list.
type Process struct {
	PID, PPID int
	// Comm is the kernel's short name (at most 16 bytes).
	Comm string
	Args []string
}

// System is the OS access vmproc needs. The darwin implementation is Host.
type System interface {
	// VMPIDs lists the PIDs of Virtualization.framework VM processes.
	VMPIDs() ([]int, error)
	// Run runs a command and returns its standard output.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

const (
	lsofPath      = "/usr/sbin/lsof"
	footprintPath = "/usr/bin/footprint"
)

// Finder lists VMs. A process's kind never changes, so it is classified once
// per PID.
type Finder struct {
	sys System

	mu    sync.Mutex
	known map[int]identity
}

type identity struct {
	kind Kind
	name string
}

// New returns a Finder over sys.
func New(sys System) *Finder { return &Finder{sys: sys, known: map[int]identity{}} }

// List returns the running VMs, in PID order of the system listing. A process
// that exits while being read is left out; a VM that is still running but
// cannot be read is an error, since dropping it would under-report its cost.
func (f *Finder) List(ctx context.Context) ([]VM, error) {
	pids, err := f.sys.VMPIDs()
	if err != nil {
		return nil, fmt.Errorf("vmproc: listing processes: %w", err)
	}
	f.forgetExited(pids)
	vms := make([]VM, 0, len(pids))
	for _, pid := range pids {
		id, err := f.identify(ctx, pid)
		var fp uint64
		if err == nil {
			fp, err = f.footprint(ctx, pid)
		}
		if err != nil {
			if f.exited(pid) {
				continue
			}
			return nil, fmt.Errorf("vmproc: reading VM %d: %w", pid, err)
		}
		vms = append(vms, VM{PID: pid, Kind: id.kind, Name: id.name, FootprintBytes: fp})
	}
	return vms, nil
}

// exited reports whether pid is no longer a VM process.
func (f *Finder) exited(pid int) bool {
	pids, err := f.sys.VMPIDs()
	return err == nil && !slices.Contains(pids, pid)
}

// forgetExited drops classifications of PIDs no longer listed, so a reused
// PID is classified afresh.
func (f *Finder) forgetExited(live []int) {
	alive := make(map[int]bool, len(live))
	for _, pid := range live {
		alive[pid] = true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for pid := range f.known {
		if !alive[pid] {
			delete(f.known, pid)
		}
	}
}

func (f *Finder) identify(ctx context.Context, pid int) (identity, error) {
	f.mu.Lock()
	id, ok := f.known[pid]
	f.mu.Unlock()
	if ok {
		return id, nil
	}
	out, err := f.sys.Run(ctx, lsofPath, "-a", "-p", strconv.Itoa(pid), "-d", "txt,0-999", "-Fn")
	if err != nil {
		return identity{}, err
	}
	id = classify(out)
	// Unknown is not cached: a VM caught while booting has not opened its
	// image yet and is classified once it has.
	if id.kind != Unknown {
		f.mu.Lock()
		f.known[pid] = id
		f.mu.Unlock()
	}
	return id, nil
}

// classify reads lsof -Fn output: one "n<path>" line per open file.
func classify(lsof []byte) identity {
	sc := bufio.NewScanner(bytes.NewReader(lsof))
	for sc.Scan() {
		p, ok := strings.CutPrefix(sc.Text(), "n")
		if !ok {
			continue
		}
		if strings.Contains(p, "/Docker.app/") || strings.Contains(p, "/com.docker.docker/") {
			return identity{kind: Docker}
		}
		if _, rest, ok := strings.Cut(p, "/.tart/vms/"); ok {
			name, _, _ := strings.Cut(rest, "/")
			return identity{kind: Tart, name: name}
		}
	}
	return identity{kind: Unknown}
}

func (f *Finder) footprint(ctx context.Context, pid int) (uint64, error) {
	out, err := f.sys.Run(ctx, footprintPath, "-p", strconv.Itoa(pid), "-f", "bytes", "--noCategories")
	if err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		// "    phys_footprint: 1709280520 B"
		v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "phys_footprint:")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " B"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("vmproc: footprint %d: %w", pid, err)
		}
		return n, nil
	}
	return 0, fmt.Errorf("vmproc: footprint %d: no phys_footprint line", pid)
}
