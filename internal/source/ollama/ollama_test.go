package ollama_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/source/ollama"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

const serverExec = "/opt/homebrew/Cellar/ollama/0.40.1/bin/ollama"

// fakeAPI answers /api/ps from testdata, and fails the test if it is called
// when it must not be: headroom never contacts an Ollama it did not find
// running.
type fakeAPI struct {
	t       *testing.T
	file    string
	allowed bool
}

func (f fakeAPI) PS(context.Context) ([]byte, error) {
	if !f.allowed {
		f.t.Error("/api/ps called while no Ollama server is running")
		return nil, errors.New("must not be called")
	}
	if f.file == "" {
		return nil, errors.New("connection refused")
	}
	return os.ReadFile("testdata/" + f.file)
}

// fakeProcs is a process table with footprints.
type fakeProcs struct {
	procs        []vmproc.Process
	footprint    map[int]uint64
	footprintErr error
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

// serverTree is a running Ollama: server 700 with one runner, plus an
// `ollama run` client in a terminal, which is not part of the server.
func serverTree() fakeProcs {
	return fakeProcs{
		procs: []vmproc.Process{
			{PID: 700, PPID: 1, Comm: "ollama", Exec: serverExec, Args: []string{"ollama", "serve"}},
			{PID: 701, PPID: 700, Comm: "ollama", Exec: serverExec, Args: []string{serverExec, "runner", "--model", "/x", "--port", "50000"}},
			{PID: 800, PPID: 2, Comm: "ollama", Exec: serverExec, Args: []string{"ollama", "run", "llama3.2:1b"}},
		},
		footprint: map[int]uint64{700: 12 << 20, 701: 2300 << 20, 800: 1 << 30},
	}
}

func collect(t *testing.T, src *ollama.Source) protocol.Ollama {
	t.Helper()
	r, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var snap protocol.Snapshot
	r.Apply(&snap)
	if snap.Ollama == nil {
		t.Fatal("reading did not fill Snapshot.Ollama")
	}
	return *snap.Ollama
}

func TestLoadedModelAndMeasuredCost(t *testing.T) {
	src := ollama.New(fakeAPI{t: t, file: "ps-one.json", allowed: true}, serverTree(), is(true))
	got := collect(t, src)
	if !got.Installed || !got.Running || len(got.Models) != 1 {
		t.Fatalf("got %+v, want running with one model", got)
	}
	m := got.Models[0]
	exp := time.Date(2026, 10, 8, 12, 5, 0, 123456000, time.UTC)
	if m.Name != "llama3.2:1b" || m.SizeBytes != 2210000000 || m.VRAMBytes != 2210000000 || m.ContextLength != 4096 ||
		m.ExpiresAt == nil || !m.ExpiresAt.Equal(exp) || m.ExpiresAt.Location() != time.UTC {
		t.Fatalf("model = %+v, want llama3.2:1b, 2.21 GB, ctx 4096, expiring %v UTC", m, exp)
	}
	// The server and its runner; not the `ollama run` client.
	if want := uint64(12+2300) << 20; got.FootprintBytes == nil || *got.FootprintBytes != want {
		t.Fatalf("footprint = %v, want %d MiB", got.FootprintBytes, want>>20)
	}
}

func TestNotRunningNeverCallsTheAPI(t *testing.T) {
	client := fakeProcs{procs: serverTree().procs[2:]} // only `ollama run`
	for name, tc := range map[string]struct {
		procs     fakeProcs
		installed bool
	}{
		"not installed":        {fakeProcs{}, false},
		"installed, idle":      {fakeProcs{}, true},
		"client but no server": {client, true},
	} {
		t.Run(name, func(t *testing.T) {
			got := collect(t, ollama.New(fakeAPI{t: t}, tc.procs, is(tc.installed)))
			if got.Installed != tc.installed || got.Running || got.Models == nil {
				t.Fatalf("got %+v, want installed=%v, not running, empty model list", got, tc.installed)
			}
		})
	}
}

func TestRunningServerMeansInstalled(t *testing.T) {
	// A server run from a path headroom does not search, e.g. a build dir.
	got := collect(t, ollama.New(fakeAPI{t: t, file: "ps-empty.json", allowed: true}, serverTree(), is(false)))
	if !got.Installed || !got.Running || len(got.Models) != 0 {
		t.Fatalf("got %+v, want installed and running with no models", got)
	}
}

func TestAPIFailureWhileRunningKeepsTheFootprint(t *testing.T) {
	// e.g. the server listens on a port headroom was not told about.
	got := collect(t, ollama.New(fakeAPI{t: t, allowed: true}, serverTree(), is(true)))
	if !got.Running || got.ModelsError == "" || got.Models != nil || got.FootprintBytes == nil {
		t.Fatalf("got %+v, want running, models unknown with a reason, footprint measured", got)
	}
}

func TestMalformedPSKeepsTheFootprint(t *testing.T) {
	got := collect(t, ollama.New(fakeAPI{t: t, file: "../ollama_test.go", allowed: true}, serverTree(), is(true)))
	if got.ModelsError == "" || got.FootprintBytes == nil {
		t.Fatalf("got %+v, want models unknown, footprint measured", got)
	}
}

func TestEveryServerIsMeasured(t *testing.T) {
	procs := serverTree()
	procs.procs = append(procs.procs,
		vmproc.Process{PID: 900, PPID: 1, Comm: "ollama", Exec: "/Applications/Ollama.app/Contents/Resources/ollama", Args: []string{"ollama", "serve"}},
		vmproc.Process{PID: 901, PPID: 900, Comm: "ollama", Exec: "/Applications/Ollama.app/Contents/Resources/ollama", Args: []string{"ollama", "runner"}})
	procs.footprint[900], procs.footprint[901] = 20<<20, 4000<<20
	got := collect(t, ollama.New(fakeAPI{t: t, file: "ps-empty.json", allowed: true}, procs, is(true)))
	if want := uint64(12+2300+20+4000) << 20; got.FootprintBytes == nil || *got.FootprintBytes != want {
		t.Fatalf("footprint = %v, want %d MiB", got.FootprintBytes, want>>20)
	}
}

// vanishingProcs lists a server that has exited by the time Tree is asked.
type vanishingProcs struct{ fakeProcs }

func (vanishingProcs) Tree(int) ([]vmproc.Process, error) { return nil, errors.New("no such process") }

func TestServerExitingMidTickIsNotRunning(t *testing.T) {
	got := collect(t, ollama.New(fakeAPI{t: t}, vanishingProcs{serverTree()}, is(true)))
	if got.Running {
		t.Fatalf("got %+v, want not running", got)
	}
}

func TestInstalledIsCheckedEachTick(t *testing.T) {
	installed := false
	src := ollama.New(fakeAPI{t: t}, fakeProcs{}, func() bool { return installed })
	if collect(t, src).Installed {
		t.Fatal("installed before install")
	}
	installed = true
	if !collect(t, src).Installed {
		t.Fatal("install while the daemon runs not seen")
	}
}

func TestUnreadableFootprintIsUnknown(t *testing.T) {
	procs := serverTree()
	procs.footprintErr = errors.New("footprint: exit status 1")
	got := collect(t, ollama.New(fakeAPI{t: t, file: "ps-empty.json", allowed: true}, procs, is(true)))
	if got.FootprintBytes != nil || got.FootprintError == "" {
		t.Fatalf("footprint=%v error=%q, want unknown with an error", got.FootprintBytes, got.FootprintError)
	}
}

func TestKeptLoadedForeverHasNoExpiry(t *testing.T) {
	// keep_alive -1: Ollama reports now + the largest duration, in 2318.
	got := collect(t, ollama.New(fakeAPI{t: t, file: "ps-forever.json", allowed: true}, serverTree(), is(true)))
	if m := got.Models[0]; m.ExpiresAt != nil {
		t.Fatalf("expires = %v, want absent", *m.ExpiresAt)
	}
}

func TestRemoteEndpointLeavesModelsUnknown(t *testing.T) {
	got := collect(t, ollama.New(ollama.NotLocal{Host: "gpu-box:11434"}, serverTree(), is(true)))
	if !got.Running || !strings.Contains(got.ModelsError, `"gpu-box:11434"`) || got.FootprintBytes == nil {
		t.Fatalf("got %+v, want running, models unknown naming the host, footprint measured", got)
	}
}

func is(b bool) func() bool { return func() bool { return b } }
