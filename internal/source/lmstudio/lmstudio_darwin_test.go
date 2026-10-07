//go:build darwin

package lmstudio_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/source/lmstudio"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

func TestRealLMStudio(t *testing.T) {
	path := lmstudio.Locate("", os.Getenv)
	if path == "" {
		t.Skip("LM Studio not installed")
	}
	got := collect(t, lmstudio.New(lmstudio.Exec{Path: path}, vmproc.Host{}, os.Getenv("HOME")))
	if !got.Running {
		t.Skip("LM Studio not running (and must not be woken by this test)")
	}
	// Only now is it safe to call lms directly: it is the oracle.
	out, err := exec.Command(path, "ps", "--json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var loaded []json.RawMessage
	if err := json.Unmarshal(out, &loaded); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != len(loaded) || got.FootprintBytes == nil {
		t.Fatalf("%d models, footprint %v; lms ps lists %d", len(got.Models), got.FootprintBytes, len(loaded))
	}
}
