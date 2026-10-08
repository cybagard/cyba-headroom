package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// doctorRig is a config dir with shim links to a headroom binary, a dir of
// real tools, a daemon snapshot and a login shell's answers.
type doctorRig struct {
	cfg, shims, tools, bin string
	env                    map[string]string
	snap                   *protocol.Snapshot
	statusErr              error
	login                  string // what `$SHELL -lc` prints
	loginErr               error
	ancestors              []int
	cwd                    string
}

func newDoctorRig(t *testing.T) *doctorRig {
	t.Helper()
	cfg, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cfg) })
	r := &doctorRig{cfg: cfg, shims: filepath.Join(cfg, "shims"), tools: filepath.Join(cfg, "tools"),
		bin: filepath.Join(cfg, "bin", "headroom")}
	for _, p := range []string{r.bin, filepath.Join(r.tools, "docker"), filepath.Join(r.tools, "podman"), filepath.Join(r.tools, "tart")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := linkShims(r.shims, r.bin); err != nil {
		t.Fatal(err)
	}
	r.env = map[string]string{
		"HEADROOM_CONFIG_DIR": cfg,
		"PATH":                r.shims + ":" + r.tools + ":/usr/bin",
		"HEADROOM_WORKTREE":   "wt-a",
		"SHELL":               "/bin/zsh",
	}
	r.snap = &protocol.Snapshot{Orca: &protocol.Orca{Installed: true, Running: true, Worktrees: []protocol.Worktree{
		{ID: "wt-a", Name: "Fix login", Path: "/Users/dev/src/a", SessionPIDs: []int{500}},
		{ID: "wt-b", Name: "Release prep", Path: "/Users/dev/src/b"},
	}}}
	r.login = "docker=" + filepath.Join(r.shims, "docker") + "\npodman=" + filepath.Join(r.shims, "podman") + "\ntart=" + filepath.Join(r.shims, "tart") + "\n"
	r.cwd = "/Users/dev/src/a"
	return r
}

func (r *doctorRig) run() (int, string) {
	var out strings.Builder
	code := Run(Env{Args: []string{"headroom", "doctor"}, Stdout: &out, Stderr: &out,
		Getenv:    func(k string) string { return r.env[k] },
		ancestors: func() []int { return r.ancestors },
		getwd:     func() (string, error) { return r.cwd, nil },
		fallbacks: map[string][]string{}, // only the rig's tools, not the host's
		status: func(config.Config) (*protocol.Snapshot, error) {
			return r.snap, r.statusErr
		},
		loginShell: func(shell string) (string, error) {
			want := r.env["SHELL"]
			if want == "" || strings.HasSuffix(want, "/fish") {
				want = "/bin/zsh"
			}
			if shell != want {
				return "", errors.New("unexpected shell " + shell)
			}
			return r.login, r.loginErr
		},
	})
	return code, out.String()
}

// line returns the output line for check name, failing if absent.
func doctorLine(t *testing.T, out, name string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 1 && f[1] == name {
			return l
		}
	}
	t.Fatalf("no %q line in:\n%s", name, out)
	return ""
}

func wantMark(t *testing.T, out, name, mark string, subs ...string) {
	t.Helper()
	l := doctorLine(t, out, name)
	if !strings.HasPrefix(strings.TrimSpace(l), mark) {
		t.Errorf("%s line %q, want mark %s\n%s", name, l, mark, out)
	}
	for _, s := range subs {
		if !strings.Contains(l, s) {
			t.Errorf("%s line %q lacks %q", name, l, s)
		}
	}
}

func TestDoctorAllGood(t *testing.T) {
	r := newDoctorRig(t)
	code, out := r.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantMark(t, out, "config", "✓", r.cfg)
	wantMark(t, out, "shims", "✓", "docker, podman, tart", r.bin)
	wantMark(t, out, "PATH", "✓", "docker, podman, tart")
	wantMark(t, out, "daemon", "✓")
	wantMark(t, out, "identity", "✓", `"Fix login"`, "HEADROOM_WORKTREE")
	wantMark(t, out, "login", "✓")
	// The real binary behind each shim.
	if !strings.Contains(out, filepath.Join(r.tools, "docker")) {
		t.Errorf("real docker not shown:\n%s", out)
	}
}

