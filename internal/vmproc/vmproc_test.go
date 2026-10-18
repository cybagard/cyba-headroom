package vmproc_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// fakeSystem lists fixed PIDs and answers lsof, footprint and ps from
// captured output. A ps without an answer fails; it removes no process.
type fakeSystem struct {
	pids  []int
	files map[string]string // "lsof <pid>", "footprint <pid>" or "ps <pid,…>" -> testdata file
	// outputs, checked before files, are outputs built by the test.
	outputs map[string][]byte
	calls   map[string]int
	// toolBroken makes a missing file a tool failure rather than an exit.
	toolBroken bool
}

func (f *fakeSystem) VMPIDs() ([]int, error) { return f.pids, nil }

func (f *fakeSystem) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	var pid string
	for i, a := range args {
		if a == "-p" && i+1 < len(args) {
			pid = args[i+1]
		}
	}
	key := path.Base(name) + " " + pid
	f.calls[key]++
	if out, ok := f.outputs[key]; ok {
		return out, nil
	}
	file, ok := f.files[key]
	if !ok {
		if f.toolBroken || name == "/bin/ps" {
			return nil, errors.New("exit status 1") // tool failed, process still there
		}
		f.pids = slices.DeleteFunc(f.pids, func(p int) bool { return strconv.Itoa(p) == pid })
		return nil, errors.New("exit status 1") // process gone
	}
	return os.ReadFile("testdata/" + file)
}

func newFake() *fakeSystem {
	return &fakeSystem{
		pids: []int{1234, 5678},
		files: map[string]string{
			"lsof 1234":      "lsof-docker.txt",
			"lsof 5678":      "lsof-tart.txt",
			"footprint 1234": "footprint.txt",
			"footprint 5678": "footprint.txt",
		},
		calls: map[string]int{},
	}
}

