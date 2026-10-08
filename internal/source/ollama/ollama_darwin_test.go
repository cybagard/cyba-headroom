//go:build darwin

package ollama_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/source/ollama"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

func TestRealOllama(t *testing.T) {
	base, ok := ollama.Endpoint("", os.Getenv("OLLAMA_HOST"))
	if !ok {
		t.Skip("OLLAMA_HOST is not on this Mac")
	}
	api := ollama.NewHTTP(base)
	installed := ollama.Installed("", os.Getenv, ollama.Locations)
	got := collect(t, ollama.New(api, vmproc.Host{}, func() bool { return installed }))
	if !got.Running {
		if !installed {
			t.Skip("Ollama not installed")
		}
		t.Skip("Ollama not running (and must not be started by this test)")
	}
	// The API is the oracle for the model list.
	out, err := api.PS(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ps struct{ Models []json.RawMessage }
	if err := json.Unmarshal(out, &ps); err != nil {
		t.Fatal(err)
	}
	if !got.Installed || len(got.Models) != len(ps.Models) || got.FootprintBytes == nil || *got.FootprintBytes == 0 {
		t.Fatalf("got %+v; /api/ps lists %d models", got, len(ps.Models))
	}
}
