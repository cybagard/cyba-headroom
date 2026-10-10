package docker_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/source/docker"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// fakeVMs returns a fixed VM list.
type fakeVMs []vmproc.VM

func (f fakeVMs) List(context.Context) ([]vmproc.VM, error) { return f, nil }

// brokenVMs fails to list VMs.
type brokenVMs struct{}

func (brokenVMs) List(context.Context) ([]vmproc.VM, error) {
	return nil, errors.New("kern.proc.all: operation not permitted")
}

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
			msg := `{"message":"no route"}`
			if strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json") {
				msg = `{"message":"No such container: x"}` // as Docker answers
			}
			http.Error(w, msg, http.StatusNotFound)
			return
		}
		if code, ok := strings.CutPrefix(body, "!"); ok { // "!409": reply with that status
			status, _ := strconv.Atoi(code)
			http.Error(w, `{"message":"container is marked for removal"}`, status)
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
	d := collect(t, docker.New(sock, fakeVMs{}))
	if d.Running || d.Containers == nil || len(d.Containers) != 0 {
		t.Fatalf("got %+v, want not running with an empty (not null) container list", d)
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
	d := collect(t, docker.New(runningEngine(t), vms))

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
	d := collect(t, docker.New(runningEngine(t), fakeVMs{}))
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
	d := collect(t, docker.New(sock, fakeVMs{}))
	if got := d.Containers[0].Mounts; len(got) != 1 || got[0] != "/Users/dev/wt/fix-login" {
		t.Fatalf("mounts = %v, want the bind's host path only", got)
	}

	// Docker Desktop records the path the user gave in a label; prefer it.
	d = collect(t, docker.New(runningEngine(t), fakeVMs{}))
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
	src := docker.New(sock, fakeVMs{})

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
	d := collect(t, docker.New(sock, fakeVMs{}))
	if len(d.Containers) != 1 || d.Containers[0].ID != fixtureID {
		t.Fatalf("containers = %+v, want only the live one", d.Containers)
	}
}

func TestUnsetSocketIsAnError(t *testing.T) {
	_, err := docker.New("", fakeVMs{}).Collect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "[docker] socket") {
		t.Fatalf("err = %v, want a hint to set [docker] socket", err)
	}
}

func TestAPIFailureIsAnError(t *testing.T) {
	sock, _ := engine(t, map[string]string{"/info": "info.json"}) // /containers/json 404s
	if _, err := docker.New(sock, fakeVMs{}).Collect(context.Background()); err == nil {
		t.Fatal("want error")
	}
}

func TestContainerStatsErrorSkipsOnlyThatContainer(t *testing.T) {
	list, err := os.ReadFile("testdata/containers.json")
	if err != nil {
		t.Fatal(err)
	}
	withDying := `[{"Id":"dying1","Names":["/dying"],"Image":"alpine"},` + strings.TrimPrefix(strings.TrimSpace(string(list)), "[")
	sock, _ := engine(t, map[string]string{
		"/info":            "info.json",
		"/containers/json": withDying,
		"/containers/" + fixtureID + "/stats?stream=false&one-shot=true": "stats.json",
		"/containers/dying1/stats?stream=false&one-shot=true":            "!409",
	})
	d := collect(t, docker.New(sock, fakeVMs{}))
	if len(d.Containers) != 1 || d.Containers[0].ID != fixtureID {
		t.Fatalf("containers = %+v, want the healthy one", d.Containers)
	}
}

func TestVMListingFailureKeepsContainers(t *testing.T) {
	d := collect(t, docker.New(runningEngine(t), brokenVMs{}))
	if len(d.Containers) != 1 || d.VMRunning || d.VMError == "" {
		t.Fatalf("got %+v, want containers plus a VM error", d)
	}
}

