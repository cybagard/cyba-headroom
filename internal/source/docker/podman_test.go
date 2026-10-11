package docker_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/lease"
	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/source/docker"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// recordedSocket is the socket path in testdata/podman-machine-inspect.json.
const recordedSocket = "/var/folders/00/T/podman/podman-machine-default-api.sock"

// fakePodman answers the podman CLI from recorded output: list is
// `podman machine list --format json`'s, and inspect, by machine name,
// `podman machine inspect <name>`'s, with recordedSocket swapped for the
// socket in sockets. It records each call.
type fakePodman struct {
	mu      sync.Mutex
	list    string
	inspect map[string]string // machine name → recorded file
	sockets map[string]string // machine name → socket
	calls   []string
}

func (f *fakePodman) Run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case call == "machine list --format json":
		return read(f.list)
	case strings.HasPrefix(call, "machine inspect "):
		name := strings.TrimPrefix(call, "machine inspect ")
		file, ok := f.inspect[name]
		if !ok {
			return []byte("[]\n"), errors.New("exit status 125") // Error: <name>: VM does not exist
		}
		b, err := read(file)
		return bytes.ReplaceAll(b, []byte(recordedSocket), []byte(f.sockets[name])), err
	}
	return nil, fmt.Errorf("unexpected podman %s", call)
}

func (f *fakePodman) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// read returns testdata's file, or inline JSON as it is.
func read(body string) ([]byte, error) {
	if strings.HasPrefix(body, "[") {
		return []byte(body), nil
	}
	return os.ReadFile("testdata/" + body)
}