func TestDoctorPATHWithoutTheShims(t *testing.T) {
	r := newDoctorRig(t)
	r.env["PATH"] = r.tools + ":/usr/bin"
	code, out := r.run()
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	wantMark(t, out, "PATH", "✗", "docker resolves to "+filepath.Join(r.tools, "docker"), "headroom run")
}

func TestDoctorPATHWithTheShimsLast(t *testing.T) {
	r := newDoctorRig(t)
	r.env["PATH"] = r.tools + ":" + r.shims
	if code, out := r.run(); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	} else {
		wantMark(t, out, "PATH", "✗", "not the shim")
	}
}

func TestDoctorToolNotInstalledIsNotAFailure(t *testing.T) {
	r := newDoctorRig(t)
	if err := os.Remove(filepath.Join(r.tools, "podman")); err != nil {
		t.Fatal(err)
	}
	code, out := r.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "podman") || !strings.Contains(out, "not installed") {
		t.Errorf("podman not reported as not installed:\n%s", out)
	}
}

func TestDoctorToolOffPATHIsNotAFailure(t *testing.T) {
	r := newDoctorRig(t)
	other := filepath.Join(r.cfg, "elsewhere")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(r.tools, "tart"), filepath.Join(other, "tart")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r.shims, "tart")); err != nil {
		t.Fatal(err)
	}
	r.env["PATH"] = r.tools + ":/usr/bin" // no shims at all
	_, out := r.run()
	l := doctorLine(t, out, "PATH")
	if strings.Contains(l, "tart resolves") {
		t.Fatalf("tart, not on PATH, reported as resolving wrong: %q", l)
	}
}

func TestDoctorRealToolInTheShimDirIsNotAShim(t *testing.T) {
	r := newDoctorRig(t)
	p := filepath.Join(r.shims, "docker")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out := r.run()
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	wantMark(t, out, "PATH", "✗", "docker resolves to "+p)
	// Not a link: install leaves it alone, so say to remove it.
	wantMark(t, out, "shims", "✗", "docker is not a link", "remove it")
}

func TestDoctorNothingThroughTheShimsFails(t *testing.T) {
	r := newDoctorRig(t)
	for _, n := range []string{"docker", "podman", "tart"} {
		if err := os.Remove(filepath.Join(r.tools, n)); err != nil {
			t.Fatal(err)
		}
	}
	r.env["PATH"] = "/usr/bin" // no shim dir, and no tools on it
	code, out := r.run()
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	wantMark(t, out, "PATH", "✗", "not on this PATH")
}

func TestDoctorRelativePATHEntryBeforeTheShims(t *testing.T) {
	r := newDoctorRig(t)
	r.env["PATH"] = ".:" + r.shims + ":" + r.tools
	_, out := r.run()
	wantMark(t, out, "PATH", "!", `"."`, "not an absolute path")
}

func TestDoctorShimsFromAnotherShimDir(t *testing.T) {
	r := newDoctorRig(t)
	other := filepath.Join(r.cfg, "old-shims")
	if _, err := linkShims(other, r.bin); err != nil {
		t.Fatal(err)
	}
	r.env["PATH"] = other + ":/usr/bin:bin:" + r.shims + ":" + r.tools
	_, out := r.run()
	l := doctorLine(t, out, "PATH")
	wantMark(t, out, "PATH", "!", other, "not the configured")
	if strings.Contains(l, `"bin"`) {
		t.Errorf("relative entry after the shims flagged: %q", l)
	}
}

