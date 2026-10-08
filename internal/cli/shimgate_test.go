package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/shim"
)

// shimRig runs the shim against a fake daemon (ask) and records what it
// would exec.
type shimRig struct {
	t        *testing.T
	dir      string // PATH dir with fake docker and tart, and the config dir
	env      []string
	ask      func(protocol.CheckRequest) (*protocol.Decision, error)
	asked    []protocol.CheckRequest
	execed   string
	execEnv  []string
	sleeps   int
	now      time.Time
	sleepFor time.Duration // how far the clock moves per wait
	stop     bool          // the wait is interrupted
	execErr  error         // what exec returns
	released []string
	raised   os.Signal
	pending  os.Signal // a signal that came during an ask
}

func newShimRig(t *testing.T) *shimRig {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	for _, n := range []string{"docker", "tart"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!real\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &shimRig{t: t, dir: dir, env: []string{"PATH=" + dir, "HEADROOM_CONFIG_DIR=" + dir}, now: time.Unix(1e9, 0), sleepFor: 2 * time.Second}
}

func (r *shimRig) run(argv ...string) (code int, stderr string) {
	var errb strings.Builder
	var mu sync.Mutex
	code = Run(Env{Args: argv, Stdout: io.Discard, Stderr: &errb,
		Getenv:  func(string) string { return "" },
		Environ: func() []string { return r.env },
		exec: func(path string, _, env []string) error {
			r.execed, r.execEnv = path, env
			return r.execErr
		},
		release: func(_ config.Config, id string) error {
			r.released = append(r.released, id)
			return nil
		},
		ask: func(_ config.Config, req protocol.CheckRequest) (*protocol.Decision, error) {
			mu.Lock()
			defer mu.Unlock()
			r.asked = append(r.asked, req)
			return r.ask(req)
		},
		ancestors: func() []int { return []int{4321, 1} },
		now:       func() time.Time { return r.now },
		wait: func(d time.Duration) os.Signal {
			if d == 0 { // is a signal pending?
				return r.pending
			}
			r.sleeps++
			r.now = r.now.Add(r.sleepFor)
			if r.stop {
				return syscall.SIGINT
			}
			return nil
		},
		raise: func(s os.Signal) { r.raised = s },
	})
	return code, errb.String()
}

func allow(protocol.CheckRequest) (*protocol.Decision, error) {
	return &protocol.Decision{Allow: true, LeaseID: "lease-7", Message: "headroom: allowed"}, nil
}

func TestShimGateAllows(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "ORCA_WORKTREE_ID=repo::/Users/dev/src/project-a")
	r.ask = allow
	code, stderr := r.run("docker", "run", "--rm", "-m", "2g", "-e", "TOKEN=x", "alpine", "true")
	if code != 0 || r.execed != filepath.Join(r.dir, "docker") {
		t.Fatalf("exit %d, exec %q, stderr %q", code, r.execed, stderr)
	}
	// The environment names the worktree: no cwd or ancestry needed.
	want := protocol.CheckRequest{Worktree: "repo::/Users/dev/src/project-a", Kind: "container", Command: "docker run alpine",
		CostBytes: 2 << 30}
	if len(r.asked) != 1 || !equalRequest(r.asked[0], want) {
		t.Fatalf("asked %+v, want %+v", r.asked, want)
	}
	// The marker is this process's PID, which the real binary keeps: only
	// a binary it execs in its place skips the check, not its children.
	if !slices.Contains(r.execEnv, "HEADROOM_SHIM_CHECKED="+shim.Self()) {
		t.Fatalf("child env lacks the checked marker: %q", r.execEnv)
	}
}