func TestListClassifiesVMsAndReadsFootprint(t *testing.T) {
	got, err := vmproc.New(newFake()).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []vmproc.VM{
		{PID: 1234, Kind: vmproc.Docker, FootprintBytes: 1709280520},
		{PID: 5678, Kind: vmproc.Tart, Name: "headroom-mac", FootprintBytes: 1709280520},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestClassifiesEachProcessOnce(t *testing.T) {
	sys := newFake()
	f := vmproc.New(sys)
	for range 3 {
		if _, err := f.List(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if sys.calls["lsof 1234"] != 1 || sys.calls["footprint 1234"] != 3 {
		t.Fatalf("calls = %v, want lsof once and footprint every list", sys.calls)
	}
}

func TestReclassifiesAReusedPID(t *testing.T) {
	sys := newFake()
	f := vmproc.New(sys)
	if _, err := f.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The Docker VM exits (Resource Saver); its PID is later reused by a Tart VM.
	sys.pids = []int{5678}
	if _, err := f.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	sys.pids = []int{1234, 5678}
	sys.files["lsof 1234"] = "lsof-tart.txt"
	got, err := f.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Kind != vmproc.Tart {
		t.Fatalf("reused PID 1234 still classified as %s", got[0].Kind)
	}
}

func TestSkipsProcessThatExitsWhileRead(t *testing.T) {
	sys := newFake()
	delete(sys.files, "footprint 5678")
	got, err := vmproc.New(sys).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PID != 1234 {
		t.Fatalf("got %+v, want only the Docker VM", got)
	}
}

func TestUnrecognisedVMIsUnknown(t *testing.T) {
	sys := newFake()
	sys.pids = []int{42}
	sys.files["lsof 42"] = "lsof-other.txt"
	sys.files["footprint 42"] = "footprint.txt"
	got, err := vmproc.New(sys).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != vmproc.Unknown {
		t.Fatalf("got %+v, want one unknown VM", got)
	}
}

func TestFootprintFailureForLiveVMIsAnError(t *testing.T) {
	sys := newFake()
	sys.toolBroken = true
	delete(sys.files, "footprint 1234")
	// Dropping the VM would report Docker's VM as not running while it holds GBs.
	if _, err := vmproc.New(sys).List(context.Background()); err == nil {
		t.Fatal("want error when footprint fails for a VM that is still running")
	}
}

func TestUnknownIsRetriedOnTheNextList(t *testing.T) {
	sys := newFake()
	sys.pids = []int{1234}
	sys.files["lsof 1234"] = "lsof-other.txt" // VM still booting: no image open yet
	f := vmproc.New(sys)
	if _, err := f.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	sys.files["lsof 1234"] = "lsof-docker.txt"
	got, err := f.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Kind != vmproc.Docker {
		t.Fatalf("kind = %s, want docker once the VM has its image open", got[0].Kind)
	}
}

func TestPodmanVMIsNamedByItsMachine(t *testing.T) {
	sys := newFake()
	sys.pids = []int{1234, 5678, 9012}
	sys.files["lsof 9012"] = "lsof-podman.txt"
	sys.files["footprint 9012"] = "footprint.txt"
	got, err := vmproc.New(sys).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The EFI store is open before the disk; the name comes from the disk.
	want := []vmproc.VM{
		{PID: 1234, Kind: vmproc.Docker, FootprintBytes: 1709280520},
		{PID: 5678, Kind: vmproc.Tart, Name: "headroom-mac", FootprintBytes: 1709280520},
		{PID: 9012, Kind: vmproc.Podman, Name: "podman-machine-default", FootprintBytes: 1709280520},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestUnknownVMIsNamedByItsCommand(t *testing.T) {
	sys := newFake()
	sys.pids = []int{1234, 5678, 4242}
	// lsof-unknown.txt is the recorded lsof-tart.txt capture with the Tart
	// VM's files moved to a folder no owner claims.
	sys.files["lsof 4242"] = "lsof-unknown.txt"
	sys.files["footprint 4242"] = "footprint.txt"
	got, err := vmproc.New(sys).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []vmproc.VM{
		{PID: 1234, Kind: vmproc.Docker, FootprintBytes: 1709280520},
		{PID: 5678, Kind: vmproc.Tart, Name: "headroom-mac", FootprintBytes: 1709280520},
		{PID: 4242, Kind: vmproc.Unknown, Name: "com.apple.Virtualization.VirtualMachine", FootprintBytes: 1709280520},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestTartVMUnderTartHomeIsTart(t *testing.T) {
	sys := newFake()
	sys.pids = []int{4343}
	// lsof-tart-home.txt is the recorded lsof-tart.txt capture with the Tart
	// VM's files moved under a TART_HOME.
	sys.files["lsof 4343"] = "lsof-tart-home.txt"
	sys.files["footprint 4343"] = "footprint.txt"
	got, err := vmproc.New(sys).WithTartHome("/Users/dev/tart-home").List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Unknown would also reserve it in other_vms, beside the tart source's.
	want := []vmproc.VM{{PID: 4343, Kind: vmproc.Tart, Name: "vm-a", FootprintBytes: 1709280520}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestTartVMUnderASymlinkedTartHomeIsTart(t *testing.T) {
	// lsof prints resolved paths: a TART_HOME under /tmp on macOS is
	// /private/tmp in its output.
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(target, "vms", "vm-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := os.ReadFile("testdata/lsof-tart-home.txt")
	if err != nil {
		t.Fatal(err)
	}
	sys := newFake()
	sys.pids = []int{4343}
	sys.outputs = map[string][]byte{"lsof 4343": bytes.ReplaceAll(capture, []byte("/Users/dev/tart-home"), []byte(resolved))}
	sys.files["footprint 4343"] = "footprint.txt"
	got, err := vmproc.New(sys).WithTartHome(link).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []vmproc.VM{{PID: 4343, Kind: vmproc.Tart, Name: "vm-a", FootprintBytes: 1709280520}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestTartVMRunFromTheOCICacheIsUnknown(t *testing.T) {
	sys := newFake()
	sys.pids = []int{4444}
	// lsof-tart-oci.txt is the recorded lsof-tart.txt capture with the Tart
	// VM's files moved into the OCI cache, at the digest folder a tag's
	// symlink resolves to.
	sys.files["lsof 4444"] = "lsof-tart-oci.txt"
	sys.files["footprint 4444"] = "footprint.txt"
	got, err := vmproc.New(sys).WithTartHome("/Users/dev/.tart").List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Tart never runs a VM from the cache; one that did would be counted by
	// no owner as Tart, so it is shown in other_vms.
	want := []vmproc.VM{{PID: 4444, Kind: vmproc.Unknown, Name: "com.apple.Virtualization.VirtualMachine", FootprintBytes: 1709280520}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestStackedTartVMIsNamedByItsVMFolder(t *testing.T) {
	capture, err := os.ReadFile("testdata/lsof-tart-home.txt")
	if err != nil {
		t.Fatal(err)
	}
	// A stacked VM (Tart 2.40) holds its base disk open in the shared content
	// store, here listed first, and its own overlay in vms/<name>/.
	capture = bytes.Replace(capture,
		[]byte("/Users/dev/tart-home/vms/vm-a/nvram.bin"),
		[]byte("/Users/dev/tart-home/cache/content/sha256/0000000000000000000000000000000000000000000000000000000000000001"), 1)
	capture = bytes.Replace(capture, []byte("vms/vm-a/disk.img"), []byte("vms/vm-a/overlay.asif"), 1)
	sys := newFake()
	sys.pids = []int{4343}
	sys.outputs = map[string][]byte{"lsof 4343": capture}
	sys.files["footprint 4343"] = "footprint.txt"
	got, err := vmproc.New(sys).WithTartHome("/Users/dev/tart-home").List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []vmproc.VM{{PID: 4343, Kind: vmproc.Tart, Name: "vm-a", FootprintBytes: 1709280520}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestTartVMUnderASymlinkedDefaultTartHomeIsTart(t *testing.T) {
	// ~/.tart moved to another disk and linked back; TART_HOME unset.
	dir := t.TempDir()
	target := filepath.Join(dir, "ext", "tart")
	if err := os.MkdirAll(filepath.Join(target, "vms", "vm-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ".tart")); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := os.ReadFile("testdata/lsof-tart-home.txt")
	if err != nil {
		t.Fatal(err)
	}
	sys := newFake()
	sys.pids = []int{4343}
	sys.outputs = map[string][]byte{"lsof 4343": bytes.ReplaceAll(capture, []byte("/Users/dev/tart-home"), []byte(resolved))}
	sys.files["footprint 4343"] = "footprint.txt"
	getenv := func(k string) string { return map[string]string{"HOME": dir}[k] }
	got, err := vmproc.New(sys).WithTartHome(vmproc.TartHome(getenv)).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []vmproc.VM{{PID: 4343, Kind: vmproc.Tart, Name: "vm-a", FootprintBytes: 1709280520}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// TestTartVMsAreTartWhereverTheirFilesLive lays out a Tart home on disk for
// each way Tart finds its VMs, symlinks included. lsof prints the resolved
// path of each open file, so the capture names the VM folder's resolved
// path, and the VM must still be Tart: else it is reserved again in
// other_vms beside the tart source's reservation (#226 B1–B4). The OCI
// cache rows are Unknown: Tart never runs a VM from the cache (#234).
func TestTartVMsAreTartWhereverTheirFilesLive(t *testing.T) {
	const oci = "cache/OCIs/registry.example/org-a/image-a"
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	cases := []struct {
		name string
		// dirs are made under the test's folder, then links (link -> target).
		dirs  []string
		links [][2]string
		// tartHome is TART_HOME, relative to the test's folder; empty for unset.
		tartHome string
		// vm is the VM folder as Tart reaches it.
		vm       string
		wantName string
		// unknown is for files Tart never runs a VM from: the VM is Unknown,
		// named by its command.
		unknown bool
	}{
		{name: "TART_HOME", dirs: []string{"th/vms/vm-a"}, tartHome: "th", vm: "th/vms/vm-a", wantName: "vm-a"},
		{name: "OCI cache", dirs: []string{".tart/" + oci + "/" + digest}, links: [][2]string{{".tart/" + oci + "/1", digest}}, vm: ".tart/" + oci + "/1", wantName: "com.apple.Virtualization.VirtualMachine", unknown: true},
		{name: "symlinked home", dirs: []string{"ext/tart/vms/vm-a"}, links: [][2]string{{".tart", "ext/tart"}}, vm: ".tart/vms/vm-a", wantName: "vm-a"},
		{name: "symlinked vms", dirs: []string{".tart", "ext/vms/vm-a"}, links: [][2]string{{".tart/vms", "../ext/vms"}}, vm: ".tart/vms/vm-a", wantName: "vm-a"},
		{name: "symlinked cache", dirs: []string{".tart", "ext/cache/OCIs/registry.example/org-a/image-a/" + digest}, links: [][2]string{{".tart/cache", "../ext/cache"}, {".tart/" + oci + "/1", digest}}, vm: ".tart/" + oci + "/1", wantName: "com.apple.Virtualization.VirtualMachine", unknown: true},
		{name: "symlinked VM folder", dirs: []string{".tart/vms", "ext/vm-a"}, links: [][2]string{{".tart/vms/vm-a", "../../ext/vm-a"}}, vm: ".tart/vms/vm-a", wantName: "vm-a"},
	}
	capture, err := os.ReadFile("testdata/lsof-tart-home.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, d := range c.dirs {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, l := range c.links {
				if err := os.Symlink(l[1], filepath.Join(dir, l[0])); err != nil {
					t.Fatal(err)
				}
			}
			resolved, err := filepath.EvalSymlinks(filepath.Join(dir, c.vm))
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{"HOME": dir}
			if c.tartHome != "" {
				env["TART_HOME"] = filepath.Join(dir, c.tartHome)
			}
			sys := newFake()
			sys.pids = []int{4343}
			sys.outputs = map[string][]byte{"lsof 4343": bytes.ReplaceAll(capture, []byte("/Users/dev/tart-home/vms/vm-a"), []byte(resolved))}
			sys.files["footprint 4343"] = "footprint.txt"
			getenv := func(k string) string { return env[k] }
			got, err := vmproc.New(sys).WithTartHome(vmproc.TartHome(getenv)).List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := []vmproc.VM{{PID: 4343, Kind: vmproc.Tart, Name: c.wantName, FootprintBytes: 1709280520}}
			if c.unknown {
				want[0].Kind = vmproc.Unknown
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got  %+v\nwant %+v", got, want)
			}
		})
	}
}

// stalledDirs is a Tart home on a stalled volume: ReadDir blocks until
// released.
type stalledDirs struct {
	release chan struct{}
	reads   atomic.Int32
}

func (d *stalledDirs) ReadDir(string) ([]fs.DirEntry, error) {
	d.reads.Add(1)
	<-d.release
	return nil, fs.ErrNotExist
}

func (d *stalledDirs) EvalSymlinks(p string) (string, error) { return p, nil }

func TestAStalledTartHomeDoesNotBlockList(t *testing.T) {
	sys := newFake()
	sys.pids = []int{4343}
	sys.files["lsof 4343"] = "lsof-tart-home.txt"
	sys.files["footprint 4343"] = "footprint.txt"
	d := &stalledDirs{release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(d.release) })
	t.Cleanup(release)
	f := vmproc.SetDirs(vmproc.New(sys).WithTartHome("/Users/dev/tart-home"), d)
	list := func(timeout time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := f.List(ctx)
			done <- err
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(time.Second):
			t.Fatal("List did not return by its deadline")
			return nil
		}
	}
	for i := range 2 {
		if err := list(50 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("list %d: err = %v, want the deadline", i+1, err)
		}
	}
	// The second List waited on the first one's scan rather than start
	// another: a stalled volume holds one goroutine, not one per tick.
	if n := d.reads.Load(); n != 1 {
		t.Fatalf("ReadDir called %d times, want once", n)
	}
	release()
	if err := list(time.Second); err != nil {
		t.Fatalf("after the volume recovers: %v", err)
	}
}

// clock is a fake clock for CPU readings.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// cpuFinder is a Finder over sys with a fake clock.
func cpuFinder(sys *fakeSystem) (*vmproc.Finder, *clock) {
	c := &clock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	return vmproc.SetNow(vmproc.New(sys), c.now), c
}

// cpus is each VM's CPU, nil when unknown, by PID.
func cpus(t *testing.T, f *vmproc.Finder) map[int]*float64 {
	t.Helper()
	vms, err := f.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]*float64{}
	for _, vm := range vms {
		got[vm.PID] = vm.CPUPercent
	}
	return got
}

// pct shows a CPU reading.
func pct(p *float64) string {
	if p == nil {
		return "unknown"
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

func near(p *float64, want float64) bool { return p != nil && *p > want-1e-6 && *p < want+1e-6 }

func TestCPUIsCPUTimeOverWallTime(t *testing.T) {
	sys := newFake()
	f, c := cpuFinder(sys)
	sys.outputs = map[string][]byte{"ps 1234,5678": []byte(" 1234   0:10.00\n 5678 250:43.69\n")}
	if got := cpus(t, f); got[1234] != nil || got[5678] != nil {
		t.Fatalf("first reading = %v %v, want unknown", pct(got[1234]), pct(got[5678]))
	}
	c.t = c.t.Add(2 * time.Second)
	sys.outputs["ps 1234,5678"] = []byte(" 1234   0:15.50\n 5678 250:43.69\n")
	if got := cpus(t, f); !near(got[1234], 275) || !near(got[5678], 0) {
		t.Fatalf("cpu = %v %v, want 275 and 0", pct(got[1234]), pct(got[5678]))
	}
}

func TestCPUStartsOverForAReusedPID(t *testing.T) {
	sys := newFake()
	f, c := cpuFinder(sys)
	sys.outputs = map[string][]byte{"ps 1234,5678": []byte(" 1234   0:10.00\n 5678   0:01.00\n")}
	cpus(t, f)
	c.t = c.t.Add(2 * time.Second)
	sys.outputs["ps 1234,5678"] = []byte(" 1234   0:15.50\n 5678   0:02.00\n")
	if got := cpus(t, f); !near(got[1234], 275) {
		t.Fatalf("cpu = %v, want 275", pct(got[1234]))
	}
	// The VM exits; its PID is reused by one that has run longer.
	sys.pids = []int{5678}
	sys.outputs["ps 5678"] = []byte(" 5678   0:03.00\n")
	c.t = c.t.Add(2 * time.Second)
	cpus(t, f)
	sys.pids = []int{1234, 5678}
	sys.outputs["ps 1234,5678"] = []byte(" 1234   0:20.00\n 5678   0:04.00\n")
	c.t = c.t.Add(2 * time.Second)
	if got := cpus(t, f); got[1234] != nil || !near(got[5678], 50) {
		t.Fatalf("cpu = %v %v, want unknown for the reused PID and 50", pct(got[1234]), pct(got[5678]))
	}
}

func TestCPUTimeThatWentBackwardsIsUnknown(t *testing.T) {
	sys := newFake()
	f, c := cpuFinder(sys)
	sys.outputs = map[string][]byte{"ps 1234,5678": []byte(" 1234   0:10.00\n 5678   0:01.00\n")}
	cpus(t, f)
	c.t = c.t.Add(2 * time.Second)
	sys.outputs["ps 1234,5678"] = []byte(" 1234   0:09.00\n 5678   0:02.00\n")
	if got := cpus(t, f); got[1234] != nil || !near(got[5678], 50) {
		t.Fatalf("cpu = %v %v, want unknown and 50", pct(got[1234]), pct(got[5678]))
	}
}

func TestPSFailureLeavesCPUUnknownAndKeepsFootprints(t *testing.T) {
	sys := newFake()
	f, c := cpuFinder(sys)
	sys.outputs = map[string][]byte{"ps 1234,5678": []byte(" 1234   0:10.00\n 5678   0:01.00\n")}
	cpus(t, f)
	c.t = c.t.Add(2 * time.Second)
	sys.outputs["ps 1234,5678"] = []byte(" 1234   0:15.50\n 5678   0:02.00\n")
	if got := cpus(t, f); !near(got[1234], 275) {
		t.Fatalf("cpu = %v, want 275", pct(got[1234]))
	}
	delete(sys.outputs, "ps 1234,5678") // ps fails
	c.t = c.t.Add(2 * time.Second)
	vms, err := f.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, vm := range vms {
		if vm.CPUPercent != nil || vm.FootprintBytes != 1709280520 {
			t.Fatalf("vm %d: cpu %v, footprint %d; want unknown CPU and the footprint", vm.PID, pct(vm.CPUPercent), vm.FootprintBytes)
		}
	}
	if len(vms) != 2 {
		t.Fatalf("got %d VMs, want 2", len(vms))
	}
}

func TestAnUnparsableLineLeavesOnlyThatPIDUnknown(t *testing.T) {
	sys := newFake()
	f, c := cpuFinder(sys)
	sys.outputs = map[string][]byte{"ps 1234,5678": []byte(" 1234   0:10.00\n 5678   0:01.00\n")}
	cpus(t, f)
	c.t = c.t.Add(2 * time.Second)
	// hh:mm:ss is not how macOS ps prints CPU time.
	sys.outputs["ps 1234,5678"] = []byte(" 1234   0:15.50\n 5678 1:00:02.00\n")
	if got := cpus(t, f); !near(got[1234], 275) || got[5678] != nil {
		t.Fatalf("cpu = %v %v, want 275 and unknown", pct(got[1234]), pct(got[5678]))
	}
}

func TestCPUTimesParseAsPSPrintsThem(t *testing.T) {
	sys := newFake()
	sys.pids = []int{1001, 2002, 3003, 4004}
	for _, pid := range []string{"1001", "2002", "3003", "4004"} {
		sys.files["lsof "+pid] = "lsof-docker.txt"
		sys.files["footprint "+pid] = "footprint.txt"
	}
	f, c := cpuFinder(sys)
	// Two runs of ps 2 s apart: 0:35.65, 10:04.64, 227:33.61 and 0:00.00.
	sys.files["ps 1001,2002,3003,4004"] = "ps-cputime-1.txt"
	cpus(t, f)
	c.t = c.t.Add(2 * time.Second)
	sys.files["ps 1001,2002,3003,4004"] = "ps-cputime-2.txt"
	got := cpus(t, f)
	if !near(got[1001], 0) || !near(got[2002], 0) || !near(got[3003], 11.5) || !near(got[4004], 0) {
		t.Fatalf("cpu = %v %v %v %v, want 0 0 11.5 0", pct(got[1001]), pct(got[2002]), pct(got[3003]), pct(got[4004]))
	}
	// A minute later, one more minute of CPU time each.
	c.t = c.t.Add(time.Minute)
	sys.outputs = map[string][]byte{"ps 1001,2002,3003,4004": []byte(" 1001   1:35.65\n 2002  11:04.64\n 3003 228:33.84\n 4004   1:00.00\n")}
	got = cpus(t, f)
	for _, pid := range []int{1001, 2002, 3003, 4004} {
		if !near(got[pid], 100) {
			t.Fatalf("cpu of %d = %v, want 100", pid, pct(got[pid]))
		}
	}
}

func TestNoPSRunWithoutVMs(t *testing.T) {
	sys := newFake()
	sys.pids = nil
	if _, err := vmproc.New(sys).List(context.Background()); err != nil {
		t.Fatal(err)
	}
	for key := range sys.calls {
		if strings.HasPrefix(key, "ps ") {
			t.Fatalf("ran %q with no VM process", key)
		}
	}
}
