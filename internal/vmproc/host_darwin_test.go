//go:build darwin

package vmproc_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// psVMs lists VM processes by executable path with ps(1), an independent
// path to the same data. (pgrep -f would also match command lines that merely
// mention the name.)
func psVMs(t *testing.T) []int {
	t.Helper()
	out, err := exec.Command("/bin/ps", "-axo", "pid=,comm=").Output()
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		pid, comm, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !strings.HasSuffix(comm, "/com.apple.Virtualization.VirtualMachine") {
			continue
		}
		n, err := strconv.Atoi(pid)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, n)
	}
	slices.Sort(pids)
	return pids
}

func TestHostListsTheRealVMs(t *testing.T) {
	want := psVMs(t)
	got, err := vmproc.Host{}.VMPIDs()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("VMPIDs = %v, ps says %v", got, want)
	}
	if len(want) == 0 {
		t.Skip("no VM running; start Docker or a Tart VM to check classification")
	}
	vms, err := vmproc.New(vmproc.Host{}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, vm := range vms {
		if vm.Kind == vmproc.Unknown || vm.FootprintBytes == 0 {
			t.Errorf("VM %+v not classified or no footprint", vm)
		}
	}
}

func TestHostProcessesNamedAndCwd(t *testing.T) {
	self := os.Getpid()
	comm := filepath.Base(os.Args[0])
	if len(comm) > 16 {
		comm = comm[:16] // the kernel keeps MAXCOMLEN bytes
	}
	procs, err := vmproc.Host{}.ProcessesNamed(comm)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(procs, func(p vmproc.Process) bool { return p.PID == self })
	if i < 0 {
		t.Fatalf("own process %d (%q) not in %+v", self, comm, procs)
	}
	p := procs[i]
	if p.PPID != os.Getppid() || !slices.Equal(p.Args, os.Args) {
		t.Fatalf("got ppid=%d args=%q, want %d %q", p.PPID, p.Args, os.Getppid(), os.Args)
	}

	wd, _ := os.Getwd()
	wd, _ = filepath.EvalSymlinks(wd)
	cwd, err := vmproc.Host{}.Cwd(context.Background(), self)
	if err != nil || cwd != wd {
		t.Fatalf("Cwd = %q, %v; want %q", cwd, err, wd)
	}
}

func TestHostTreeAndFootprints(t *testing.T) {
	child := exec.Command("/bin/sleep", "5")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()

	self := os.Getpid()
	tree, err := vmproc.Host{}.Tree(self)
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	root, _ := filepath.EvalSymlinks(tree[0].Exec) // the kernel keeps the path as invoked
	if tree[0].PID != self || root != exe {
		t.Fatalf("root = %+v, want pid %d exec %q", tree[0], self, exe)
	}
	i := slices.IndexFunc(tree, func(p vmproc.Process) bool { return p.PID == child.Process.Pid })
	if i < 0 || tree[i].Exec != "/bin/sleep" || tree[i].PPID != self {
		t.Fatalf("child %d not in tree %+v", child.Process.Pid, tree)
	}

	// One footprint call for both equals the two read one by one.
	both, err := vmproc.Host{}.Footprints(context.Background(), self, child.Process.Pid)
	if err != nil || both == 0 {
		t.Fatalf("Footprints = %d, %v", both, err)
	}
	one, _ := vmproc.Host{}.Footprint(context.Background(), child.Process.Pid)
	if one == 0 || one >= both {
		t.Fatalf("child footprint %d not part of total %d", one, both)
	}
}

func TestHostListsOtherUsersProcessesWithoutArguments(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every process's arguments")
	}
	procs, err := vmproc.Host{}.ProcessesNamed("launchd")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(procs, func(p vmproc.Process) bool { return p.PID == 1 })
	if i < 0 {
		t.Fatalf("launchd (pid 1, root's) not in %+v", procs)
	}
	if p := procs[i]; p.ArgsErr == nil || p.Args != nil || p.Exec != "" {
		t.Fatalf("got %+v, want no arguments and why", p)
	}
}