func TestShimGateSendsCwdAndAncestry(t *testing.T) {
	r := newShimRig(t)
	r.ask = allow
	realDir := filepath.Join(r.dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(r.dir, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(link)
	r.run("docker", "run", "alpine")
	got := r.asked[0]
	resolved, _ := filepath.EvalSymlinks(realDir)
	if !slices.Equal(got.Ancestors, []int{4321, 1}) || got.Cwd == "" || (got.Cwd != resolved && got.RealCwd != resolved) {
		t.Fatalf("asked %+v, want ancestry and the cwd resolved to %s", got, resolved)
	}
}

func TestShimGateReleasesTheLeaseIfExecFails(t *testing.T) {
	r := newShimRig(t)
	r.ask = allow
	r.execErr = syscall.ENOENT
	if code, _ := r.run("docker", "run", "alpine"); code != 127 || !slices.Equal(r.released, []string{"lease-7"}) {
		t.Fatalf("exit %d, released %q", code, r.released)
	}
}

func equalRequest(a, b protocol.CheckRequest) bool {
	return a.Worktree == b.Worktree && a.Kind == b.Kind && a.Command == b.Command && a.CostBytes == b.CostBytes &&
		a.Cwd == b.Cwd && a.RealCwd == b.RealCwd && slices.Equal(a.Ancestors, b.Ancestors)
}

func TestShimGateHeadroomWorktreeOverridesOrca(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "ORCA_WORKTREE_ID=orca-id", "HEADROOM_WORKTREE=override")
	r.ask = allow
	r.run("tart", "run", "ci-vm")
	if len(r.asked) != 1 || r.asked[0].Worktree != "override" || r.asked[0].Kind != "tart" || r.asked[0].Command != "tart run ci-vm" {
		t.Fatalf("asked %+v", r.asked)
	}
}

func TestShimGateDenies(t *testing.T) {
	r := newShimRig(t)
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return &protocol.Decision{Retry: true, Message: "headroom: not starting `docker run alpine` (≈ 1.0 GB): only 0.5 GB headroom"}, nil
	}
	code, stderr := r.run("docker", "run", "alpine")
	if code != exitDenied || r.execed != "" || !strings.Contains(stderr, "only 0.5 GB headroom") || !strings.Contains(stderr, "BUDGET_WAIT=1") {
		t.Fatalf("exit %d, exec %q, stderr %q", code, r.execed, stderr)
	}
	// A deny waiting cannot fix says so.
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return &protocol.Decision{Message: "headroom: not starting it: over its cap"}, nil
	}
	_, stderr = r.run("docker", "run", "alpine")
	if strings.Contains(stderr, "BUDGET_WAIT") || !strings.Contains(stderr, "waiting will not help") {
		t.Fatalf("stderr %q", stderr)
	}
}

func TestShimGateDoesNotAsk(t *testing.T) {
	for name, c := range map[string]struct {
		argv []string
		env  []string
	}{
		"a call that starts nothing": {[]string{"docker", "ps"}, nil},
		"already checked":            {[]string{"docker", "run", "alpine"}, []string{"HEADROOM_SHIM_CHECKED=" + shim.Self()}},
		"a remote engine":            {[]string{"docker", "run", "alpine"}, []string{"DOCKER_HOST=ssh://dev@build-box"}},
		"a remote engine by flag":    {[]string{"docker", "-H", "tcp://10.0.0.5:2376", "run", "alpine"}, nil},
	} {
		r := newShimRig(t)
		r.env = append(r.env, c.env...)
		r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
			t.Errorf("%s: asked the daemon", name)
			return nil, errors.New("no")
		}
		if code, _ := r.run(c.argv...); code != 0 || r.execed == "" {
			t.Errorf("%s: exit %d, exec %q", name, code, r.execed)
		}
	}
}

func TestShimGateChecksAChildOfACheckedCall(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "HEADROOM_SHIM_CHECKED=1") // another process's marker
	r.ask = allow
	if r.run("docker", "run", "alpine"); len(r.asked) != 1 {
		t.Fatalf("asked %d times", len(r.asked))
	}
}

func TestShimGateOldDaemon(t *testing.T) {
	r := newShimRig(t)
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return nil, fmt.Errorf("%w (unknown op)", errCannotCheck)
	}
	code, stderr := r.run("docker", "run", "alpine")
	if code != 0 || r.execed == "" || !strings.Contains(stderr, "headroom install") || strings.Contains(stderr, "not reachable") {
		t.Fatalf("exit %d, exec %q, stderr %q", code, r.execed, stderr)
	}
}

func TestShimGateFailsOpen(t *testing.T) {
	r := newShimRig(t)
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) { return nil, errors.New("connection refused") }
	code, stderr := r.run("docker", "run", "alpine")
	if code != 0 || r.execed == "" || !strings.Contains(stderr, "not gated") {
		t.Fatalf("exit %d, exec %q, stderr %q", code, r.execed, stderr)
	}
}

