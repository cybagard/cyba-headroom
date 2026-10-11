// Package vms collects the VM processes no known owner claims (Docker
// Desktop, Tart, Podman) from the VM process listing (internal/vmproc), so
// their memory is reserved rather than left unaccounted.
package vms

import (
	"context"

	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// Source is the vms source.
type Source struct{ vms vmproc.Lister }

// New returns a source over the shared VM listing.
func New(vms vmproc.Lister) *Source { return &Source{vms: vms} }

// Name implements daemon.Source.
func (s *Source) Name() string { return "vms" }

// Collect implements daemon.Source. A listing that fails is an error: the
// VMs' footprints are then unknown, not zero.
func (s *Source) Collect(ctx context.Context) (daemon.Reading, error) {
	list, err := s.vms.List(ctx)
	if err != nil {
		return nil, err
	}
	var v protocol.VMs
	for _, vm := range list {
		if vm.Kind == vmproc.Unknown {
			v.Other = append(v.Other, protocol.OtherVM{PID: vm.PID, Name: vm.Name, FootprintBytes: vm.FootprintBytes})
		}
	}
	if len(v.Other) == 0 {
		return reading{}, nil
	}
	return reading{&v}, nil
}

// reading is nil while every VM has a known owner: the section is then
// omitted.
type reading struct{ v *protocol.VMs }

func (r reading) Apply(s *protocol.Snapshot) { s.VMs = r.v }
