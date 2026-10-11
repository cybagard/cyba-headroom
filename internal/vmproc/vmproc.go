// Package vmproc finds the host processes of Apple Virtualization VMs, tells
// Docker Desktop's VM from Tart VMs and Podman machines, and reads what each
// costs the host.
//
// Every VM runs as a com.apple.Virtualization.VirtualMachine XPC service with
// parent PID 1, so neither name nor parent tells them apart (spike #9). The
// files each one holds open do: Docker's VM has Docker.app's linuxkit image and
// com.docker.docker data open, a Tart VM its ~/.tart/vms/<name>/disk.img, a
// Podman machine its disk under ~/.local/share/containers/podman/machine/.
package vmproc

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Kind is who owns a VM process.
type Kind string

// VM kinds.
const (
	Docker  Kind = "docker"
	Tart    Kind = "tart"
	Podman  Kind = "podman"
	Unknown Kind = "unknown"
)

// VM is one running VM process.
type VM struct {
	PID  int
	Kind Kind
	// Name is the Tart VM name, the Podman machine name, or for an unknown VM
	// its command name (its executable's base name, printable characters
	// only); empty for Docker's.
	Name string
	// FootprintBytes is phys_footprint: what the host pays, as Activity
	// Monitor shows it.
	FootprintBytes uint64
}

// Process is a process with its executable and full argument list.
type Process struct {
	PID, PPID int
	// Comm is the kernel's short name (at most 16 bytes).
	Comm string
	// Exec is the executable's path; argv[0] (Args[0]) may differ.
	Exec string
	Args []string
	// ArgsErr is why Exec and Args could not be read (another user's
	// process); nil when they were.
	ArgsErr error
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
	// tartVMs is TART_HOME's vms folder with a trailing slash; empty without
	// one.
	tartVMs string

	mu    sync.Mutex
	known map[int]identity
}

type identity struct {
	kind Kind
	name string
}

// New returns a Finder over sys.
func New(sys System) *Finder { return &Finder{sys: sys, known: map[int]identity{}} }

// WithTartHome makes f also recognise Tart VMs under home/vms/<name>/: the
// TART_HOME the tart source lists VMs from. An empty or relative home is
// ignored, as tart's shim ignores it. lsof prints resolved paths, so home is
// resolved too (/tmp is /private/tmp on macOS); if it can't be, it is used
// as given.
func (f *Finder) WithTartHome(home string) *Finder {
	if path.IsAbs(home) {
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			home = resolved
		}
		f.tartVMs = path.Join(home, "vms") + "/"
	}
	return f
}

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
	id = classify(out, f.tartVMs)
	// Unknown is not cached: a VM caught while booting has not opened its
	// image yet and is classified once it has.
	if id.kind != Unknown {
		f.mu.Lock()
		f.known[pid] = id
		f.mu.Unlock()
	}
	return id, nil
}

// classify reads lsof -Fn output: an "f<fd>" line, then an "n<path>" line,
// per open file. The first txt file is the executable. tartVMs, if not
// empty, is a further folder of Tart VMs (TART_HOME's).
func classify(lsof []byte, tartVMs string) identity {
	var fd, command string
	sc := bufio.NewScanner(bytes.NewReader(lsof))
	for sc.Scan() {
		if f, ok := strings.CutPrefix(sc.Text(), "f"); ok {
			fd = f
			continue
		}
		p, ok := strings.CutPrefix(sc.Text(), "n")
		if !ok {
			continue
		}
		if fd == "txt" && command == "" {
			command = printable(path.Base(p))
		}
		if strings.Contains(p, "/Docker.app/") || strings.Contains(p, "/com.docker.docker/") {
			return identity{kind: Docker}
		}
		if _, rest, ok := strings.Cut(p, "/.tart/vms/"); ok {
			name, _, _ := strings.Cut(rest, "/")
			return identity{kind: Tart, name: name}
		}
		if rest, ok := strings.CutPrefix(p, tartVMs); ok && tartVMs != "" {
			name, _, _ := strings.Cut(rest, "/")
			return identity{kind: Tart, name: name}
		}
		// A Podman machine also holds its EFI store open there: only the
		// disk, <machine>-<arch>.raw, names it (spike #49).
		if strings.Contains(p, "/.local/share/containers/podman/machine/") {
			if disk, ok := strings.CutSuffix(path.Base(p), ".raw"); ok {
				if i := strings.LastIndex(disk, "-"); i > 0 {
					return identity{kind: Podman, name: disk[:i]}
				}
			}
		}
	}
	return identity{kind: Unknown, name: command}
}

// printable drops the characters a terminal would interpret, so a command
// name is safe to show.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
}

func (f *Finder) footprint(ctx context.Context, pid int) (uint64, error) {
	return ReadFootprint(ctx, f.sys, pid)
}

// Runner runs a command; System and Host implement it.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ReadFootprint returns a process's phys_footprint via footprint(1): what it
// costs the host, as Activity Monitor shows it. No sudo for own processes.
func ReadFootprint(ctx context.Context, r Runner, pid int) (uint64, error) {
	return ReadFootprints(ctx, r, pid)
}

// ReadFootprints returns the summed phys_footprint of pids in one footprint(1)
// run. A pid that has exited is skipped (footprint only warns); it is an
// error only if none could be read.
func ReadFootprints(ctx context.Context, r Runner, pids ...int) (uint64, error) {
	args := make([]string, 0, 2*len(pids)+3)
	for _, pid := range pids {
		args = append(args, "-p", strconv.Itoa(pid))
	}
	out, err := r.Run(ctx, footprintPath, append(args, "-f", "bytes", "--noCategories")...)
	if err != nil {
		return 0, err
	}
	var total uint64
	found := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		// "    phys_footprint: 1709280520 B", one per process
		v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "phys_footprint:")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " B"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("vmproc: footprint %v: %w", pids, err)
		}
		total += n
		found = true
	}
	if !found {
		return 0, fmt.Errorf("vmproc: footprint %v: no phys_footprint line", pids)
	}
	return total, nil
}