func TestShimGateFailsOpenOnBadConfig(t *testing.T) {
	r := newShimRig(t)
	if err := os.WriteFile(filepath.Join(r.dir, "config.toml"), []byte("[policy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.ask = allow
	code, stderr := r.run("docker", "run", "alpine")
	if code != 0 || r.execed == "" || len(r.asked) != 0 || !strings.Contains(stderr, "not gated") {
		t.Fatalf("exit %d, exec %q, asked %d, stderr %q", code, r.execed, len(r.asked), stderr)
	}
}

func TestShimGateWaits(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "BUDGET_WAIT=1")
	n := 0
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		if n++; n < 3 {
			return &protocol.Decision{Retry: true, Message: "headroom: not starting it: only 0.5 GB headroom",
				Reasons: []protocol.Reason{{Code: "headroom", Text: "only 0.5 GB headroom", Retry: true}}}, nil
		}
		return allow(protocol.CheckRequest{})
	}
	code, stderr := r.run("docker", "run", "alpine")
	if code != 0 || r.execed == "" || len(r.asked) != 3 || r.sleeps != 2 {
		t.Fatalf("exit %d, exec %q, asked %d, slept %d, stderr %q", code, r.execed, len(r.asked), r.sleeps, stderr)
	}
	if strings.Count(stderr, "waiting") != 1 || !strings.Contains(stderr, "only 0.5 GB headroom") {
		t.Fatalf("want one waiting line, stderr %q", stderr)
	}
}

func TestShimGateWaitsOnlyWhenItHelps(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "BUDGET_WAIT=1")
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return &protocol.Decision{Message: "headroom: not starting it: over its cap"}, nil
	}
	if code, _ := r.run("docker", "run", "alpine"); code != exitDenied || len(r.asked) != 1 || r.sleeps != 0 {
		t.Fatalf("exit %d, asked %d, slept %d", code, len(r.asked), r.sleeps)
	}
}

func TestShimGateWaitGivesUp(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "BUDGET_WAIT=1")
	r.sleepFor = 4 * time.Minute // the 10-minute default runs out on the third wait
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return &protocol.Decision{Retry: true, Message: "headroom: not starting it: only 0.5 GB headroom"}, nil
	}
	code, stderr := r.run("docker", "run", "alpine")
	if code != exitDenied || r.execed != "" || !strings.Contains(stderr, "gave up") || !strings.Contains(stderr, "only 0.5 GB headroom") {
		t.Fatalf("exit %d, exec %q, stderr %q", code, r.execed, stderr)
	}
}

func TestShimGateWaitInterrupted(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "BUDGET_WAIT=1")
	r.stop = true
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return &protocol.Decision{Retry: true, Message: "headroom: not starting it"}, nil
	}
	// It dies of the signal, so a shell loop around it stops too.
	if code, _ := r.run("docker", "run", "alpine"); code != 130 || r.execed != "" || r.raised != syscall.SIGINT {
		t.Fatalf("exit %d, exec %q, raised %v", code, r.execed, r.raised)
	}
}

// End to end against the daemon's own wiring: of two calls that do not both
// fit, one runs and the other is denied (R5, R10).
func TestShimGateAgainstTheDaemon(t *testing.T) {
	envMap, _ := serveDaemonWith(t, func(d *daemon.Daemon) {
		wireGate(d, config.Defaults("/x"), discardLog())
	})
	r := newShimRig(t)
	r.env = []string{"PATH=" + r.dir, "HEADROOM_CONFIG_DIR=" + envMap["HEADROOM_CONFIG_DIR"], "HEADROOM_WORKTREE=w"}
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			code := Run(Env{Args: []string{"docker", "run", "-m", "40g", "alpine"}, Stdout: io.Discard, Stderr: io.Discard,
				Getenv: func(string) string { return "" }, Environ: func() []string { return r.env },
				exec: func(string, []string, []string) error { return nil }})
			codes <- code
		})
	}
	wg.Wait()
	close(codes)
	var got []int
	for c := range codes {
		got = append(got, c)
	}
	slices.Sort(got)
	if !slices.Equal(got, []int{0, exitDenied}) {
		t.Fatalf("exit codes %v, want one run and one deny", got)
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestShimGateDebugNamesTheWorktree(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "HEADROOM_SHIM_DEBUG=1")
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return &protocol.Decision{Allow: true, Worktree: "repo::/Users/dev/src/project-a", IdentifiedBy: protocol.IdentifiedByProcess}, nil
	}
	_, stderr := r.run("docker", "run", "alpine")
	if !strings.Contains(stderr, "headroom: allowed for repo::/Users/dev/src/project-a (by process)") {
		t.Fatalf("stderr %q", stderr)
	}
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) { return &protocol.Decision{Allow: true}, nil }
	if _, stderr = r.run("docker", "run", "alpine"); !strings.Contains(stderr, "headroom: allowed as a manual call") {
		t.Fatalf("stderr %q", stderr)
	}
}

