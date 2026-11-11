package cli

import (
	"bytes"
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/shellblock"
)

// shell-setup writes zsh's two files, in ZDOTDIR if set, and the login file
// bash reads: the first of its three that exists (#370).
func TestShellTargets(t *testing.T) {
	for _, c := range []struct {
		name, shell string
		env         map[string]string
		exist       []string
		want        []string
	}{
		{"zsh", "zsh", map[string]string{"HOME": "/Users/dev"}, nil,
			[]string{"/Users/dev/.zlogin", "/Users/dev/.zshrc"}},
		{"zsh with ZDOTDIR", "zsh", map[string]string{"HOME": "/Users/dev", "ZDOTDIR": "/Users/dev/.config/zsh"}, nil,
			[]string{"/Users/dev/.config/zsh/.zlogin", "/Users/dev/.config/zsh/.zshrc"}},
		{"bash, none", "bash", map[string]string{"HOME": "/Users/dev"}, nil,
			[]string{"/Users/dev/.bash_profile"}},
		{"bash, .bash_profile", "bash", map[string]string{"HOME": "/Users/dev"},
			[]string{"/Users/dev/.bash_profile", "/Users/dev/.bash_login", "/Users/dev/.profile"},
			[]string{"/Users/dev/.bash_profile"}},
		{"bash, .bash_login", "bash", map[string]string{"HOME": "/Users/dev"},
			[]string{"/Users/dev/.bash_login", "/Users/dev/.profile"},
			[]string{"/Users/dev/.bash_login"}},
		{"bash, .profile", "bash", map[string]string{"HOME": "/Users/dev"},
			[]string{"/Users/dev/.profile"},
			[]string{"/Users/dev/.profile"}},
	} {
		stat := func(p string) (fs.FileInfo, error) {
			if slices.Contains(c.exist, p) {
				return os.Stat("/")
			}
			return nil, fs.ErrNotExist
		}
		getenv := func(k string) string { return c.env[k] }
		if got := shellTargets(c.shell, getenv, stat); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// The shell is $SHELL's, or zsh, with a note, for any other.
func TestShellFromSHELL(t *testing.T) {
	for sh, want := range map[string]string{"/bin/zsh": "zsh", "/opt/homebrew/bin/bash": "bash", "/opt/homebrew/bin/fish": "zsh", "": "zsh"} {
		got, note := shellFrom(sh)
		if got != want || (note != "") != (want == "zsh" && sh != "/bin/zsh") {
			t.Errorf("%q: %q, note %q", sh, got, note)
		}
	}
}

// shellRig is a scratch HOME and config dir, and the environment that names
// them, for shell-setup. The real dotfiles are never read.
type shellRig struct {
	home, cfg string
	env       map[string]string
	lstat     func(string) (fs.FileInfo, error)
}

func newShellRig(t *testing.T, shell string) *shellRig {
	t.Helper()
	var dirs [2]string
	for i := range dirs {
		d, err := os.MkdirTemp("/tmp", "hr")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		dirs[i] = d
	}
	return &shellRig{home: dirs[0], cfg: dirs[1], env: map[string]string{
		"HOME": dirs[0], "HEADROOM_CONFIG_DIR": dirs[1], "SHELL": shell}}
}

func (r *shellRig) run(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = Run(Env{Args: append([]string{"headroom", "shell-setup"}, args...), Stdout: &out, Stderr: &errb,
		Getenv: func(k string) string { return r.env[k] }, lstat: r.lstat})
	return code, out.String(), errb.String()
}

func (r *shellRig) path(name string) string { return filepath.Join(r.home, name) }

func (r *shellRig) write(t *testing.T, name, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(r.path(name), []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(r.path(name), mode); err != nil {
		t.Fatal(err)
	}
}

// files is every file in HOME, with its content.
func (r *shellRig) files(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(r.home)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(r.path(e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

func wantIn(t *testing.T, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("output lacks %q:\n%s", w, s)
		}
	}
}

// With no flag, shell-setup shows the block and each file's state, and
// changes nothing.
func TestShellSetupShowsTheBlockAndChangesNothing(t *testing.T) {
	for shell, files := range map[string][]string{"/bin/zsh": {".zlogin", ".zshrc"}, "/bin/bash": {".profile"}} {
		r := newShellRig(t, shell)
		r.write(t, ".zshrc", "a=1\n", 0o644)
		r.write(t, ".profile", "b=2\n", 0o644)
		before := r.files(t)
		code, out, errs := r.run()
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", shell, code, errs)
		}
		wantIn(t, out, shellblock.Block(filepath.Base(shell), filepath.Join(r.cfg, "shims")), "shell-setup --apply")
		for _, f := range files {
			wantIn(t, out, r.path(f))
		}
		if after := r.files(t); !maps.Equal(after, before) {
			t.Errorf("%s: files changed: %v", shell, after)
		}
	}
}

// --apply writes the block once; a second run changes nothing, and says so.
// Each file keeps its mode.
func TestShellSetupApplyTwice(t *testing.T) {
	r := newShellRig(t, "/bin/zsh")
	r.write(t, ".zshrc", "a=1\n", 0o600)
	block := shellblock.Block("zsh", filepath.Join(r.cfg, "shims"))
	code, out, errs := r.run("--apply")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	want := map[string]string{".zshrc": "a=1\n\n" + block, ".zlogin": block}
	if got := r.files(t); !maps.Equal(got, want) {
		t.Fatalf("files: %q", got)
	}
	wantIn(t, out, "added", r.path(".zshrc"), "created", r.path(".zlogin"))
	for name, mode := range map[string]os.FileMode{".zshrc": 0o600, ".zlogin": 0o644} {
		if fi, err := os.Stat(r.path(name)); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: mode %v, %v; want %v", name, fi.Mode(), err, mode)
		}
	}
	code, out, errs = r.run("--apply")
	if code != 0 {
		t.Fatalf("again: exit %d: %s", code, errs)
	}
	if got := r.files(t); !maps.Equal(got, want) {
		t.Fatalf("again: files: %q", got)
	}
	if strings.Contains(out, "added") || strings.Contains(out, "created") || strings.Count(out, "unchanged") != 2 {
		t.Errorf("again: output:\n%s", out)
	}
}

// --remove restores every file byte for byte: a file --apply created goes,
// an empty one stays empty, a missing one stays missing.
func TestShellSetupRemoveRestoresTheFiles(t *testing.T) {
	for shell, start := range map[string]map[string]string{
		"/bin/zsh":  {".zshrc": "a=1", ".bash_profile": "b=2\n"},
		"/bin/bash": {".zlogin": "", ".profile": "c=3\r\n", ".zshrc": "a=1\n"},
	} {
		r := newShellRig(t, shell)
		for name, content := range start {
			r.write(t, name, content, 0o640)
		}
		for _, args := range [][]string{{"--apply"}, {"--remove"}} {
			if code, _, errs := r.run(args...); code != 0 {
				t.Fatalf("%s %v: exit %d: %s", shell, args, code, errs)
			}
		}
		if got := r.files(t); !maps.Equal(got, start) {
			t.Errorf("%s: files %q, want %q", shell, got, start)
		}
	}
}

// --shell overrides $SHELL.
func TestShellSetupShellFlag(t *testing.T) {
	r := newShellRig(t, "/bin/zsh")
	if code, _, errs := r.run("--apply", "--shell", "bash"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	want := map[string]string{".bash_profile": shellblock.Block("bash", filepath.Join(r.cfg, "shims"))}
	if got := r.files(t); !maps.Equal(got, want) {
		t.Fatalf("files %q", got)
	}
	if code, _, _ := r.run("--shell", "fish"); code != 2 {
		t.Errorf("--shell fish: exit %d, want 2", code)
	}
	if code, _, _ := r.run("--apply", "--remove"); code != 2 {
		t.Errorf("--apply --remove: exit %d, want 2", code)
	}
}

// The block names the shim dir the config gives, as the daemon links it.
func TestShellSetupUsesTheConfiguredShimDir(t *testing.T) {
	r := newShellRig(t, "/bin/zsh")
	delete(r.env, "HEADROOM_CONFIG_DIR")
	r.env["XDG_CONFIG_HOME"] = r.cfg
	_, out, _ := r.run()
	wantIn(t, out, shellblock.Line("zsh", filepath.Join(r.cfg, "headroom", "shims")))
}

// A symlinked dotfile, or another user's, is refused, with the line to add
// by hand; the other files are still written.
func TestShellSetupRefusesALinkOrAnotherOwnersFile(t *testing.T) {
	r := newShellRig(t, "/bin/zsh")
	r.write(t, "real", "a=1\n", 0o644)
	if err := os.Symlink(r.path("real"), r.path(".zshrc")); err != nil {
		t.Fatal(err)
	}
	line := shellblock.Line("zsh", filepath.Join(r.cfg, "shims"))
	code, out, errs := r.run("--apply")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	wantIn(t, out+errs, r.path(".zshrc"), "symlink", line)
	if b, _ := os.ReadFile(r.path("real")); string(b) != "a=1\n" {
		t.Errorf("the link's target changed: %q", b)
	}
	if _, err := os.Stat(r.path(".zlogin")); err != nil {
		t.Errorf(".zlogin not written: %v", err)
	}

	r = newShellRig(t, "/bin/zsh")
	r.write(t, ".zlogin", "a=1\n", 0o644)
	r.lstat = func(p string) (fs.FileInfo, error) {
		fi, err := os.Lstat(p)
		if err != nil || p != r.path(".zlogin") {
			return fi, err
		}
		return otherOwner{fi}, nil
	}
	line = shellblock.Line("zsh", filepath.Join(r.cfg, "shims"))
	code, out, errs = r.run("--apply")
	if code != 1 {
		t.Fatalf("other owner: exit %d, want 1", code)
	}
	wantIn(t, out+errs, r.path(".zlogin"), "owned by uid", line)
	if b, _ := os.ReadFile(r.path(".zlogin")); string(b) != "a=1\n" {
		t.Errorf("another owner's file changed: %q", b)
	}
}

// otherOwner is a FileInfo whose owner is not the euid.
type otherOwner struct{ fs.FileInfo }

func (otherOwner) Sys() any { return &syscall.Stat_t{Uid: uint32(os.Geteuid() + 1)} }

// A begin marker with no end is refused, and the file left as it is.
func TestShellSetupRefusesAnUnendedBlock(t *testing.T) {
	r := newShellRig(t, "/bin/zsh")
	broken := "a=1\n# >>> headroom shell-setup v1 >>>\nb=2\n"
	r.write(t, ".zshrc", broken, 0o644)
	for _, flag := range []string{"--apply", "--remove"} {
		code, out, errs := r.run(flag)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1", flag, code)
		}
		wantIn(t, out+errs, r.path(".zshrc"), "no end marker")
		if b, _ := os.ReadFile(r.path(".zshrc")); string(b) != broken {
			t.Errorf("%s: changed: %q", flag, b)
		}
	}
}

// finishes fails the test if f does not return within d.
func finishes(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s: still running after %s", what, d)
	}
}

// nonRegular makes name in dir a FIFO, or a link to /dev/zero: reading
// either never ends.
func nonRegular(t *testing.T, path, kind string) {
	t.Helper()
	var err error
	if kind == "fifo" {
		err = syscall.Mkfifo(path, 0o644)
	} else {
		err = os.Symlink("/dev/zero", path)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// A FIFO .zshrc, or one linked to /dev/zero, is refused at once in every
// mode, never read (#370).
func TestShellSetupRefusesAFIFOOrADevice(t *testing.T) {
	for _, kind := range []string{"fifo", "/dev/zero"} {
		for flag, wantCode := range map[string]int{"": 0, "--apply": 1, "--remove": 1} {
			r := newShellRig(t, "/bin/zsh")
			nonRegular(t, r.path(".zshrc"), kind)
			args := []string{}
			if flag != "" {
				args = append(args, flag)
			}
			finishes(t, 5*time.Second, kind+" "+flag, func() {
				code, out, errs := r.run(args...)
				if code != wantCode {
					t.Errorf("%s %s: exit %d, want %d: %s", kind, flag, code, wantCode, errs)
				}
				wantIn(t, out+errs, r.path(".zshrc"))
			})
		}
	}
}

// Uninstall keeps such a file, saying so, and does not hang.
func TestUninstallKeepsAFIFOOrADevice(t *testing.T) {
	for _, kind := range []string{"fifo", "/dev/zero"} {
		f := newInstallFixture(t)
		nonRegular(t, filepath.Join(f.home, ".zshrc"), kind)
		finishes(t, 5*time.Second, kind, func() {
			if code := f.in.uninstall(context.Background()); code != 0 {
				t.Errorf("%s: exit %d: %s", kind, code, f.errb.String())
			}
		})
		if _, kept, _ := strings.Cut(f.out.String(), "kept:"); !strings.Contains(kept, filepath.Join(f.home, ".zshrc")) {
			t.Errorf("%s: .zshrc not listed as kept:\n%s", kind, f.out.String())
		}
	}
}

// A file over the size cap is refused, not read whole.
func TestShellSetupRefusesAHugeFile(t *testing.T) {
	r := newShellRig(t, "/bin/zsh")
	if err := os.WriteFile(r.path(".zshrc"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(r.path(".zshrc"), maxDotfile+1); err != nil {
		t.Fatal(err)
	}
	code, out, errs := r.run("--apply")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	wantIn(t, out+errs, r.path(".zshrc"), "larger than")
}
