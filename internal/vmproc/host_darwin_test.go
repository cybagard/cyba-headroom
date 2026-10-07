//go:build darwin

package vmproc_test

import (
	"context"
	"os/exec"
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
