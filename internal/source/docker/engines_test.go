package docker_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/source/docker"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// engines collects once from a source that finds the given trusted sockets.
func engines(t *testing.T, vms vmproc.Lister, sockets ...string) []protocol.Engine {
	t.Helper()
	var cands []docker.Candidate
	for _, s := range sockets {
		cands = append(cands, docker.Candidate{Socket: s, Trusted: true})
	}
	return found(t, vms, cands...)
}

// found collects once from a source that finds the given candidates.
func found(t *testing.T, vms vmproc.Lister, cands ...docker.Candidate) []protocol.Engine {
	t.Helper()
	src := docker.Discover(func() []docker.Candidate { return cands }, vms)
	r, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var snap protocol.Snapshot
	r.Apply(&snap)
	if snap.Engines == nil {
		t.Fatal("reading did not fill Snapshot.Engines")
	}
	return snap.Engines
}

// idle serves an engine with the given /info ID and /version, running
// nothing.
func idle(t *testing.T, id, version string) string {
	sock, _ := engine(t, map[string]string{
		"/info":            `{"ID":"` + id + `","MemTotal":2037100544}`,
		"/version":         version,
		"/containers/json": `[]`,
	})
	return sock
}

// Two sockets that reach one engine (one /info ID) are one engine, by the
// first socket.
func TestSocketsOfOneEngineAreOne(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000001"
	a, b := idle(t, id, "version-desktop.json"), idle(t, id, "version-desktop.json")
	got := engines(t, fakeVMs{}, a, b)
	if len(got) != 1 || got[0].Socket != a || got[0].ID != id || !got[0].Running {
		t.Fatalf("engines = %+v, want one, at %s", got, a)
	}
}

// Each engine's kind is read from its /version.
func TestKindFromVersion(t *testing.T) {
	for file, want := range map[string]string{
		"version-desktop.json": protocol.EngineDockerDesktop,
		"version-docker.json":  protocol.EngineDocker,
		"version-podman.json":  protocol.EnginePodman,
	} {
		got := engines(t, fakeVMs{}, idle(t, "00000000-0000-4000-8000-000000000001", file))
		if len(got) != 1 || got[0].Kind != want {
			t.Errorf("%s: engines = %+v, want kind %s", file, got, want)
		}
	}
}

// Two engines each report their own containers; Docker Desktop's VM is
// Docker Desktop's alone, and another engine's VM is unknown.
func TestTwoEnginesEachWithTheirContainers(t *testing.T) {
	desktop, _ := engine(t, map[string]string{
		"/info":            `{"ID":"00000000-0000-4000-8000-000000000001","MemTotal":33333952512}`,
		"/version":         "version-desktop.json",
		"/containers/json": "containers.json",
		"/containers/" + fixtureID + "/stats?stream=false&one-shot=true": "stats.json",
	})
	colima, _ := engine(t, map[string]string{
		"/info":            `{"ID":"00000000-0000-4000-8000-000000000002","MemTotal":2037100544}`,
		"/version":         "version-docker.json",
		"/containers/json": `[{"Id":"c2","Names":["/db"],"Image":"postgres"}]`,
		"/containers/c2/stats?stream=false&one-shot=true": cpuStats(1, 1),
	})
	vms := fakeVMs{{PID: 2, Kind: vmproc.Docker, FootprintBytes: 1709280520}}
	got := engines(t, vms, desktop, colima)
	if len(got) != 2 {
		t.Fatalf("engines = %+v, want two", got)
	}
	d, c := got[0], got[1]
	if d.Kind != protocol.EngineDockerDesktop || d.Socket != desktop || len(d.Containers) != 1 || d.Containers[0].ID != fixtureID ||
		!d.VMRunning || d.VMFootprintBytes != 1709280520 || d.VMLimitBytes != 33333952512 {
		t.Errorf("desktop = %+v", d)
	}
	if c.Kind != protocol.EngineDocker || c.Socket != colima || len(c.Containers) != 1 || c.Containers[0].Name != "db" ||
		c.VMLimitBytes != 2037100544 || c.VMFootprintBytes != 0 || c.VMError == "" {
		t.Errorf("colima = %+v, want its own container and its VM unknown", c)
	}
}

