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

// fakeCLI answers tart subcommands from testdata files or inline JSON.
type fakeCLI map[string]string // "list --format json" -> file or JSON

func (f fakeCLI) Run(_ context.Context, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	calls[key]++
	body, ok := f[key]
	if !ok {
		return nil, errors.New("exit status 1: tart " + key)
	}
	if strings.HasPrefix(body, "[") || strings.HasPrefix(body, "{") {
		return []byte(body), nil
	}
	return os.ReadFile("testdata/" + body)
}

// calls counts tart invocations; tests that read it reset it first.
var calls = map[string]int{}

// fakeProcs serves a fixed process table and cwds.
type fakeProcs struct {
	procs []vmproc.Process
	cwds  map[int]string
	err   error
}

func (f fakeProcs) ProcessesNamed(comm string) ([]vmproc.Process, error) {
	if f.err != nil {
		return nil, f.err
	}
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
	got := collect(t, tart.New(nil, fakeProcs{}, fakeVMs{}, home))
	if got.Installed || got.VMs == nil || len(got.VMs) != 0 {
		t.Fatalf("got %+v, want not installed with an empty VM list", got)
	}
}

func TestReportsRunningVMsWithConfiguredResources(t *testing.T) {
	got := collect(t, tart.New(runningCLI(), fakeProcs{}, fakeVMs{}, home))
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
	got := collect(t, tart.New(fakeCLI{"list --format json": "list-stopped.json"}, fakeProcs{}, fakeVMs{}, home))
	if !got.Installed || got.MacOSRunning != 0 || len(got.VMs) != 0 {
		t.Fatalf("got %+v, want installed with nothing running", got)
	}
}

func TestFootprintFromVMProcess(t *testing.T) {
	vms := fakeVMs{
		{PID: 10, Kind: vmproc.Docker, FootprintBytes: 3 << 30},
		{PID: 20, Kind: vmproc.Tart, Name: "headroom-mac", FootprintBytes: 4350000000},
	}
	got := collect(t, tart.New(runningCLI(), fakeProcs{}, vms, home))
	if vm := got.VMs[0]; vm.FootprintBytes == nil || *vm.FootprintBytes != 4350000000 {
		t.Fatalf("footprint = %v, want the headroom-mac process's", vm.FootprintBytes)
	}
}

type brokenVMs struct{}

func (brokenVMs) List(context.Context) ([]vmproc.VM, error) { return nil, errors.New("lsof failed") }

func TestVMListingFailureKeepsVMs(t *testing.T) {
	got := collect(t, tart.New(runningCLI(), fakeProcs{}, brokenVMs{}, home))
	if len(got.VMs) != 1 || got.VMError == "" {
		t.Fatalf("got %+v, want the VM plus a VM error", got)
	}
}

// home is the user's home directory in these tests.
const home = "/Users/dev"

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
			// tart expands ~ itself (its --help shows --dir="~/src/sources:ro").
			name:     "tilde and named tilde shares",
			args:     []string{tartBin, "run", "--dir=~/wt/a:ro", "--dir=sources:~/wt/b", "headroom-mac"},
			ppid:     500,
			wantCwd:  "/Users/dev/wt/fix-login",
			wantDirs: []string{"/Users/dev/wt/a", "/Users/dev/wt/b"},
		},
		{
			// Relative to where tart run was started: its launcher's cwd.
			name:     "relative share",
			args:     []string{tartBin, "run", "--dir", "src/app", "headroom-mac"},
			ppid:     500,
			wantCwd:  "/Users/dev/wt/fix-login",
			wantDirs: []string{"/Users/dev/wt/fix-login/src/app"},
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
			vm := collect(t, tart.New(runningCLI(), procs, fakeVMs{}, home)).VMs[0]
			if vm.RunPID != 900 || vm.LaunchCwd != tc.wantCwd || !slices.Equal(vm.SharedDirs, tc.wantDirs) {
				t.Fatalf("got pid=%d cwd=%q dirs=%v, want 900 %q %v", vm.RunPID, vm.LaunchCwd, vm.SharedDirs, tc.wantCwd, tc.wantDirs)
			}
		})
	}
}

func TestRelativeShareWithoutLauncherIsDropped(t *testing.T) {
	procs := fakeProcs{procs: []vmproc.Process{
		{PID: 900, PPID: 1, Comm: "tart", Args: []string{tartBin, "run", "--dir=src/app", "headroom-mac"}},
	}}
	vm := collect(t, tart.New(runningCLI(), procs, fakeVMs{}, home)).VMs[0]
	if len(vm.SharedDirs) != 0 {
		t.Fatalf("dirs = %v, want none: a relative path cannot be resolved", vm.SharedDirs)
	}
}