// A call whose exec fails hands its lease back to the daemon.
func TestShimGateReleaseAgainstTheDaemon(t *testing.T) {
	envMap, d := serveDaemonWith(t, func(d *daemon.Daemon) {
		wireGate(d, config.Defaults("/x"), discardLog())
	})
	r := newShimRig(t)
	env := []string{"PATH=" + r.dir, "HEADROOM_CONFIG_DIR=" + envMap["HEADROOM_CONFIG_DIR"], "HEADROOM_WORKTREE=w"}
	code := Run(Env{Args: []string{"docker", "run", "-m", "40g", "alpine"}, Stdout: io.Discard, Stderr: io.Discard,
		Getenv: func(string) string { return "" }, Environ: func() []string { return env },
		exec: func(string, []string, []string) error { return syscall.ENOENT }})
	d.Tick(context.Background())
	if s := d.Snapshot(); code != 127 || len(s.Leases) != 0 {
		t.Fatalf("exit %d, leases %+v", code, s.Leases)
	}
}

// A Ctrl-C during the ask that finds room still cancels the call.
func TestShimGateWaitCancelledDuringTheAsk(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "BUDGET_WAIT=1")
	n := 0
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		if n++; n == 1 {
			return &protocol.Decision{Retry: true, Message: "headroom: not starting it"}, nil
		}
		r.pending = syscall.SIGINT
		return allow(protocol.CheckRequest{})
	}
	code, _ := r.run("docker", "run", "alpine")
	if code != 130 || r.execed != "" || r.raised != syscall.SIGINT || !slices.Equal(r.released, []string{"lease-7"}) {
		t.Fatalf("exit %d, exec %q, raised %v, released %q", code, r.execed, r.raised, r.released)
	}
}