// A socket that is there but refuses (Docker Desktop quit) is an engine
// that is not running; no socket at all is no engine.
func TestRefusingSocketIsAnEngineNotRunning(t *testing.T) {
	sock := filepath.Join(shortDir(t), "docker.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	_ = ln.Close() // the file stays; connecting to it is refused
	if _, err := os.Stat(sock); err != nil {
		t.Fatal(err)
	}
	got := engines(t, fakeVMs{}, sock)
	if len(got) != 1 || got[0].Running || got[0].Socket != sock || got[0].Containers == nil {
		t.Fatalf("engines = %+v, want one not running, with an empty (not null) container list", got)
	}
	if got := engines(t, fakeVMs{}); len(got) != 0 {
		t.Fatalf("engines = %+v, want none", got)
	}
}

// CPU is computed per engine: one engine's container is not another's.
func TestCPUPerEngine(t *testing.T) {
	stats := "/containers/" + fixtureID + "/stats?stream=false&one-shot=true"
	route := func(id string) map[string]string {
		return map[string]string{
			"/info":            `{"ID":"` + id + `","MemTotal":1}`,
			"/version":         "version-docker.json",
			"/containers/json": "containers.json",
			stats:              cpuStats(5e9, 1000e9),
		}
	}
	a, setA := engine(t, route("00000000-0000-4000-8000-000000000001"))
	b, _ := engine(t, route("00000000-0000-4000-8000-000000000002"))
	src := docker.Discover(func() []docker.Candidate {
		return []docker.Candidate{{Socket: a, Trusted: true}, {Socket: b, Trusted: true}}
	}, fakeVMs{})
	collectOnce := func() []protocol.Engine {
		r, err := src.Collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var s protocol.Snapshot
		r.Apply(&s)
		return s.Engines
	}
	collectOnce()
	setA(stats, cpuStats(7e9, 1008e9))
	got := collectOnce()
	pa, pb := got[0].Containers[0].CPUPercent, got[1].Containers[0].CPUPercent
	if pa == nil || *pa != 400 || pb == nil || *pb != 0 {
		t.Fatalf("cpu = %v, %v, want 400 and 0", fmt.Sprint(pa), fmt.Sprint(pb))
	}
}

// collectWithin collects once from cands, as the daemon does, bounded by
// its default source timeout.
func collectWithin(t *testing.T, cands ...docker.Candidate) ([]protocol.Engine, time.Duration, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	r, err := docker.Discover(func() []docker.Candidate { return cands }, fakeVMs{}).Collect(ctx)
	took := time.Since(start)
	if err != nil {
		return nil, took, err
	}
	var s protocol.Snapshot
	r.Apply(&s)
	return s.Engines, took, nil
}

// A context's socket that answers, but not as Docker does, is an engine not
// running; the engines after it are read as ever (#227, amendment 3).
func TestAContextThatIsNotDockerIsNotRunning(t *testing.T) {
	other, _ := engine(t, map[string]string{"/info": `{"hello":"world"}`})
	got, _, err := collectWithin(t, docker.Candidate{Socket: other}, docker.Candidate{Socket: runningEngine(t), Trusted: true})
	if err != nil {
		t.Fatalf("Collect: %v, want a reading", err)
	}
	if len(got) != 2 || got[0].Socket != other || got[0].Running || !got[1].Running || len(got[1].Containers) != 1 {
		t.Fatalf("engines = %+v, want the context's not running, then the engine with its container", got)
	}
}

// mute serves a socket that accepts and never answers.
func mute(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(shortDir(t), "mute.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		var held []net.Conn
		for {
			c, err := ln.Accept()
			if err != nil {
				for _, c := range held {
					_ = c.Close()
				}
				return
			}
			held = append(held, c) // read nothing, answer nothing
		}
	}()
	return sock
}

// A context's socket that accepts and never answers holds the reading up
// for a second at most, well within the source timeout.
func TestAContextThatNeverAnswersIsNotRunning(t *testing.T) {
	got, took, err := collectWithin(t, docker.Candidate{Socket: mute(t)}, docker.Candidate{Socket: runningEngine(t), Trusted: true})
	if err != nil {
		t.Fatalf("Collect: %v after %v, want a reading", err, took)
	}
	if took > 2*time.Second || len(got) != 2 || got[0].Running || !got[1].Running {
		t.Fatalf("engines = %+v after %v, want the context's not running, quickly", got, took)
	}
}

// Contexts share one second, however many there are and however many
// calls each needs (#227, amendment 4): three mute ones before Docker
// Desktop still leave a reading well inside the source timeout.
func TestManyMuteContextsShareOneSecond(t *testing.T) {
	got, took, err := collectWithin(t, docker.Candidate{Socket: mute(t)}, docker.Candidate{Socket: mute(t)},
		docker.Candidate{Socket: mute(t)}, docker.Candidate{Socket: runningEngine(t), Trusted: true})
	if err != nil {
		t.Fatalf("Collect: %v after %v, want a reading", err, took)
	}
	if took > 2*time.Second || len(got) != 4 || !got[3].Running || got[0].Running || got[1].Running || got[2].Running {
		t.Fatalf("engines = %+v after %v, want three not running and Docker Desktop's, quickly", got, took)
	}
}

// A context whose engine answers each call just under a second, with
// containers, is not running once the contexts' second is spent; Docker
// Desktop's engine is read in full, well inside the source timeout.
func TestASlowContextIsNotRunning(t *testing.T) {
	slow := filepath.Join(shortDir(t), "slow.sock")
	ln, err := net.Listen("unix", slow)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(900 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		switch r.URL.Path {
		case "/info":
			_, _ = w.Write([]byte(`{"ID":"00000000-0000-4000-8000-000000000009","MemTotal":1}`))
		case "/version":
			_, _ = w.Write([]byte(`{"Platform":{"Name":"x"}}`))
		case "/containers/json":
			_, _ = w.Write([]byte(`[{"Id":"s1","Names":["/a"]},{"Id":"s2","Names":["/b"]}]`))
		default:
			_, _ = w.Write([]byte(`{"memory_stats":{"usage":1}}`))
		}
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	desktop := runningEngine(t)
	got, took, err := collectWithin(t, docker.Candidate{Socket: desktop, Trusted: true}, docker.Candidate{Socket: slow})
	if err != nil {
		t.Fatalf("Collect: %v after %v, want a reading", err, took)
	}
	if took > 2*time.Second || len(got) != 2 || got[0].Socket != desktop || !got[0].Running || len(got[0].Containers) != 1 ||
		got[1].Socket != slow || got[1].Running || got[1].Error == "" {
		t.Fatalf("engines = %+v after %v, want Docker Desktop's and the slow context's not running, with its error, quickly", got, took)
	}
}

// A context that fails says why, beside not running.
func TestAFailedContextCarriesItsError(t *testing.T) {
	other, _ := engine(t, map[string]string{"/info": `{"hello":"world"}`})
	got, _, err := collectWithin(t, docker.Candidate{Socket: other})
	if err != nil || len(got) != 1 || got[0].Running || !strings.Contains(got[0].Error, "/containers/json") {
		t.Fatalf("engines = %+v, err = %v: want one not running, with its error", got, err)
	}
	// A trusted socket that is only not there, or refuses, is not running,
	// with no error: nothing failed.
	got = engines(t, fakeVMs{}, filepath.Join(shortDir(t), "none.sock"))
	if len(got) != 1 || got[0].Running || got[0].Error != "" {
		t.Fatalf("engines = %+v, want one not running, with no error", got)
	}
}

// A client is cached only while its socket is a candidate: one a context
// named once, and no more, is dropped after the reading and its idle
// connection closed. An agent can rotate a context's socket name each
// tick (a new hard link to one socket), which would else leak a client
// and a connection per tick for the daemon's lifetime (#236).
func TestADroppedCandidatesClientIsClosed(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string]string{
		"/info":            `{"ID":"00000000-0000-4000-8000-000000000001","MemTotal":2037100544}`,
		"/version":         `{"Platform":{"Name":"Docker Engine - Community"}}`,
		"/containers/json": `[]`,
	}
	var open atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(routes[r.URL.Path]))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	var tick int
	src := docker.Discover(func() []docker.Candidate {
		name := filepath.Join(dir, fmt.Sprintf("%02d.sock", tick))
		if err := os.Link(sock, name); err != nil {
			t.Fatal(err)
		}
		return []docker.Candidate{{Socket: name}}
	}, fakeVMs{})
	for tick = range 50 {
		r, err := src.Collect(context.Background())
		if err != nil {
			t.Fatalf("tick %d: Collect: %v", tick, err)
		}
		var snap protocol.Snapshot
		r.Apply(&snap)
		if len(snap.Engines) != 1 || !snap.Engines[0].Running {
			t.Fatalf("tick %d: engines = %+v, want the one running", tick, snap.Engines)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for open.Load() > 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := open.Load(); n > 1 {
		t.Fatalf("%d connections open after 50 readings of a new name each, want at most 1: the current candidate's", n)
	}
}