func TestStatsFetchedConcurrently(t *testing.T) {
	const n = 10
	var inFlight, peak atomic.Int32
	sock := filepath.Join(shortDir(t), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var list []string
	for i := range n {
		list = append(list, fmt.Sprintf(`{"Id":"c%d","Names":["/c%d"],"Image":"alpine"}`, i, i))
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/info":
			_, _ = w.Write([]byte(`{"MemTotal":1}`))
		case "/containers/json":
			_, _ = w.Write([]byte("[" + strings.Join(list, ",") + "]"))
		default: // stats: slow, as under memory pressure
			cur := inFlight.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
			inFlight.Add(-1)
			_, _ = w.Write([]byte(cpuStats(1, 1)))
		}
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	d := collect(t, docker.New(sock, fakeVMs{}))
	if len(d.Containers) != n {
		t.Fatalf("got %d containers, want %d", len(d.Containers), n)
	}
	if peak.Load() < 2 {
		t.Fatalf("stats requests were sequential (peak %d in flight)", peak.Load())
	}
}

func TestInspectResolvesAContainer(t *testing.T) {
	sock, _ := engine(t, map[string]string{
		"/containers/db/json": `{"Id":"abc123","Config":{"Labels":{"dev.headroom.lease":"lease-1-2"}},"State":{"Running":true}}`,
	})
	s := docker.New(sock, nil)
	id, labels, running, err := s.Inspect(context.Background(), "db")
	if err != nil || id != "abc123" || labels["dev.headroom.lease"] != "lease-1-2" || !running {
		t.Fatalf("Inspect = %q, %v, %v, %v", id, labels, running, err)
	}
	if _, _, _, err := s.Inspect(context.Background(), "gone"); err == nil {
		t.Fatal("want an error for no such container")
	}
	for _, ref := range []string{"", "../images/json", "db?x=1", "a/b"} {
		if _, _, _, err := s.Inspect(context.Background(), ref); err == nil {
			t.Errorf("Inspect(%q): want an error, not a request", ref)
		}
	}
}

func TestEventsStreamsContainerStartsAndExits(t *testing.T) {
	sock, _ := engine(t, map[string]string{
		docker.EventsPath: `{"Type":"container","Action":"start","Actor":{"ID":"abc","Attributes":{"name":"quick","dev.headroom.lease":"lease-1-2"}},"time":1700000000,"timeNano":1700000000000000001}
{"Type":"container","Action":"die","Actor":{"ID":"abc","Attributes":{"name":"quick","exitCode":"0"}},"time":1700000002}
{"Type":"container","Action":"die","Actor":{"ID":"def","Attributes":{"name":"old"}}}
`,
	})
	var got []string
	err := docker.New(sock, nil).Events(context.Background(), 0, func(action, id string, timeNano int64, attrs map[string]string) {
		got = append(got, fmt.Sprint(action, " ", id, " ", attrs["name"], " ", attrs["dev.headroom.lease"], " ", timeNano))
	})
	// A first stream asks for no since; an event's time is its timeNano,
	// else its time in seconds, else 0 (#146).
	want := []string{"start abc quick lease-1-2 1700000000000000001", "die abc quick  1700000002000000000", "die def old  0"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if err == nil {
		t.Fatal("a stream that ends must say so: the caller reconnects")
	}
}

// A reconnect asks for the events since a time, as Docker reads it:
// seconds, and nine digits of nanoseconds (#146).
func TestEventsAskForThoseSinceATime(t *testing.T) {
	sock, _ := engine(t, map[string]string{
		docker.EventsPath + "&since=1700000000.050000007": `{"Type":"container","Action":"start","Actor":{"ID":"abc"},"timeNano":1700000000050000007}
`,
	})
	var got []string
	_ = docker.New(sock, nil).Events(context.Background(), 1700000000050000007, func(action, id string, _ int64, _ map[string]string) {
		got = append(got, action+" "+id)
	})
	if fmt.Sprint(got) != "[start abc]" {
		t.Fatalf("events = %q, want the stream since 1700000000.050000007", got)
	}
}

// Docker refusing the since is ErrBadSince, for the caller to ask again
// without it; a refusal of a stream with no since is not (#146).
func TestEventsSinceRefusedIsErrBadSince(t *testing.T) {
	sock, _ := engine(t, map[string]string{
		docker.EventsPath + "&since=1700000000.000000000": "!400",
		docker.EventsPath: "!400",
	})
	src := docker.New(sock, nil)
	noop := func(string, string, int64, map[string]string) {}
	if err := src.Events(context.Background(), 1700000000000000000, noop); !errors.Is(err, docker.ErrBadSince) {
		t.Fatalf("err = %v, want ErrBadSince", err)
	}
	if err := src.Events(context.Background(), 0, noop); err == nil || errors.Is(err, docker.ErrBadSince) {
		t.Fatalf("err = %v, want an error that is not ErrBadSince", err)
	}
}

// Docker's word that no such container exists is told apart from a
// failed lookup: a start of it starts nothing.
func TestInspectSaysNoSuchContainer(t *testing.T) {
	sock, _ := engine(t, map[string]string{})
	s := docker.New(sock, nil)
	if _, _, _, err := s.Inspect(context.Background(), "gone"); !errors.Is(err, docker.ErrNoSuchContainer) {
		t.Errorf("Inspect(gone) = %v, want ErrNoSuchContainer", err)
	}
	// Only a well-formed name or ID is known missing: anything Docker
	// might read otherwise is unknown, and costs.
	for _, ref := range []string{"x%", "a/b", "/db", "a b", "-x"} {
		if _, _, _, err := s.Inspect(context.Background(), ref); err == nil || errors.Is(err, docker.ErrNoSuchContainer) {
			t.Errorf("Inspect(%q) = %v, want an error that is not ErrNoSuchContainer", ref, err)
		}
	}
	if _, _, _, err := docker.New(filepath.Join(shortDir(t), "none.sock"), nil).Inspect(context.Background(), "db"); errors.Is(err, docker.ErrNoSuchContainer) {
		t.Fatal("no engine is not no such container")
	}
}

// A 404 that is not Docker's "No such container" (a proxy's) is unknown.
func TestInspectTrustsOnlyDockersNotFound(t *testing.T) {
	sock, _ := engine(t, map[string]string{"/containers/db/json": "!404"})
	if _, _, _, err := docker.New(sock, nil).Inspect(context.Background(), "db"); err == nil || errors.Is(err, docker.ErrNoSuchContainer) {
		t.Fatalf("err = %v", err)
	}
}
