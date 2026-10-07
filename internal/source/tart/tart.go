// Package tart collects running Tart VMs (R1): configured resources from the
// tart CLI, host cost from the VM process (internal/vmproc), and how each was
// launched, for worktree attribution (R3).
package tart

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// CLI runs the tart binary.
type CLI interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// Procs is the process access the source needs; vmproc.Host implements it.
type Procs interface {
	ProcessesNamed(comm string) ([]vmproc.Process, error)
	Cwd(ctx context.Context, pid int) (string, error)
}

// VMLister lists VM processes; vmproc.Finder implements it.
type VMLister interface {
	List(ctx context.Context) ([]vmproc.VM, error)
}

// Source is the Tart source.
type Source struct {
	cli   CLI
	procs Procs
	vms   VMLister
}

// New returns a source. A nil cli means tart is not installed.
func New(cli CLI, procs Procs, vms VMLister) *Source {
	return &Source{cli: cli, procs: procs, vms: vms}
}

// Name implements daemon.Source.
func (s *Source) Name() string { return "tart" }

// Collect implements daemon.Source.
func (s *Source) Collect(ctx context.Context) (daemon.Reading, error) {
	t := protocol.Tart{VMs: []protocol.TartVM{}}
	if s.cli == nil {
		return reading{t}, nil
	}
	t.Installed = true

	var list []struct {
		Name    string
		Running bool
	}
	if err := s.tartJSON(ctx, &list, "list", "--format", "json"); err != nil {
		return nil, err
	}
	footprint := map[string]uint64{}
	procs, err := s.vms.List(ctx)
	if err != nil {
		t.VMError = err.Error()
	}
	for _, p := range procs {
		if p.Kind == vmproc.Tart {
			footprint[p.Name] = p.FootprintBytes
		}
	}

	running := map[string]bool{}
	for _, v := range list {
		running[v.Name] = v.Running
	}
	launches, err := s.launches(ctx, running)
	if err != nil {
		return nil, err
	}

	for _, v := range list {
		if !v.Running {
			continue
		}
		var cfg struct {
			OS     string
			CPU    int
			Memory uint64 // MiB
		}
		if err := s.tartJSON(ctx, &cfg, "get", v.Name, "--format", "json"); err != nil {
			return nil, err
		}
		vm := protocol.TartVM{
			Name:           v.Name,
			OS:             cfg.OS,
			CPUs:           cfg.CPU,
			MemoryBytes:    cfg.Memory << 20,
			FootprintBytes: footprint[v.Name],
		}
		if l, ok := launches[v.Name]; ok {
			vm.RunPID, vm.LaunchCwd, vm.SharedDirs = l.pid, l.cwd, l.dirs
		}
		if vm.OS == "darwin" {
			t.MacOSRunning++
		}
		t.VMs = append(t.VMs, vm)
	}
	return reading{t}, nil
}

type launch struct {
	pid  int
	cwd  string
	dirs []string
}

// launches finds the tart run process of each running VM.
func (s *Source) launches(ctx context.Context, running map[string]bool) (map[string]launch, error) {
	procs, err := s.procs.ProcessesNamed("tart")
	if err != nil {
		return nil, fmt.Errorf("tart: listing processes: %w", err)
	}
	out := map[string]launch{}
	for _, p := range procs {
		if len(p.Args) < 2 || p.Args[1] != "run" {
			continue
		}
		name, dirs := parseRun(p.Args[2:], running)
		if name == "" {
			continue
		}
		l := launch{pid: p.PID, dirs: dirs}
		if p.PPID > 1 { // reparented to launchd: the launcher is gone
			l.cwd, _ = s.procs.Cwd(ctx, p.PPID)
		}
		out[name] = l
	}
	return out, nil
}

// parseRun reads tart run's arguments: the VM is the argument naming a
// running VM (options may follow it), and each --dir value is
// [name:]path[:options].
func parseRun(args []string, running map[string]bool) (name string, dirs []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		var dir string
		switch {
		case strings.HasPrefix(a, "--dir="):
			dir = strings.TrimPrefix(a, "--dir=")
		case a == "--dir" && i+1 < len(args):
			i++
			dir = args[i]
		case running[a]:
			name = a
		}
		if p := hostPath(dir); p != "" {
			dirs = append(dirs, p)
		}
	}
	return name, dirs
}

// hostPath extracts the absolute host path from a --dir value.
func hostPath(dir string) string {
	i := strings.Index(dir, "/")
	if i < 0 {
		return ""
	}
	p, _, _ := strings.Cut(dir[i:], ":")
	return p
}

func (s *Source) tartJSON(ctx context.Context, v any, args ...string) error {
	out, err := s.cli.Run(ctx, args...)
	if err != nil {
		return fmt.Errorf("tart %s: %w", args[0], err)
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("tart %s: %w", args[0], err)
	}
	return nil
}

type reading struct{ t protocol.Tart }

func (r reading) Apply(s *protocol.Snapshot) {
	t := r.t
	s.Tart = &t
}
