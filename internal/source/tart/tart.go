// Package tart collects running Tart VMs (R1): configured resources from the
// tart CLI, host cost from the VM process (internal/vmproc), and how each was
// launched, for worktree attribution (R3).
package tart

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
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

// Source is the Tart source.
type Source struct {
	cli   CLI
	procs Procs
	vms   vmproc.Lister
	home  string

	// configs caches tart get per running VM: a running VM's config cannot
	// change. Collect is never called concurrently (daemon.Tick).
	configs map[string]vmConfig
}

type vmConfig struct {
	OS     string
	CPU    int
	Memory uint64 // MiB
}

// New returns a source. A nil cli means tart is not installed; home expands
// ~ in --dir shares.
func New(cli CLI, procs Procs, vms vmproc.Lister, home string) *Source {
	return &Source{cli: cli, procs: procs, vms: vms, home: home, configs: map[string]vmConfig{}}
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
	running := map[string]bool{}
	for _, v := range list {
		running[v.Name] = v.Running
	}
	for name := range s.configs {
		if !running[name] {
			delete(s.configs, name) // stopped: re-read if it starts again
		}
	}

	// VM processes and launch details are optional: on failure, report the
	// VMs without them rather than failing the reading.
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
	launches, err := s.launches(ctx, running)
	if err != nil {
		t.LaunchError = err.Error()
	}

	for _, v := range list {
		if !v.Running {
			continue
		}
		cfg, ok := s.configs[v.Name]
		if !ok {
			if err := s.tartJSON(ctx, &cfg, "get", v.Name, "--format", "json"); err == nil {
				s.configs[v.Name] = cfg
			} else {
				// Its OS and memory are unknown ("", 0), but it may hold
				// a macOS slot (R6): it is counted as one rather than let
				// a third VM by. If it stopped since tart list, the next
				// reading drops it.
				cfg = vmConfig{}
			}
		}
		vm := protocol.TartVM{Name: v.Name, OS: cfg.OS, CPUs: cfg.CPU, MemoryBytes: cfg.Memory << 20}
		if fp, ok := footprint[v.Name]; ok {
			vm.FootprintBytes = &fp
		}
		if l, ok := launches[v.Name]; ok {
			vm.RunPID, vm.RunPIDs, vm.LaunchCwd, vm.SharedDirs = l.pid, l.pids, l.cwd, l.dirs
		}
		if vm.OS == "darwin" || vm.OS == "" {
			t.MacOSRunning++
		}
		t.VMs = append(t.VMs, vm)
	}
	return reading{t}, nil
}

type launch struct {
	pid  int
	pids []int // every tart run of the VM, pid among them
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
		name, dirs := parseRun(p.Args[2:])
		if !running[name] {
			continue
		}
		l := launch{pid: p.PID, pids: append(out[name].pids, p.PID)}
		if p.PPID > 1 { // reparented to launchd: the launcher is gone
			l.cwd, _ = s.procs.Cwd(ctx, p.PPID)
		}
		for _, d := range dirs {
			if hp := hostPath(d, s.home, l.cwd); hp != "" {
				l.dirs = append(l.dirs, hp)
			}
		}
		out[name] = l
	}
	return out, nil
}

// valueFlags are the tart run options that take a value (tart 2.40 --help),
// so their values are not mistaken for the VM name.
var valueFlags = map[string]bool{
	"--serial-path": true, "--disk": true, "--rosetta": true, "--dir": true,
	"--net-bridged": true, "--net-softnet-allow": true, "--net-softnet-block": true,
	"--net-softnet-control-fd": true, "--net-softnet-expose": true,
	"--root-disk-opts": true, "--provisioning-opts": true,
}

// parseRun reads tart run's arguments: the VM name is the one positional
// argument, and --dir values are collected raw.
func parseRun(args []string) (name string, dirs []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			if name == "" {
				name = a
			}
			continue
		}
		flag, value, hasValue := strings.Cut(a, "=")
		if !valueFlags[flag] {
			continue
		}
		if !hasValue && i+1 < len(args) {
			i++
			value = args[i]
		}
		if flag == "--dir" {
			dirs = append(dirs, value)
		}
	}
	return name, dirs
}

// hostPath resolves a --dir value, [name:]path[:options], to an absolute host
// path: ~ expands to home, as tart does, and a relative path is relative to
// where tart run was started (cwd). Unresolvable paths give "".
func hostPath(dir, home, cwd string) string {
	parts := strings.Split(dir, ":")
	path := parts[0]
	if len(parts) > 1 && !looksLikePath(parts[0]) && looksLikePath(parts[1]) {
		path = parts[1] // named share
	}
	switch {
	case path == "~" || strings.HasPrefix(path, "~/"):
		if home == "" {
			return ""
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	case filepath.IsAbs(path):
	case cwd != "":
		path = filepath.Join(cwd, path)
	default:
		return ""
	}
	return filepath.Clean(path)
}

// looksLikePath tells a share's path from its name: names are plain words.
func looksLikePath(s string) bool { return strings.ContainsAny(s, "/~.") }

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
