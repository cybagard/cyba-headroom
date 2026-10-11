// Package podman collects running Podman machines' VMs (R1) from the VM
// process listing (internal/vmproc), and warns when Docker Desktop's VM runs
// beside them.
package podman

import (
	"context"
	"log/slog"

	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// bothRunning is the warning while Docker Desktop's and a Podman machine's
// VMs run together: two engines' VMs, each holding its own memory.
const bothRunning = "Docker Desktop's and Podman's VMs are both running"

// Source is the Podman source.
type Source struct {
	vms vmproc.Lister
	log *slog.Logger

	// both is whether the last reading had both VMs running. Collect is
	// never called concurrently (daemon.Tick).
	both bool
}

// New returns a source over the shared VM listing.
func New(vms vmproc.Lister, log *slog.Logger) *Source { return &Source{vms: vms, log: log} }

// Name implements daemon.Source.
func (s *Source) Name() string { return "podman" }

// Collect implements daemon.Source. A listing that fails is an error: the
// VMs' footprints are then unknown, not zero.
func (s *Source) Collect(ctx context.Context) (daemon.Reading, error) {
	vms, err := s.vms.List(ctx)
	if err != nil {
		return nil, err
	}
	var p protocol.Podman
	docker := false
	for _, vm := range vms {
		switch vm.Kind {
		case vmproc.Podman:
			p.VMs = append(p.VMs, protocol.PodmanVM{Machine: vm.Name, PID: vm.PID, FootprintBytes: vm.FootprintBytes})
		case vmproc.Docker:
			docker = true
		}
	}
	both := docker && len(p.VMs) > 0
	if both {
		p.Warnings = []string{bothRunning}
	}
	// Logged once each time it starts or ends, not every tick.
	if both != s.both {
		if both {
			s.log.Warn(bothRunning)
		} else {
			s.log.Info("Docker Desktop's and Podman's VMs are no longer both running")
		}
		s.both = both
	}
	if len(p.VMs) == 0 {
		return reading{}, nil
	}
	return reading{&p}, nil
}

// reading is nil while no Podman machine runs: the section is then omitted.
type reading struct{ p *protocol.Podman }

func (r reading) Apply(s *protocol.Snapshot) { s.Podman = r.p }