func TestDoctorShimDirByAnotherSpelling(t *testing.T) {
	r := newDoctorRig(t)
	link := filepath.Join(r.cfg, "shims-link")
	if err := os.Symlink(r.shims, link); err != nil {
		t.Fatal(err)
	}
	r.env["PATH"] = link + ":" + r.tools + ":."
	code, out := r.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantMark(t, out, "PATH", "✓")
}

func TestDoctorBrokenShimsOnPATHFail(t *testing.T) {
	r := newDoctorRig(t)
	if err := os.Remove(r.bin); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"docker", "podman", "tart"} {
		if err := os.Remove(filepath.Join(r.tools, n)); err != nil {
			t.Fatal(err)
		}
	}
	_, out := r.run()
	wantMark(t, out, "PATH", "✗", "no shim")
}

func TestDoctorMissingShims(t *testing.T) {
	r := newDoctorRig(t)
	if err := os.Remove(filepath.Join(r.shims, "tart")); err != nil {
		t.Fatal(err)
	}
	code, out := r.run()
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	wantMark(t, out, "shims", "✗", "tart", "headroom install")
}

func TestDoctorDanglingShims(t *testing.T) {
	r := newDoctorRig(t)
	if err := os.Remove(r.bin); err != nil {
		t.Fatal(err)
	}
	if code, out := r.run(); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	} else {
		wantMark(t, out, "shims", "✗", "headroom install")
	}
}

func TestDoctorNoConfigFile(t *testing.T) {
	r := newDoctorRig(t)
	_, out := r.run()
	wantMark(t, out, "config", "✓", "no file", "defaults")
}

