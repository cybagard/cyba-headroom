package vmproc_test

import (
	"context"
	"errors"
	"os"
	"path"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// fakeSystem lists fixed PIDs and answers lsof/footprint from captured output.
type fakeSystem struct {
	pids  []int
	files map[string]string // "lsof <pid>" or "footprint <pid>" -> testdata file
	calls map[string]int
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
