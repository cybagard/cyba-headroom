//go:build darwin

package host_test

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/source/host"
)

// sysctlCmd reads one value with the sysctl(8) command, an independent path
// to the same kernel data.
func sysctlCmd(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("/usr/sbin/sysctl", "-n", name).Output()
	if err != nil {
		t.Fatalf("sysctl %s: %v", name, err)
	}
	return strings.TrimSpace(string(out))
}

func TestSystemReadsTheRealKernel(t *testing.T) {
	r, err := host.New(host.System{}, 5*time.Minute, time.Now).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var snap protocol.Snapshot
	r.Apply(&snap)
	h := snap.Host

	if want := sysctlCmd(t, "hw.memsize"); strconv.FormatUint(h.TotalBytes, 10) != want {
		t.Errorf("total = %d, sysctl says %s", h.TotalBytes, want)
	}
	if h.Pressure != "normal" && h.Pressure != "warn" && h.Pressure != "critical" {
		t.Errorf("pressure = %q", h.Pressure)
	}
	if h.FreePercent < 0 || h.FreePercent > 100 {
		t.Errorf("free = %d%%", h.FreePercent)
	}
	if h.SwapUsedBytes > h.SwapTotalBytes {
		t.Errorf("swap used %d > total %d", h.SwapUsedBytes, h.SwapTotalBytes)
	}
	// macOS 15 has no vm.page_wired_count (only host_statistics64, which
	// needs cgo), so memory used is unknown there; newer releases have it.
	_, wiredErr := exec.Command("/usr/sbin/sysctl", "-n", "vm.page_wired_count").Output()
	switch {
	case wiredErr != nil && h.UsedBytes != nil:
		t.Errorf("memory used = %d without vm.page_wired_count", *h.UsedBytes)
	case wiredErr == nil && h.UsedBytes == nil:
		t.Error("memory used unknown, though vm.page_wired_count exists")
	case h.UsedBytes != nil && (*h.UsedBytes == 0 || *h.UsedBytes > h.TotalBytes):
		t.Errorf("memory used = %d, total %d", *h.UsedBytes, h.TotalBytes)
	}
	// sysctl prints e.g. "total = 2048.00M  used = 1024.00M ...".
	if !strings.Contains(sysctlCmd(t, "vm.swapusage"), "total = ") {
		t.Error("unexpected vm.swapusage format")
	}
}
