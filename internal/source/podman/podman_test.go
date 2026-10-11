package podman_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/source/podman"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// fakeVMs returns the VMs it holds; Collect reads it each call.
type fakeVMs struct {
	vms []vmproc.VM
	err error
}

func (f *fakeVMs) List(context.Context) ([]vmproc.VM, error) { return f.vms, f.err }

var (
	dockerVM = vmproc.VM{PID: 10, Kind: vmproc.Docker, FootprintBytes: 3 << 30}
	tartVM   = vmproc.VM{PID: 20, Kind: vmproc.Tart, Name: "headroom-mac", FootprintBytes: 4 << 30}
	podmanVM = vmproc.VM{PID: 30, Kind: vmproc.Podman, Name: "podman-machine-default", FootprintBytes: 1 << 30}
)

// collect runs one Collect and returns the snapshot it builds.
func collect(t *testing.T, s *podman.Source) *protocol.Snapshot {
	t.Helper()
	r, err := s.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var snap protocol.Snapshot
	r.Apply(&snap)
	return &snap
}

func TestReportsPodmanVMs(t *testing.T) {
	vms := &fakeVMs{vms: []vmproc.VM{tartVM, podmanVM}}
	s := podman.New(vms, slog.New(slog.DiscardHandler))
	if s.Name() != "podman" {
		t.Fatalf("name = %q", s.Name())
	}
	want := &protocol.Podman{VMs: []protocol.PodmanVM{
		{Machine: "podman-machine-default", PID: 30, FootprintBytes: 1 << 30},
	}}
	if got := collect(t, s).Podman; !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestNoPodmanVMOmitsTheSection(t *testing.T) {
	s := podman.New(&fakeVMs{vms: []vmproc.VM{dockerVM, tartVM}}, slog.New(slog.DiscardHandler))
	if got := collect(t, s).Podman; got != nil {
		t.Fatalf("podman = %+v, want nil", got)
	}
}

func TestAListingFailureIsAnError(t *testing.T) {
	s := podman.New(&fakeVMs{err: errors.New("lsof failed")}, slog.New(slog.DiscardHandler))
	if _, err := s.Collect(context.Background()); err == nil {
		t.Fatal("want an error: the footprints are unknown, not zero")
	}
}

func TestBothVMsWarnInTheSnapshotAndOncePerChangeInTheLog(t *testing.T) {
	var log bytes.Buffer
	vms := &fakeVMs{vms: []vmproc.VM{podmanVM}}
	s := podman.New(vms, slog.New(slog.NewTextHandler(&log, nil)))
	const warning = "Docker Desktop's and Podman's VMs are both running"
	warns := func() int { return strings.Count(log.String(), "level=WARN") }

	if got := collect(t, s).Podman.Warnings; got != nil {
		t.Fatalf("podman alone: warnings = %v, want none", got)
	}
	vms.vms = []vmproc.VM{dockerVM, podmanVM}
	for range 3 {
		if got := collect(t, s).Podman.Warnings; !reflect.DeepEqual(got, []string{warning}) {
			t.Fatalf("both: warnings = %v, want %q", got, warning)
		}
	}
	if warns() != 1 || !strings.Contains(log.String(), warning) {
		t.Fatalf("log after three ticks with both:\n%s\nwant the warning once", log.String())
	}
	vms.vms = []vmproc.VM{dockerVM} // Podman stopped
	collect(t, s)
	collect(t, s)
	if warns() != 2 || strings.Count(log.String(), "no longer both running") != 1 {
		t.Fatalf("log after Podman stopped:\n%s\nwant the end logged once", log.String())
	}
	vms.vms = []vmproc.VM{dockerVM, podmanVM}
	collect(t, s)
	if warns() != 3 {
		t.Fatalf("log after both again:\n%s\nwant a second warning", log.String())
	}
}
