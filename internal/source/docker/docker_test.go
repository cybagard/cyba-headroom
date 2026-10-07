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
	"sync"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/source/docker"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// fakeVMs returns a fixed VM list.
type fakeVMs []vmproc.VM

func (f fakeVMs) List(context.Context) ([]vmproc.VM, error) { return f, nil }

// shortDir is a temp dir short enough for a Unix socket path on macOS.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// engine serves the Docker Engine API on a Unix socket. routes maps a request
// path (with query) to a testdata file or inline JSON. It returns the socket
// and a function to change a route between collections.
func engine(t *testing.T, routes map[string]string) (string, func(path, body string)) {
	t.Helper()
	sock := filepath.Join(shortDir(t), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		body, ok := routes[r.URL.RequestURI()]
		mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"no route"}`, http.StatusNotFound)
			return
		}
		if !strings.HasPrefix(body, "{") && !strings.HasPrefix(body, "[") {
			b, err := os.ReadFile("testdata/" + body)
			if err != nil {
				t.Error(err)
			}
			body = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return sock, func(path, body string) {
		mu.Lock()
		defer mu.Unlock()
		routes[path] = body
	}
}

func collect(t *testing.T, src *docker.Source) protocol.Docker {
	t.Helper()
	r, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var snap protocol.Snapshot
	r.Apply(&snap)
	if snap.Docker == nil {
		t.Fatal("reading did not fill Snapshot.Docker")
	}
	return *snap.Docker
}

func TestDockerNotRunningIsAReading(t *testing.T) {
	sock := filepath.Join(shortDir(t), "docker.sock") // nothing listens here
	d := collect(t, docker.New(sock, fakeVMs{}, time.Now))
	if d.Running || len(d.Containers) != 0 {
		t.Fatalf("got %+v, want not running", d)
	}
}

// fixtureID is the container in testdata/containers.json and stats.json.
const fixtureID = "585f8ebeefcc32175f161d005a37cf22651c0b9ee7e4d46496a19be28943e4e0"

func runningEngine(t *testing.T) string {
	sock, _ := engine(t, map[string]string{
		"/info":            "info.json",
		"/containers/json": "containers.json",
		"/containers/" + fixtureID + "/stats?stream=false&one-shot=true": "stats.json",
	})
	return sock
}

func TestReportsContainersAndVMCost(t *testing.T) {
	vms := fakeVMs{
		{PID: 1, Kind: vmproc.Tart, Name: "macos-a", FootprintBytes: 4 << 30},
		{PID: 2, Kind: vmproc.Docker, FootprintBytes: 1709280520},
	}
	d := collect(t, docker.New(runningEngine(t), vms, time.Now))

	if !d.Running || d.VMLimitBytes != 33333952512 || !d.VMRunning || d.VMFootprintBytes != 1709280520 {
		t.Fatalf("got running=%v limit=%d vm=%v footprint=%d", d.Running, d.VMLimitBytes, d.VMRunning, d.VMFootprintBytes)
	}
	if len(d.Containers) != 1 {
		t.Fatalf("containers = %+v, want 1", d.Containers)
	}
	c := d.Containers[0]
	// memory = usage - inactive_file, what docker stats shows (spike #9).
	if c.ID != fixtureID || c.Name != "hr-fixture" || c.Image != "alpine" || c.MemoryBytes != 69210112 {
		t.Fatalf("container = %+v", c)
	}
	if c.Labels["com.docker.compose.project"] != "fix-login" {
		t.Fatalf("labels = %v", c.Labels)
	}
}

func TestDockerVMAsleepCostsNothing(t *testing.T) {
	// Resource Saver: the API answers but the VM process is gone.
	d := collect(t, docker.New(runningEngine(t), fakeVMs{}, time.Now))
	if !d.Running || d.VMRunning || d.VMFootprintBytes != 0 {
		t.Fatalf("got %+v, want running API with no VM cost", d)
	}
}

func TestBindMountsAreHostPaths(t *testing.T) {
	const plain = "plain1"
	sock, _ := engine(t, map[string]string{
		"/info": "info.json",
		// A container started without Docker Desktop's binds labels.
		"/containers/json": `[{"Id":"` + plain + `","Names":["/plain"],"Image":"alpine",
			"Mounts":[{"Type":"bind","Source":"/host_mnt/Users/dev/wt/fix-login"},{"Type":"volume","Source":"/var/lib/docker/volumes/x/_data"}]}]`,
		"/containers/" + plain + "/stats?stream=false&one-shot=true": "stats.json",
	})
	d := collect(t, docker.New(sock, fakeVMs{}, time.Now))
	if got := d.Containers[0].Mounts; len(got) != 1 || got[0] != "/Users/dev/wt/fix-login" {
		t.Fatalf("mounts = %v, want the bind's host path only", got)
	}

	// Docker Desktop records the path the user gave in a label; prefer it.
	d = collect(t, docker.New(runningEngine(t), fakeVMs{}, time.Now))
	if got := d.Containers[0].Mounts; len(got) != 1 || got[0] != "/tmp/hr-fixture" {
		t.Fatalf("mounts = %v, want [/tmp/hr-fixture]", got)
	}
}

// cpuStats is a one-shot stats body with the given cumulative CPU counters (ns).
func cpuStats(container, system uint64) string {
	return fmt.Sprintf(`{"memory_stats":{"usage":1048576,"stats":{"inactive_file":0}},
		"cpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d,"online_cpus":16}}`, container, system)
}

func TestCPUPercentFromConsecutiveTicks(t *testing.T) {
	stats := "/containers/" + fixtureID + "/stats?stream=false&one-shot=true"
	sock, set := engine(t, map[string]string{
		"/info":            "info.json",
		"/containers/json": "containers.json",
		stats:              cpuStats(5e9, 1000e9),
	})
	src := docker.New(sock, fakeVMs{}, time.Now)

	if c := collect(t, src).Containers[0]; c.CPUPercent != nil {
		t.Fatalf("first tick cpu = %v, want none", *c.CPUPercent)
	}
	// 2 s of container CPU over 8 s of host CPU on 16 cores: a quarter of
	// the machine, 400% in docker stats terms (100% = one core).
	set(stats, cpuStats(7e9, 1008e9))
	c := collect(t, src).Containers[0]
	if c.CPUPercent == nil || *c.CPUPercent != 400 {
		t.Fatalf("cpu = %v, want 400", c.CPUPercent)
	}
}

func TestContainerGoneBeforeStatsIsSkipped(t *testing.T) {
	list, err := os.ReadFile("testdata/containers.json")
	if err != nil {
		t.Fatal(err)
	}
	// Prepend a container whose stats 404: it exited after the list call.
	withGone := `[{"Id":"gone1","Names":["/gone"],"Image":"alpine"},` + strings.TrimPrefix(strings.TrimSpace(string(list)), "[")
	sock, _ := engine(t, map[string]string{
		"/info":            "info.json",
		"/containers/json": withGone,
		"/containers/" + fixtureID + "/stats?stream=false&one-shot=true": "stats.json",
	})
	d := collect(t, docker.New(sock, fakeVMs{}, time.Now))
	if len(d.Containers) != 1 || d.Containers[0].ID != fixtureID {
		t.Fatalf("containers = %+v, want only the live one", d.Containers)
	}
}

func TestAPIFailureIsAnError(t *testing.T) {
	sock, _ := engine(t, map[string]string{"/info": "info.json"}) // /containers/json 404s
	if _, err := docker.New(sock, fakeVMs{}, time.Now).Collect(context.Background()); err == nil {
		t.Fatal("want error")
	}
}
