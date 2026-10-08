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
	procs        []vmproc.Process
	footprint    map[int]uint64
	footprintErr error
}

// Tree returns root and its descendants, root first, like vmproc.Host.
func (f fakeProcs) Tree(root int) ([]vmproc.Process, error) {
	var out []vmproc.Process
	for _, p := range f.procs {
		if p.PID == root {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no such process")
	}
	for i := 0; i < len(out); i++ {
		for _, p := range f.procs {
			if p.PPID == out[i].PID {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

func (f fakeProcs) Footprints(_ context.Context, pids ...int) (uint64, error) {
	if f.footprintErr != nil {
		return 0, f.footprintErr
	}
	var total uint64
	for _, pid := range pids {
		total += f.footprint[pid]
	}
	return total, nil
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
	backend := vmproc.Process{PID: 500, PPID: 1, Exec: appExec, Args: []string{appExec}}
	cases := map[string]struct {
		lock  string
		procs []vmproc.Process
	}{
		"no pid lock":           {"", []vmproc.Process{backend}},
		"stale pid lock":        {"500", nil},
		"pid reused by another": {"500", []vmproc.Process{{PID: 500, Exec: "/usr/bin/other", Args: []string{"/usr/bin/other"}}}},
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

// lmTree is a running LM Studio: backend 500 with an Electron helper, two
// node workers (one holding the model, one idle), an engine process that is a
// grandchild, and an unrelated node process elsewhere.
func lmTree(home string) fakeProcs {
	node := filepath.Join(home, ".lmstudio", ".internal", "utils", "node")
	return fakeProcs{
		procs: []vmproc.Process{
			{PID: 500, PPID: 1, Exec: appExec, Args: []string{appExec, "--run-as-service"}},
			{PID: 501, PPID: 500, Exec: node, Args: []string{node, "-e", "..."}},
			{PID: 502, PPID: 500, Exec: node, Args: []string{node, "-e", "..."}},
			{PID: 503, PPID: 500, Exec: "/Applications/LM Studio.app/Contents/Frameworks/Helper", Args: []string{"Helper"}},
			{PID: 504, PPID: 501, Exec: filepath.Join(home, ".lmstudio/extensions/backends/mlx/python"), Args: []string{"python"}},
			{PID: 900, PPID: 1, Exec: "/opt/homebrew/bin/node", Args: []string{"node", "server.js"}},
		},
		footprint: map[int]uint64{500: 300 << 20, 501: 15086 << 20, 502: 70 << 20, 503: 40 << 20, 504: 1000 << 20, 900: 1 << 30},
	}
}

func running(t *testing.T, psFile string) *lmstudio.Source {
	t.Helper()
	home := lmHome(t, "500")
	cli := fakeCLI{t: t, allowed: true, files: map[string]string{"ps --json": psFile}}
	return lmstudio.New(cli, lmTree(home), home)
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
		LastUsedAt:    ptr(time.UnixMilli(1791397953705)),
	}
	if m := got.Models[0]; m.LastUsedAt == nil || !m.LastUsedAt.Equal(*want.LastUsedAt) || m.Key != want.Key || m.Type != want.Type ||
		m.Format != want.Format || m.SizeBytes != want.SizeBytes || m.ContextLength != want.ContextLength ||
		m.Status != want.Status || m.TTL != nil {
		t.Fatalf("model = %+v, want %+v", m, want)
	}
	// The whole tree under the backend, grandchildren included; not the
	// unrelated node process.
	if want := uint64(300+15086+70+40+1000) << 20; got.FootprintBytes == nil || *got.FootprintBytes != want {
		t.Fatalf("footprint = %v, want %d MiB", got.FootprintBytes, want>>20)
	}
}

func TestRunningWithNothingLoaded(t *testing.T) {
	got := collect(t, running(t, "ps-empty.json"))
	if !got.Running || len(got.Models) != 0 || got.FootprintBytes == nil {
		t.Fatalf("got %+v, want running, no models, baseline footprint", got)
	}
}

func TestLMSFailureWhileRunningIsAnError(t *testing.T) {
	src := running(t, "missing.json")
	if _, err := src.Collect(context.Background()); err == nil {
		t.Fatal("want error")
	}
}

func ptr[T any](v T) *T { return &v }

func TestBackendMatchedByExecutableNotArgv0(t *testing.T) {
	home := lmHome(t, "500")
	procs := lmTree(home)
	procs.procs[0].Args = []string{"LM Studio", "--run-as-service"} // relative argv[0]
	cli := fakeCLI{t: t, allowed: true, files: map[string]string{"ps --json": "ps-empty.json"}}
	if got := collect(t, lmstudio.New(cli, procs, home)); !got.Running {
		t.Fatal("backend with a relative argv[0] not recognised")
	}
}

func TestHeadlessBackendUnderLMStudioHome(t *testing.T) {
	home := lmHome(t, "500")
	procs := lmTree(home)
	llmster := filepath.Join(home, ".lmstudio", "bin", "llmster")
	procs.procs[0].Exec, procs.procs[0].Args = llmster, []string{llmster}
	cli := fakeCLI{t: t, allowed: true, files: map[string]string{"ps --json": "ps-empty.json"}}
	if got := collect(t, lmstudio.New(cli, procs, home)); !got.Running {
		t.Fatal("headless llmster backend not recognised")
	}
}

func TestRelativeHOMEMeansNotInstalled(t *testing.T) {
	got := collect(t, lmstudio.New(fakeCLI{t: t}, fakeProcs{}, ".lmstudio-home"))
	if got.Installed {
		t.Fatalf("got %+v, want not installed for a relative HOME", got)
	}
}

func TestUnreadableFootprintIsUnknown(t *testing.T) {
	home := lmHome(t, "500")
	procs := lmTree(home)
	procs.footprintErr = errors.New("footprint: exit status 1")
	cli := fakeCLI{t: t, allowed: true, files: map[string]string{"ps --json": "ps-empty.json"}}
	got := collect(t, lmstudio.New(cli, procs, home))
	if got.FootprintBytes != nil || got.FootprintError == "" {
		t.Fatalf("footprint=%v error=%q, want unknown with an error", got.FootprintBytes, got.FootprintError)
	}
}

func TestNeverUsedModelHasNoLastUse(t *testing.T) {
	home := lmHome(t, "500")
	cli := fakeCLI{t: t, allowed: true, files: map[string]string{"ps --json": "ps-unused.json"}}
	got := collect(t, lmstudio.New(cli, lmTree(home), home))
	if m := got.Models[0]; m.LastUsedAt != nil {
		t.Fatalf("last used = %v, want absent", *m.LastUsedAt)
	}
}

func TestBackendReachedThroughASymlink(t *testing.T) {
	// The app runs from a path that is a symlink to the installed one, as
	// /var is to /private/var.
	installed := t.TempDir()
	link := filepath.Join(t.TempDir(), "apps")
	if err := os.Symlink(installed, link); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(installed, "LM Studio")
	if err := os.WriteFile(app, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := lmHome(t, "500")
	install := `{"path":"` + app + `"}`
	if err := os.WriteFile(filepath.Join(home, ".lmstudio/.internal/app-install-location.json"), []byte(install), 0o644); err != nil {
		t.Fatal(err)
	}
	procs := lmTree(home)
	viaLink := filepath.Join(link, "LM Studio")
	procs.procs[0].Exec, procs.procs[0].Args = viaLink, []string{viaLink}
	cli := fakeCLI{t: t, allowed: true, files: map[string]string{"ps --json": "ps-empty.json"}}
	if got := collect(t, lmstudio.New(cli, procs, home)); !got.Running {
		t.Fatal("backend run through a symlinked path not recognised")
	}
}

func TestTTLAndLastUse(t *testing.T) {
	got := collect(t, running(t, "ps-ttl.json"))
	if m := got.Models[0]; m.TTL == nil || *m.TTL != time.Hour {
		t.Fatalf("ttl = %v, want 1h", m.TTL)
	}
	// LM Studio has no TTL to apply for 0 or negative: the model stays loaded.
	for _, m := range got.Models[1:] {
		if m.TTL != nil {
			t.Errorf("%s: ttl = %v, want absent", m.Key, *m.TTL)
		}
	}
	// Stored in UTC, like every other time headroom records.
	if l := got.Models[0].LastUsedAt; l == nil || l.Location() != time.UTC {
		t.Fatalf("last used = %v, want UTC", l)
	}
}
