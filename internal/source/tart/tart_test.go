package tart_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/source/tart"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// fakeCLI answers tart subcommands from testdata files.
type fakeCLI map[string]string // "list --format json" -> file

func (f fakeCLI) Run(_ context.Context, args ...string) ([]byte, error) {
	file, ok := f[strings.Join(args, " ")]
	if !ok {
		return nil, errors.New("unexpected tart " + strings.Join(args, " "))
	}
	return os.ReadFile("testdata/" + file)
}

// fakeProcs serves a fixed process table and cwds.
type fakeProcs struct {
	procs []vmproc.Process
	cwds  map[int]string
}

func (f fakeProcs) ProcessesNamed(comm string) ([]vmproc.Process, error) {
	var out []vmproc.Process
	for _, p := range f.procs {
		if p.Comm == comm {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f fakeProcs) Cwd(_ context.Context, pid int) (string, error) {
	if cwd, ok := f.cwds[pid]; ok {
		return cwd, nil
	}
	return "", errors.New("no such process")
}

type fakeVMs []vmproc.VM

func (f fakeVMs) List(context.Context) ([]vmproc.VM, error) { return f, nil }

func runningCLI() fakeCLI {
	return fakeCLI{
		"list --format json":             "list.json",
		"get headroom-mac --format json": "get-headroom-mac.json",
	}
}

func collect(t *testing.T, src *tart.Source) protocol.Tart {
	t.Helper()
	r, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var snap protocol.Snapshot
	r.Apply(&snap)
	if snap.Tart == nil {
		t.Fatal("reading did not fill Snapshot.Tart")
	}
	return *snap.Tart
}

func TestTartNotInstalledIsAReading(t *testing.T) {
	got := collect(t, tart.New(nil, fakeProcs{}, fakeVMs{}))
	if got.Installed || got.VMs == nil || len(got.VMs) != 0 {
		t.Fatalf("got %+v, want not installed with an empty VM list", got)
	}
}

func TestReportsRunningVMsWithConfiguredResources(t *testing.T) {
	got := collect(t, tart.New(runningCLI(), fakeProcs{}, fakeVMs{}))
	if !got.Installed || got.MacOSRunning != 1 || len(got.VMs) != 1 {
		t.Fatalf("got %+v, want one running macOS VM", got)
	}
	vm := got.VMs[0]
	// 4096 MiB configured in tart get; stopped VMs are not listed.
	if vm.Name != "headroom-mac" || vm.OS != "darwin" || vm.CPUs != 4 || vm.MemoryBytes != 4096<<20 {
		t.Fatalf("vm = %+v", vm)
	}
}

func TestNothingRunning(t *testing.T) {
	got := collect(t, tart.New(fakeCLI{"list --format json": "list-stopped.json"}, fakeProcs{}, fakeVMs{}))
	if !got.Installed || got.MacOSRunning != 0 || len(got.VMs) != 0 {
		t.Fatalf("got %+v, want installed with nothing running", got)
	}
}

func TestFootprintFromVMProcess(t *testing.T) {
	vms := fakeVMs{
		{PID: 10, Kind: vmproc.Docker, FootprintBytes: 3 << 30},
		{PID: 20, Kind: vmproc.Tart, Name: "headroom-mac", FootprintBytes: 4350000000},
	}
	got := collect(t, tart.New(runningCLI(), fakeProcs{}, vms))
	if vm := got.VMs[0]; vm.FootprintBytes != 4350000000 {
		t.Fatalf("footprint = %d, want the headroom-mac process's", vm.FootprintBytes)
	}
}

type brokenVMs struct{}

func (brokenVMs) List(context.Context) ([]vmproc.VM, error) { return nil, errors.New("lsof failed") }

func TestVMListingFailureKeepsVMs(t *testing.T) {
	got := collect(t, tart.New(runningCLI(), fakeProcs{}, brokenVMs{}))
	if len(got.VMs) != 1 || got.VMError == "" {
		t.Fatalf("got %+v, want the VM plus a VM error", got)
	}
}

// tartBin is where tart.app installs its binary.
const tartBin = "/Users/dev/Applications/tart.app/Contents/MacOS/tart"

func TestLaunchDetailsFromTheTartRunProcess(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		ppid     int
		wantCwd  string
		wantDirs []string
	}{
		{
			// Captured from scripts/tart-test.sh, username replaced.
			name:     "named read-only share",
			args:     []string{tartBin, "run", "--no-graphics", "--dir=src:/Users/dev/wt/fix-login:ro", "headroom-mac"},
			ppid:     500,
			wantCwd:  "/Users/dev/wt/fix-login",
			wantDirs: []string{"/Users/dev/wt/fix-login"},
		},
		{
			name:     "space-separated flags, unnamed share, VM name not last",
			args:     []string{tartBin, "run", "--dir", "/Users/dev/wt/api", "headroom-mac", "--no-graphics"},
			ppid:     500,
			wantCwd:  "/Users/dev/wt/fix-login",
			wantDirs: []string{"/Users/dev/wt/api"},
		},
		{
			// Parent exited: reparented to launchd, whose cwd says nothing.
			name: "orphaned",
			args: []string{tartBin, "run", "headroom-mac"},
			ppid: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			procs := fakeProcs{
				procs: []vmproc.Process{
					{PID: 900, PPID: tc.ppid, Comm: "tart", Args: tc.args},
					// Not a launcher: tart exec into the same VM.
					{PID: 901, PPID: 500, Comm: "tart", Args: []string{tartBin, "exec", "headroom-mac", "sleep", "40"}},
				},
				cwds: map[int]string{500: "/Users/dev/wt/fix-login", 1: "/"},
			}
			vm := collect(t, tart.New(runningCLI(), procs, fakeVMs{})).VMs[0]
			if vm.RunPID != 900 || vm.LaunchCwd != tc.wantCwd || !slices.Equal(vm.SharedDirs, tc.wantDirs) {
				t.Fatalf("got pid=%d cwd=%q dirs=%v, want 900 %q %v", vm.RunPID, vm.LaunchCwd, vm.SharedDirs, tc.wantCwd, tc.wantDirs)
			}
		})
	}
}
