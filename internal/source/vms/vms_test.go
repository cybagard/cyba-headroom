package vms_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/source/vms"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// fakeVMs returns the VMs it holds; Collect reads it each call.
type fakeVMs struct {
	vms []vmproc.VM
	err error
}

func (f *fakeVMs) List(context.Context) ([]vmproc.VM, error) { return f.vms, f.err }

const vmCommand = "com.apple.Virtualization.VirtualMachine"

var (
	dockerVM  = vmproc.VM{PID: 10, Kind: vmproc.Docker, FootprintBytes: 3 << 30}
	tartVM    = vmproc.VM{PID: 20, Kind: vmproc.Tart, Name: "headroom-mac", FootprintBytes: 4 << 30}
	podmanVM  = vmproc.VM{PID: 30, Kind: vmproc.Podman, Name: "podman-machine-default", FootprintBytes: 1 << 30}
	unknownA  = vmproc.VM{PID: 40, Kind: vmproc.Unknown, Name: vmCommand, FootprintBytes: 2 << 30}
	unknownB  = vmproc.VM{PID: 50, Kind: vmproc.Unknown, Name: vmCommand, FootprintBytes: 1 << 29}
	ownedVMs  = []vmproc.VM{dockerVM, tartVM, podmanVM}
	allKindsV = []vmproc.VM{dockerVM, unknownA, tartVM, podmanVM, unknownB}
)

// collect runs one Collect and returns the snapshot it builds.
func collect(t *testing.T, s *vms.Source) *protocol.Snapshot {
	t.Helper()
	r, err := s.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var snap protocol.Snapshot
	r.Apply(&snap)
	return &snap
}

func TestReportsOnlyVMsNoOwnerClaims(t *testing.T) {
	s := vms.New(&fakeVMs{vms: allKindsV})
	if s.Name() != "vms" {
		t.Fatalf("name = %q", s.Name())
	}
	want := &protocol.VMs{Other: []protocol.OtherVM{
		{PID: 40, Name: vmCommand, FootprintBytes: 2 << 30},
		{PID: 50, Name: vmCommand, FootprintBytes: 1 << 29},
	}}
	if got := collect(t, s).VMs; !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestOnlyOwnedVMsOmitTheSection(t *testing.T) {
	if got := collect(t, vms.New(&fakeVMs{vms: ownedVMs})).VMs; got != nil {
		t.Fatalf("vms = %+v, want nil", got)
	}
}

func TestAFailedListingIsAnError(t *testing.T) {
	// The footprints are then unknown, not zero.
	if _, err := vms.New(&fakeVMs{err: errors.New("lsof failed")}).Collect(context.Background()); err == nil {
		t.Fatal("want an error")
	}
}