// twoRunning has VMs a and b running.
func twoRunning() fakeCLI {
	return fakeCLI{
		"list --format json":  `[{"Name":"a","Running":true},{"Name":"b","Running":true}]`,
		"get a --format json": `{"OS":"darwin","CPU":4,"Memory":4096}`,
		"get b --format json": `{"OS":"linux","CPU":2,"Memory":2048}`,
	}
}

func TestOptionValueNamedLikeAnotherVMIsNotTheVM(t *testing.T) {
	procs := fakeProcs{
		procs: []vmproc.Process{
			// "b" here is an interface name that happens to match VM b.
			{PID: 900, PPID: 500, Comm: "tart", Args: []string{tartBin, "run", "--net-bridged", "b", "a"}},
		},
		cwds: map[int]string{500: "/Users/dev/wt/fix-login"},
	}
	got := collect(t, tart.New(twoRunning(), procs, fakeVMs{}, home))
	byName := map[string]int{}
	for _, vm := range got.VMs {
		byName[vm.Name] = vm.RunPID
	}
	if byName["a"] != 900 || byName["b"] != 0 {
		t.Fatalf("run pids = %v, want a=900 and b unlaunched", byName)
	}
}

func TestOneVMFailingTartGetKeepsTheOthers(t *testing.T) {
	cli := twoRunning()
	delete(cli, "get b --format json") // b's config cannot be read
	got := collect(t, tart.New(cli, fakeProcs{}, fakeVMs{}, home))
	if len(got.VMs) != 2 || got.VMs[0].Name != "a" || got.VMs[0].OS == "" || got.VMs[1].OS != "" {
		t.Fatalf("vms = %+v, want a, and b with its OS unknown", got.VMs)
	}
}

func TestProcessListingFailureKeepsVMs(t *testing.T) {
	procs := fakeProcs{err: errors.New("kern.proc.all: cannot allocate memory")}
	got := collect(t, tart.New(runningCLI(), procs, fakeVMs{}, home))
	if len(got.VMs) != 1 || got.LaunchError == "" {
		t.Fatalf("got %+v, want the VM plus a launch error", got)
	}
}

func TestFootprintUnknownWhenVMProcessNotFound(t *testing.T) {
	// e.g. still booting: vmproc classified it Unknown.
	vms := fakeVMs{{PID: 20, Kind: vmproc.Unknown, FootprintBytes: 4 << 30}}
	got := collect(t, tart.New(runningCLI(), fakeProcs{}, vms, home))
	if fp := got.VMs[0].FootprintBytes; fp != nil {
		t.Fatalf("footprint = %d, want unknown (nil), not a cost", *fp)
	}
}

func TestConfigReadOncePerRun(t *testing.T) {
	clear(calls)
	cli := runningCLI()
	src := tart.New(cli, fakeProcs{}, fakeVMs{}, home)
	for range 3 {
		collect(t, src)
	}
	if n := calls["get headroom-mac --format json"]; n != 1 {
		t.Fatalf("tart get ran %d times while the VM kept running, want 1", n)
	}
	// Stopped, then started again (perhaps reconfigured): read anew.
	cli["list --format json"] = "list-stopped.json"
	collect(t, src)
	cli["list --format json"] = "list.json"
	collect(t, src)
	if n := calls["get headroom-mac --format json"]; n != 2 {
		t.Fatalf("tart get ran %d times across a restart, want 2", n)
	}
}

// A running VM whose config cannot be read still holds a slot: it is
// reported with its OS unknown and counted as macOS (R6, #29).
func TestARunningVMWithoutItsConfigCountsAsMacOS(t *testing.T) {
	cli := fakeCLI{"list --format json": "list.json"} // tart get fails
	got := collect(t, tart.New(cli, fakeProcs{}, fakeVMs{}, home))
	if got.MacOSRunning != 1 || len(got.VMs) != 1 || got.VMs[0].Name != "headroom-mac" || got.VMs[0].OS != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestAnotherUsersTartProcessIsNotALauncher(t *testing.T) {
	procs := fakeProcs{procs: []vmproc.Process{
		{PID: 900, PPID: 500, Comm: "tart", ArgsErr: errors.New("kern.procargs2: operation not permitted")},
	}}
	vm := collect(t, tart.New(runningCLI(), procs, fakeVMs{}, home)).VMs[0]
	if vm.RunPID != 0 || vm.LaunchCwd != "" || vm.SharedDirs != nil {
		t.Fatalf("got pid=%d cwd=%q dirs=%v, want no launcher", vm.RunPID, vm.LaunchCwd, vm.SharedDirs)
	}
}
