//go:build darwin

package tart_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/source/tart"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

func TestRealTart(t *testing.T) {
	path := tart.Locate("", os.Getenv)
	if path == "" {
		t.Skip("tart not installed")
	}
	// tart list is the oracle for which VMs run.
	out, err := exec.Command(path, "list", "--format", "json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var list []struct {
		Name    string
		Running bool
	}
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatal(err)
	}
	running := 0
	for _, v := range list {
		if v.Running {
			running++
		}
	}

	got := collect(t, tart.New(tart.Exec{Path: path}, vmproc.Host{}, vmproc.New(vmproc.Host{}), os.Getenv("HOME")))
	if !got.Installed || len(got.VMs) != running {
		t.Fatalf("got %d VMs, tart list says %d running", len(got.VMs), running)
	}
	for _, vm := range got.VMs {
		if vm.MemoryBytes == 0 || vm.FootprintBytes == nil || vm.RunPID == 0 {
			t.Errorf("VM %+v missing memory, footprint or run pid", vm)
		}
	}
}
