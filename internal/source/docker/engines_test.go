package docker_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/source/docker"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// engines collects once from a source that finds the given sockets.
func engines(t *testing.T, vms vmproc.Lister, sockets ...string) []protocol.Engine {
	t.Helper()
	src := docker.Discover(func() []string { return sockets }, vms)
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
	src := docker.Discover(func() []string { return []string{a, b} }, fakeVMs{})
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
