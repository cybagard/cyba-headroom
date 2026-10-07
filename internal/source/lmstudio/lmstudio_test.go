package lmstudio_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/source/lmstudio"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

const appExec = "/Applications/LM Studio.app/Contents/MacOS/LM Studio"

// fakeCLI answers lms subcommands from testdata, and fails the test if it is
// called when it must not be: lms wakes LM Studio when it is not running.
type fakeCLI struct {
	t       *testing.T
	files   map[string]string
	allowed bool
}

func (f fakeCLI) Run(_ context.Context, args ...string) ([]byte, error) {
	if !f.allowed {
		f.t.Errorf("lms %s called while LM Studio is not running: it would wake it", strings.Join(args, " "))
		return nil, errors.New("must not be called")
	}
	file, ok := f.files[strings.Join(args, " ")]
	if !ok {
		return nil, errors.New("exit status 1")
	}
	return os.ReadFile("testdata/" + file)
}

// fakeProcs is a process table with footprints.
type fakeProcs struct {
	procs     []vmproc.Process
	footprint map[int]uint64
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

func (f fakeProcs) Footprint(_ context.Context, pid int) (uint64, error) {
	if fp, ok := f.footprint[pid]; ok {
		return fp, nil
	}
	return 0, errors.New("no such process")
}

// lmHome creates ~/.lmstudio/.internal with the install location and,
// if pid > 0, the backend's pid lock.
func lmHome(t *testing.T, pid string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".lmstudio", ".internal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	install := `{"path":"` + appExec + `","argv":["` + appExec + `","--run-as-service"]}`
	if err := os.WriteFile(filepath.Join(dir, "app-install-location.json"), []byte(install), 0o644); err != nil {
		t.Fatal(err)
	}
	if pid != "" {
		if err := os.WriteFile(filepath.Join(dir, "llmster-pid.lock"), []byte(pid), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func collect(t *testing.T, src *lmstudio.Source) protocol.LMStudio {
	t.Helper()
	r, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var snap protocol.Snapshot
	r.Apply(&snap)
	if snap.LMStudio == nil {
		t.Fatal("reading did not fill Snapshot.LMStudio")
	}
	return *snap.LMStudio
}

func TestNotInstalled(t *testing.T) {
	got := collect(t, lmstudio.New(nil, fakeProcs{}, t.TempDir()))
	if got.Installed || got.Running || got.Models == nil {
		t.Fatalf("got %+v, want not installed with an empty model list", got)
	}
}

func TestNotRunningNeverCallsLMS(t *testing.T) {
	backend := vmproc.Process{PID: 500, PPID: 1, Comm: "LM Studio", Args: []string{appExec}}
	cases := map[string]struct {
		lock  string
		procs []vmproc.Process
	}{
		"no pid lock":           {"", []vmproc.Process{backend}},
		"stale pid lock":        {"500", nil},
		"pid reused by another": {"500", []vmproc.Process{{PID: 500, Comm: "LM Studio", Args: []string{"/usr/bin/other"}}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			src := lmstudio.New(fakeCLI{t: t}, fakeProcs{procs: tc.procs}, lmHome(t, tc.lock))
			got := collect(t, src)
			if !got.Installed || got.Running {
				t.Fatalf("got %+v, want installed but not running", got)
			}
		})
	}
}

// running returns a source over a running LM Studio: backend 500 with two
// node workers (one holding the model, one idle utility) and an unrelated
// node process elsewhere.
func running(t *testing.T, psFile string) *lmstudio.Source {
	t.Helper()
	home := lmHome(t, "500")
	node := filepath.Join(home, ".lmstudio", ".internal", "utils", "node")
	procs := fakeProcs{
		procs: []vmproc.Process{
			{PID: 500, PPID: 1, Comm: "LM Studio", Args: []string{appExec, "--run-as-service"}},
			{PID: 501, PPID: 500, Comm: "node", Args: []string{node, "-e", "..."}},
			{PID: 502, PPID: 500, Comm: "node", Args: []string{node, "-e", "..."}},
			{PID: 900, PPID: 1, Comm: "node", Args: []string{"/opt/homebrew/bin/node", "server.js"}},
		},
		footprint: map[int]uint64{500: 300 << 20, 501: 15086 << 20, 502: 70 << 20, 900: 1 << 30},
	}
	cli := fakeCLI{t: t, allowed: true, files: map[string]string{"ps --json": psFile}}
	return lmstudio.New(cli, procs, home)
}

func TestLoadedModelAndMeasuredCost(t *testing.T) {
	got := collect(t, running(t, "ps-mlx.json"))
	if !got.Installed || !got.Running || len(got.Models) != 1 {
		t.Fatalf("got %+v, want running with one model", got)
	}
	want := protocol.LoadedModel{
		Key:           "devstral-small-2-24b-instruct-2512",
		Type:          "llm",
		Format:        "safetensors",
		SizeBytes:     15136817368,
		ContextLength: 180736,
		Status:        "idle",
		LastUsedAt:    time.UnixMilli(1791397953705),
	}
	if m := got.Models[0]; !m.LastUsedAt.Equal(want.LastUsedAt) || m.Key != want.Key || m.Type != want.Type ||
		m.Format != want.Format || m.SizeBytes != want.SizeBytes || m.ContextLength != want.ContextLength ||
		m.Status != want.Status || m.TTL != nil {
		t.Fatalf("model = %+v, want %+v", m, want)
	}
	// Backend plus its own workers; not the unrelated node process.
	if want := uint64(300+15086+70) << 20; got.FootprintBytes != want {
		t.Fatalf("footprint = %d MiB, want %d MiB", got.FootprintBytes>>20, want>>20)
	}
}

func TestRunningWithNothingLoaded(t *testing.T) {
	got := collect(t, running(t, "ps-empty.json"))
	if !got.Running || len(got.Models) != 0 || got.FootprintBytes == 0 {
		t.Fatalf("got %+v, want running, no models, baseline footprint", got)
	}
}

func TestLMSFailureWhileRunningIsAnError(t *testing.T) {
	src := running(t, "missing.json")
	if _, err := src.Collect(context.Background()); err == nil {
		t.Fatal("want error")
	}
}