// tart run asks with the VM's own memory and whether it takes a macOS
// slot; tart clone runs nothing and is not asked about (#29).
func TestShimGateTartVMs(t *testing.T) {
	r := newShimRig(t)
	tartHome := filepath.Join(r.dir, "tarthome")
	if err := os.MkdirAll(filepath.Join(tartHome, "vms", "mac-ci"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tartHome, "vms", "mac-ci", "config.json"), []byte(`{"os":"darwin","memorySize":17179869184}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.env = append(r.env, "TART_HOME="+tartHome)
	r.ask = allow
	r.run("tart", "run", "--no-graphics", "mac-ci")
	if len(r.asked) != 1 || !r.asked[0].MacOS || r.asked[0].CostBytes != 16<<30 || r.asked[0].Kind != "tart" {
		t.Fatalf("asked %+v", r.asked)
	}
	r.env = append(r.env, "HEADROOM_SHIM_DEBUG=1")
	_, stderr := r.run("tart", "run", "not-here")
	if len(r.asked) != 2 || r.asked[1].MacOS || r.asked[1].CostBytes != 0 || !strings.Contains(stderr, "no config for the VM in `tart run not-here`") {
		t.Fatalf("unknown VM asked %+v, stderr %q", r.asked[1], stderr)
	}
	if r.asked[0].PID != os.Getpid() {
		t.Fatalf("a tart run must name its process, which becomes tart's: %+v", r.asked[0])
	}
	if code, _ := r.run("tart", "clone", "mac-ci", "mac-ci-2"); code != 0 || len(r.asked) != 2 || r.execed == "" {
		t.Fatalf("clone: exit %d, asked %d", code, len(r.asked))
	}
}

// fakeTart is a Tart source whose running VMs a test can change.
type fakeTart struct {
	mu  sync.Mutex
	vms []protocol.TartVM
}

func (f *fakeTart) Name() string { return "tart" }
func (f *fakeTart) Collect(context.Context) (daemon.Reading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := protocol.Tart{Installed: true, VMs: slices.Clone(f.vms)}
	for _, vm := range t.VMs {
		if vm.OS == "darwin" {
			t.MacOSRunning++
		}
	}
	return tartReading{t}, nil
}

type tartReading struct{ t protocol.Tart }

func (r tartReading) Apply(s *protocol.Snapshot) { s.Tart = &r.t }

// End to end: a third macOS VM is denied naming both holders; waiting, it
// starts once one of them stops (R6).
func TestShimGateMacOSSlotsAgainstTheDaemon(t *testing.T) {
	tart := &fakeTart{vms: []protocol.TartVM{{Name: "a-mac", OS: "darwin"}, {Name: "b-mac", OS: "darwin"}}}
	envMap, d := serveDaemonFrom(t, []daemon.Source{hostSource{}, tart}, func(d *daemon.Daemon) {
		wireGate(d, config.Defaults("/x"), discardLog())
	})
	r := newShimRig(t)
	tartHome := filepath.Join(r.dir, "tarthome")
	if err := os.MkdirAll(filepath.Join(tartHome, "vms", "c-mac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tartHome, "vms", "c-mac", "config.json"), []byte(`{"os":"darwin","memorySize":8589934592}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + r.dir, "HEADROOM_CONFIG_DIR=" + envMap["HEADROOM_CONFIG_DIR"], "HEADROOM_WORKTREE=w", "TART_HOME=" + tartHome}
	var stderr strings.Builder
	execed := false
	shimEnv := Env{Args: []string{"tart", "run", "c-mac"}, Stdout: io.Discard, Stderr: &stderr,
		Getenv: func(string) string { return "" }, Environ: func() []string { return env },
		exec: func(string, []string, []string) error { execed = true; return nil }}
	if code := Run(shimEnv); code != exitDenied || execed || !strings.Contains(stderr.String(), "a-mac (manual)") ||
		!strings.Contains(stderr.String(), "b-mac (manual)") {
		t.Fatalf("exit %d, exec %v, stderr %q", code, execed, stderr.String())
	}
	env = append(env, "BUDGET_WAIT=1")
	shimEnv.wait = func(dur time.Duration) os.Signal {
		if dur == 0 {
			return nil // no signal pending
		}
		tart.mu.Lock()
		tart.vms = tart.vms[1:] // a-mac stops
		tart.mu.Unlock()
		d.Tick(context.Background())
		return nil
	}
	if code := Run(shimEnv); code != 0 || !execed {
		t.Fatalf("waiting: exit %d, exec %v, stderr %q", code, execed, stderr.String())
	}
}

func TestDenyHintFitsTheReason(t *testing.T) {
	for _, c := range []struct {
		d    protocol.Decision
		want string
	}{
		{protocol.Decision{Retry: true, Reasons: []protocol.Reason{{Code: policy.Headroom, Retry: true}}}, "BUDGET_WAIT=1 to wait for room"},
		{protocol.Decision{Retry: true, Reasons: []protocol.Reason{{Code: policy.VMSlots, Retry: true}}}, "BUDGET_WAIT=1 to wait for a slot"},
		{protocol.Decision{Reasons: []protocol.Reason{{Code: policy.VMSlots}}}, "budget.max_macos_vms"},
		{protocol.Decision{Reasons: []protocol.Reason{{Code: policy.WorktreeCap}}}, "reuse or stop"},
		{protocol.Decision{Reasons: []protocol.Reason{{Code: policy.IdleHolder}}}, "reuse or stop"},
		// Full slots can wait; the cap that also fails cannot.
		{protocol.Decision{Reasons: []protocol.Reason{{Code: policy.VMSlots, Retry: true}, {Code: policy.WorktreeCap}}}, "reuse or stop"},
	} {
		if got := denyHint(&c.d); !strings.Contains(got, c.want) {
			t.Errorf("%+v: hint %q, want %q", c.d.Reasons, got, c.want)
		}
	}
}