func TestDoctorUnreadableShim(t *testing.T) {
	r := newDoctorRig(t)
	// The shim dir's parent component is a file: ENOTDIR, not "not a link".
	r.env["HEADROOM_CONFIG_DIR"] = r.cfg
	if err := os.WriteFile(filepath.Join(r.cfg, config.FileName), []byte("shim_dir = \""+filepath.Join(r.bin, "shims")+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out := r.run()
	l := doctorLine(t, out, "shims")
	if strings.Contains(l, "remove it") || !strings.Contains(l, "missing") && !strings.Contains(l, "cannot read") {
		t.Fatalf("shims line %q", l)
	}
}

func TestDoctorBadConfig(t *testing.T) {
	r := newDoctorRig(t)
	if err := os.WriteFile(filepath.Join(r.cfg, config.FileName), []byte("[policy]\nnope = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := r.run()
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	wantMark(t, out, "config", "✗", "unknown keys")
	// The shims fail open on a bad config, from the default shim dir:
	// the other checks still run.
	doctorLine(t, out, "PATH")
}

func TestDoctorDaemonDownWarns(t *testing.T) {
	r := newDoctorRig(t)
	r.snap, r.statusErr = nil, errors.New("connection refused")
	code, out := r.run()
	if code != 0 {
		t.Fatalf("exit %d, want 0 (fail-open is by design):\n%s", code, out)
	}
	wantMark(t, out, "daemon", "!", "connection refused", "ungated")
	// The env's word still identifies the caller.
	wantMark(t, out, "identity", "✓", "wt-a")
}

func TestDoctorIdentity(t *testing.T) {
	cases := map[string]struct {
		edit func(*doctorRig)
		mark string
		subs []string
	}{
		"from ORCA_WORKTREE_ID": {func(r *doctorRig) {
			delete(r.env, "HEADROOM_WORKTREE")
			r.env["ORCA_WORKTREE_ID"] = "wt-a"
		}, "✓", []string{`"Fix login"`, "ORCA_WORKTREE_ID"}},
		"from the working directory": {func(r *doctorRig) {
			delete(r.env, "HEADROOM_WORKTREE")
			r.cwd = "/Users/dev/src/b/sub"
		}, "✓", []string{`"Release prep"`, "working directory"}},
		"from the terminal": {func(r *doctorRig) {
			delete(r.env, "HEADROOM_WORKTREE")
			r.cwd, r.ancestors = "/tmp", []int{900, 500, 1}
		}, "✓", []string{`"Fix login"`, "terminal"}},
		"manual": {func(r *doctorRig) {
			delete(r.env, "HEADROOM_WORKTREE")
			r.cwd = "/tmp"
		}, "!", []string{"manual", "no worktree"}},
		"unknown ID": {func(r *doctorRig) {
			r.env["HEADROOM_WORKTREE"] = "wt-gone"
		}, "!", []string{"wt-gone", "not one of Orca's worktrees"}},
		"env and directory disagree": {func(r *doctorRig) {
			r.cwd = "/Users/dev/src/b"
		}, "!", []string{`"Fix login"`}},
		"env and terminal disagree": {func(r *doctorRig) {
			r.env["HEADROOM_WORKTREE"] = "wt-b"
			r.cwd, r.ancestors = "/tmp", []int{900, 500}
		}, "!", []string{`"Release prep"`}},
		"daemon down, nothing in the env": {func(r *doctorRig) {
			delete(r.env, "HEADROOM_WORKTREE")
			r.snap, r.statusErr = nil, errors.New("refused")
		}, "!", []string{"unknown", "daemon"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newDoctorRig(t)
			tc.edit(r)
			code, out := r.run()
			if code != 0 {
				t.Fatalf("exit %d, want 0 (identity only warns):\n%s", code, out)
			}
			wantMark(t, out, "identity", tc.mark, tc.subs...)
		})
	}
}

func TestDoctorIdentityWithoutOrcasList(t *testing.T) {
	cases := map[string]struct {
		edit func(*doctorRig)
		subs []string
	}{
		"worktree without a display name": {func(r *doctorRig) {
			r.snap.Orca.Worktrees[0].Name = ""
		}, []string{`"a"`, "wt-a"}},
		"Orca not running": {func(r *doctorRig) {
			r.snap.Orca = &protocol.Orca{Installed: true}
		}, []string{"wt-a", "Orca is not running"}},
		"no Orca reading, nothing in the env": {func(r *doctorRig) {
			delete(r.env, "HEADROOM_WORKTREE")
			r.snap.Orca = nil
		}, []string{"unknown", "no Orca reading"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newDoctorRig(t)
			tc.edit(r)
			_, out := r.run()
			l := doctorLine(t, out, "identity")
			for _, sub := range tc.subs {
				if !strings.Contains(l, sub) {
					t.Errorf("identity line %q lacks %q", l, sub)
				}
			}
			if strings.Contains(l, "not one of Orca's worktrees") || strings.Contains(l, "manual") {
				t.Errorf("identity line %q blames the worktree", l)
			}
		})
	}
}

func TestDoctorUnknownIDWithAStaleOrcaList(t *testing.T) {
	r := newDoctorRig(t)
	r.env["HEADROOM_WORKTREE"] = "wt-new"
	r.snap.Attribution = &protocol.Attribution{OrcaStale: true}
	_, out := r.run()
	wantMark(t, out, "identity", "!", "wt-new", "out of date")
}

func TestDoctorIdentityMismatchNamesBoth(t *testing.T) {
	r := newDoctorRig(t)
	r.cwd = "/Users/dev/src/b"
	_, out := r.run()
	if !strings.Contains(out, "wt-b") || !strings.Contains(out, "charged to wt-a") {
		t.Fatalf("mismatch not explained:\n%s", out)
	}
}

func TestDoctorLoginShell(t *testing.T) {
	cases := map[string]struct {
		edit func(*doctorRig)
		mark string
		subs []string
	}{
		"path_helper puts docker first": {func(r *doctorRig) {
			r.login = "docker=/usr/local/bin/docker\npodman=\ntart=" + filepath.Join(r.shims, "tart") + "\n"
		}, "!", []string{"docker → /usr/local/bin/docker", "run ungated", "headroom shows them as ungated"}},
		"shell fails": {func(r *doctorRig) {
			r.login, r.loginErr = "", errors.New("signal: killed")
		}, "!", []string{"could not run", "killed"}},
		"no SHELL: zsh": {func(r *doctorRig) {
			delete(r.env, "SHELL")
		}, "✓", []string{"/bin/zsh -l"}},
		"finds none of the tools": {func(r *doctorRig) {
			r.login = "docker=\npodman=\ntart=\n"
		}, "✓", []string{"finds none"}},
		"a function": {func(r *doctorRig) {
			r.login = "docker=docker\npodman=" + filepath.Join(r.shims, "podman") + "\ntart=\n"
		}, "!", []string{"docker", "alias or function"}},
		"an alias": {func(r *doctorRig) {
			r.login = "docker=alias docker=/usr/local/bin/docker\npodman=\ntart=\n"
		}, "!", []string{"docker", "alias or function"}},
		"fish: probe with zsh": {func(r *doctorRig) {
			r.env["SHELL"] = "/opt/homebrew/bin/fish"
		}, "✓", []string{"/bin/zsh -l"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newDoctorRig(t)
			tc.edit(r)
			code, out := r.run()
			if code != 0 {
				t.Fatalf("exit %d, want 0 (login only warns):\n%s", code, out)
			}
			wantMark(t, out, "login", tc.mark, tc.subs...)
		})
	}
}

func TestDoctorLoginShellRunsBesideTheStatusCall(t *testing.T) {
	r := newDoctorRig(t)
	asked := make(chan struct{})
	var out strings.Builder
	Run(Env{Args: []string{"headroom", "doctor"}, Stdout: &out, Stderr: &out,
		Getenv:    func(k string) string { return r.env[k] },
		ancestors: func() []int { return nil },
		getwd:     func() (string, error) { return r.cwd, nil },
		fallbacks: map[string][]string{},
		status: func(config.Config) (*protocol.Snapshot, error) {
			select {
			case <-asked:
				return r.snap, nil
			case <-time.After(5 * time.Second):
				return nil, errors.New("login shell not started while the daemon was asked")
			}
		},
		loginShell: func(string) (string, error) {
			close(asked)
			return r.login, nil
		},
	})
	wantMark(t, out.String(), "daemon", "✓")
}

// A login shell that finds the running headroom under another name finds
// the shims.
func TestLoginFindingKnowsTheRunningBinary(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	f := loginFinding(func(string) string { return "/bin/zsh" }, func(string) (string, error) {
		return "docker=" + self + "\n", nil
	}, self)
	if f.mark != pass {
		t.Fatalf("login = %+v, want pass", f)
	}
}

func TestDoctorSanitisesWhatItPrints(t *testing.T) {
	r := newDoctorRig(t)
	r.snap.Orca.Worktrees[0].Name = "evil\x1b]52;c;cGF5bG9hZA==\x07name"
	_, out := r.run()
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x07) {
		t.Fatalf("control characters printed: %q", out)
	}
}

func TestDoctorRejectsArguments(t *testing.T) {
	r := newDoctorRig(t)
	var errb strings.Builder
	code := Run(Env{Args: []string{"headroom", "doctor", "--fix"}, Stdout: io.Discard, Stderr: &errb,
		Getenv: func(k string) string { return r.env[k] }})
	if code != 2 || !strings.Contains(errb.String(), "--fix") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}

// A profile that leaves a background process holding stdout must not hang
// doctor past its timeout.
func TestAskLoginShellDoesNotHang(t *testing.T) {
	sh := filepath.Join(t.TempDir(), "sh")
	if err := os.WriteFile(sh, []byte("#!/bin/sh\n(sleep 30) &\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := askLoginShell(sh); err == nil {
		t.Fatal("want a timeout error")
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("took %v", d)
	}
}

// A real login shell answers in the form doctor parses.
func TestAskLoginShellReal(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	out, err := askLoginShell("/bin/sh")
	if err != nil {
		t.Skipf("login sh: %v", err)
	}
	for _, n := range []string{"docker=", "podman=", "tart="} {
		if !strings.Contains(out, n) {
			t.Fatalf("output %q lacks %q", out, n)
		}
	}
}