// machine is a recorded default machine whose socket is sock.
func machine(sock string) *fakePodman {
	return &fakePodman{
		list:    "podman-machine-list.json",
		inspect: map[string]string{"podman-machine-default": "podman-machine-inspect.json"},
		sockets: map[string]string{"podman-machine-default": sock},
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// Podman's socket is the one podman machine inspect names, a trusted
// candidate of its machine.
func TestPodmanSocketFromInspect(t *testing.T) {
	sock := listen(t, filepath.Join(shortDir(t), "api.sock"))
	p := docker.NewPodman(machine(sock), (&fakeClock{t0}).now, quiet)
	got := p.Candidates()
	want := []docker.Candidate{{Socket: sock, Trusted: true, Machine: "podman-machine-default"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

// The socket found is cached: inspect does not run again while it answers.
func TestPodmanInspectNotRunWhileTheSocketAnswers(t *testing.T) {
	sock := listen(t, filepath.Join(shortDir(t), "api.sock"))
	cli := machine(sock)
	clock := &fakeClock{t0}
	p := docker.NewPodman(cli, clock.now, quiet)
	p.Candidates()
	ran := cli.count()
	for range 5 {
		clock.t = clock.t.Add(time.Minute)
		if got := p.Candidates(); len(got) != 1 || got[0].Socket != sock {
			t.Fatalf("candidates = %v", got)
		}
	}
	if cli.count() != ran {
		t.Fatalf("podman ran %v, want no more than the first %d calls", cli.calls, ran)
	}
}

// A socket that stops answering is read again from inspect, at most once
// every 30 s: the machine restarted on a new socket.
func TestPodmanInspectRereadAfterTheSocketRefuses(t *testing.T) {
	dir := shortDir(t)
	old := filepath.Join(dir, "old.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: old, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false) // the file stays: it refuses
	cli := machine(old)
	clock := &fakeClock{t0}
	p := docker.NewPodman(cli, clock.now, quiet)
	p.Candidates()
	_ = ln.Close()
	cli.sockets["podman-machine-default"] = listen(t, filepath.Join(dir, "new.sock"))
	ran := cli.count()

	// Within 30 s of the last inspect, the dead socket stands.
	clock.t = t0.Add(29 * time.Second)
	if got := p.Candidates(); len(got) != 1 || got[0].Socket != old || cli.count() != ran {
		t.Fatalf("at 29 s: candidates = %v, calls = %v; want the old socket, no inspect", got, cli.calls)
	}
	clock.t = t0.Add(30 * time.Second)
	if got := p.Candidates(); len(got) != 1 || got[0].Socket != filepath.Join(dir, "new.sock") || cli.count() == ran {
		t.Fatalf("at 30 s: candidates = %v, calls = %v; want the new socket, from inspect", got, cli.calls)
	}
}

// A missing socket (the machine stopped) is read again too, rate-limited:
// a stopped machine runs the CLI once every 30 s, not every tick.
func TestPodmanStoppedMachineIsInspectedEvery30s(t *testing.T) {
	gone := filepath.Join(shortDir(t), "api.sock") // never listened on
	cli := machine(gone)
	clock := &fakeClock{t0}
	p := docker.NewPodman(cli, clock.now, quiet)
	p.Candidates()
	first := cli.count()
	for s := 5; s < 30; s += 5 { // the daemon's ticks
		clock.t = t0.Add(time.Duration(s) * time.Second)
		p.Candidates()
	}
	if cli.count() != first {
		t.Fatalf("within 30 s: podman ran %v", cli.calls)
	}
	clock.t = t0.Add(30 * time.Second)
	p.Candidates()
	if cli.count() != 2*first {
		t.Fatalf("at 30 s: podman ran %v, want inspect again", cli.calls)
	}
	// Its path is still the candidate: Candidates leaves a missing one out.
	if got := docker.Candidates("", env(nil), quiet, p.Candidates()...); len(got) != 0 {
		t.Fatalf("candidates = %v, want none for a stopped machine", got)
	}
}

// Without a default machine, the first running one's socket is used.
func TestPodmanFirstRunningMachineWithoutADefault(t *testing.T) {
	sock := listen(t, filepath.Join(shortDir(t), "api.sock"))
	cli := &fakePodman{
		list:    `[{"Name":"a","Default":false,"Running":false},{"Name":"b","Default":false,"Running":true},{"Name":"c","Default":false,"Running":true}]`,
		inspect: map[string]string{"b": "podman-machine-inspect.json"},
		sockets: map[string]string{"b": sock},
	}
	p := docker.NewPodman(cli, (&fakeClock{t0}).now, quiet)
	if got := p.Candidates(); len(got) != 1 || got[0].Socket != sock || got[0].Machine != "b" {
		t.Fatalf("candidates = %v, calls = %v; want machine b's", got, cli.calls)
	}
}

// No podman CLI, or no machine, is no candidate, logged once, not every
// tick.
func TestPodmanNoCLIOrNoMachineIsNoCandidateLoggedOnce(t *testing.T) {
	for name, cli := range map[string]docker.PodmanCLI{
		"no CLI":     nil,
		"no machine": &fakePodman{list: `[]`},
		"none runs":  &fakePodman{list: `[{"Name":"a","Default":false,"Running":false}]`},
	} {
		var log bytes.Buffer
		clock := &fakeClock{t0}
		p := docker.NewPodman(cli, clock.now, slog.New(slog.NewTextHandler(&log, nil)))
		for range 20 {
			if got := p.Candidates(); len(got) != 0 {
				t.Fatalf("%s: candidates = %v, want none", name, got)
			}
			clock.t = clock.t.Add(5 * time.Second)
		}
		if n := strings.Count(log.String(), "\n"); n != 1 {
			t.Errorf("%s: logged %d lines, want 1:\n%s", name, n, log.String())
		}
	}
}

// Podman's socket is a candidate after Docker Desktop's.
func TestPodmanCandidateComesAfterDockerDesktops(t *testing.T) {
	home := shortDir(t)
	desktop := listen(t, filepath.Join(home, ".docker", "run", "docker.sock"))
	sock := listen(t, filepath.Join(home, "api.sock"))
	p := docker.NewPodman(machine(sock), (&fakeClock{t0}).now, quiet)
	got := docker.Candidates("", env(map[string]string{"HOME": home}), quiet, p.Candidates()...)
	want := []docker.Candidate{{Socket: desktop, Trusted: true}, {Socket: sock, Trusted: true, Machine: "podman-machine-default"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

// Podman's socket named earlier too (DOCKER_HOST) comes once, by its first
// name, and keeps its machine.
func TestPodmanSocketNamedTwiceKeepsItsMachine(t *testing.T) {
	sock := listen(t, filepath.Join(shortDir(t), "api.sock"))
	p := docker.NewPodman(machine(sock), (&fakeClock{t0}).now, quiet)
	got := docker.Candidates("", env(map[string]string{"DOCKER_HOST": "unix://" + sock}), quiet, p.Candidates()...)
	want := []docker.Candidate{{Socket: sock, Trusted: true, Machine: "podman-machine-default"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

// podmanID is a container on the Podman engine.
const podmanID = "0d1c2b3a4f5e6d7c8b9a0f1e2d3c4b5a6f7e8d9c0b1a2f3e4d5c6b7a8f9e0d1c"

// podmanEngine serves Podman's engine, as the spike read it, running one
// container with labels.
func podmanEngine(t *testing.T, labels string) string {
	t.Helper()
	sock, _ := engine(t, map[string]string{
		"/info":            "info-podman.json",
		"/version":         "version-podman.json",
		"/containers/json": `[{"Id":"` + podmanID + `","Names":["/cache"],"Image":"docker.io/library/redis:7-alpine","Labels":` + labels + `}]`,
		"/containers/" + podmanID + "/stats?stream=false&one-shot=true": cpuStats(1, 1),
	})
	return sock
}

var podmanVM = vmproc.VM{PID: 30, Kind: vmproc.Podman, Name: "podman-machine-default", FootprintBytes: 1085276160}

// Podman's engine, found from inspect, is of kind podman, with its
// containers, and its VM is the Podman VM of its machine.
func TestPodmanEngineWithItsContainersAndVM(t *testing.T) {
	sock := podmanEngine(t, `{}`)
	p := docker.NewPodman(machine(sock), (&fakeClock{t0}).now, quiet)
	vms := fakeVMs{{PID: 2, Kind: vmproc.Docker, FootprintBytes: 1709280520}, {PID: 31, Kind: vmproc.Podman, Name: "other", FootprintBytes: 1}, podmanVM}
	got := found(t, vms, p.Candidates()...)
	if len(got) != 1 {
		t.Fatalf("engines = %+v, want one", got)
	}
	e := got[0]
	if e.Kind != protocol.EnginePodman || e.Machine != "podman-machine-default" || !e.Running || e.Socket != sock ||
		e.ID != "podman:podman-machine-default" || e.VMLimitBytes != 2037100544 {
		t.Errorf("engine = %+v", e)
	}
	if len(e.Containers) != 1 || e.Containers[0].ID != podmanID || e.Containers[0].Name != "cache" || e.Containers[0].MemoryBytes != 1<<20 {
		t.Errorf("containers = %+v", e.Containers)
	}
	if e.VMError != "" || !e.VMRunning || e.VMFootprintBytes != podmanVM.FootprintBytes {
		t.Errorf("VM: running %v, footprint %d, error %q; want the machine's", e.VMRunning, e.VMFootprintBytes, e.VMError)
	}
}

// randomPodman serves Podman's engine as Podman does: /info answers with
// a new random ID on every request (review round 1, B1). It runs one
// container whose CPU counters grow by a second in 100 each reading.
func randomPodman(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(shortDir(t), "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	infos, stats := 0, 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body string
		switch r.URL.RequestURI() {
		case "/info":
			infos++
			body = fmt.Sprintf(`{"ID":"00000000-0000-4000-8000-%012d","MemTotal":2037100544}`, infos)
		case "/version":
			b, _ := os.ReadFile("testdata/version-podman.json")
			body = string(b)
		case "/containers/json":
			body = `[{"Id":"` + podmanID + `","Names":["/cache"],"Image":"docker.io/library/redis:7-alpine"}]`
		case "/containers/" + podmanID + "/stats?stream=false&one-shot=true":
			stats++
			body = cpuStats(uint64(stats)*1e9, uint64(stats)*100e9)
		default:
			http.Error(w, `{"message":"no route"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return sock
}

// Podman's engine keeps one ID though its /info's changes on every
// request: podman:<machine>, its candidate being from inspect (#228, plan
// amendment 2).
func TestPodmanEngineIDIsStable(t *testing.T) {
	sock := randomPodman(t)
	p := docker.NewPodman(machine(sock), (&fakeClock{t0}).now, quiet)
	src := docker.Discover(p.Candidates, fakeVMs{podmanVM})
	var ids []string
	for range 2 {
		ids = append(ids, collect(t, src).ID)
	}
	if ids[0] != "podman:podman-machine-default" || ids[1] != ids[0] {
		t.Fatalf("IDs = %q, want podman:podman-machine-default twice", ids)
	}
}

// Reached through another name (DOCKER_HOST, a context) with no machine,
// Podman's engine is its resolved socket path.
func TestPodmanEngineWithoutAMachineIsItsSocket(t *testing.T) {
	sock := randomPodman(t)
	link := filepath.Join(shortDir(t), "link.sock")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	src := docker.Discover(func() []docker.Candidate { return []docker.Candidate{{Socket: link, Trusted: true}} }, fakeVMs{})
	resolved, err := filepath.EvalSymlinks(sock)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if e := collect(t, src); e.ID != resolved || e.Socket != link {
			t.Fatalf("engine = %+v, want ID %s", e, resolved)
		}
	}
}

// Podman's containers' CPU is computed from the previous reading's.
func TestPodmanCPUOnTheSecondReading(t *testing.T) {
	sock := randomPodman(t)
	src := docker.Discover(docker.NewPodman(machine(sock), (&fakeClock{t0}).now, quiet).Candidates, fakeVMs{podmanVM})
	if c := collect(t, src).Containers; len(c) != 1 || c[0].CPUPercent != nil {
		t.Fatalf("first reading: containers = %+v, want CPU unknown", c)
	}
	// 1 s in 100, on 16 CPUs
	if c := collect(t, src).Containers; len(c) != 1 || c[0].CPUPercent == nil || *c[0].CPUPercent != 16 {
		t.Fatalf("second reading: containers = %+v, want 16%% CPU", c)
	}
}

// A Podman engine whose machine's VM is not listed has its VM not running;
// one of no known machine has its VM unknown.
func TestPodmanEngineVMNotListedOrMachineUnknown(t *testing.T) {
	sock := podmanEngine(t, `{}`)
	got := found(t, fakeVMs{}, docker.Candidate{Socket: sock, Trusted: true, Machine: "podman-machine-default"})
	if len(got) != 1 || got[0].VMRunning || got[0].VMError != "" {
		t.Errorf("listed none: engine = %+v, want its VM not running", got)
	}
	got = found(t, fakeVMs{podmanVM}, docker.Candidate{Socket: sock, Trusted: true})
	if len(got) != 1 || got[0].VMRunning || got[0].VMError == "" {
		t.Errorf("no machine: engine = %+v, want its VM unknown", got)
	}
}

// A podman run through the shim (its container labelled with its lease)
// binds its lease when the container appears on Podman's engine, and so
// never ends as "never appeared".
func TestAShimGatedPodmanRunBindsItsLease(t *testing.T) {
	sock, set := engine(t, map[string]string{
		"/info":            "info-podman.json",
		"/version":         "version-podman.json",
		"/containers/json": `[]`,
	})
	src := docker.Discover(docker.NewPodman(machine(sock), (&fakeClock{t0}).now, quiet).Candidates, fakeVMs{podmanVM})
	clock := &fakeClock{t0}
	var log bytes.Buffer
	book := lease.New(2*time.Minute, clock.now, slog.New(slog.NewTextHandler(&log, nil)))
	const gib = 1 << 30
	read := func() *protocol.Snapshot {
		t.Helper()
		r, err := src.Collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		s := &protocol.Snapshot{
			Host:        &protocol.Host{TotalBytes: 64 * gib, Pressure: "normal"},
			Budget:      &protocol.Budget{TotalBytes: 64 * gib, HeadroomBytes: new(int64(8 * gib))},
			Orca:        &protocol.Orca{Running: true, Worktrees: []protocol.Worktree{{ID: "w1", Name: "A", Agents: []protocol.Agent{{State: "working"}}}}},
			Attribution: &protocol.Attribution{Worktrees: []protocol.WorktreeUsage{{ID: "w1"}}},
			CollectedAt: clock.t,
		}
		r.Apply(s)
		return s
	}
	book.Observe(read())
	d := book.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "podman run redis", CostBytes: gib, Labelled: true}, read(), policy.Config{PressureGuard: "critical", DefaultContainerBytes: gib})
	if !d.Allow || d.LeaseID == "" {
		t.Fatalf("check = %+v", d)
	}
	set("/containers/json", `[{"Id":"`+podmanID+`","Names":["/cache"],"Image":"docker.io/library/redis:7-alpine","Labels":{"`+protocol.LeaseLabel+`":"`+d.LeaseID+`"}}]`)
	set("/containers/"+podmanID+"/stats?stream=false&one-shot=true", cpuStats(1, 1))
	clock.t = clock.t.Add(5 * time.Second)
	book.Observe(read())
	// Bound, it holds back only what its container does not use yet.
	if ls := book.List(); len(ls) != 1 || ls[0].ID != d.LeaseID || ls[0].Bytes != gib-1<<20 {
		t.Fatalf("leases = %+v, want %s bound to its container, which uses 1 MiB", ls, d.LeaseID)
	}
	for range 30 { // past the lease's timeout
		clock.t = clock.t.Add(5 * time.Second)
		book.Observe(read())
	}
	if u := book.Ungated(); len(u) != 0 || strings.Contains(log.String(), "never appeared") {
		t.Fatalf("ungated = %+v, log:\n%s", u, log.String())
	}
}
