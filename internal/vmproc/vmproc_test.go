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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// fakeSystem lists fixed PIDs and answers lsof/footprint from captured output.
type fakeSystem struct {
	pids  []int
	files map[string]string // "lsof <pid>" or "footprint <pid>" -> testdata file
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
		if f.toolBroken {
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
// other_vms beside the tart source's reservation (#226 B1–B4).
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
