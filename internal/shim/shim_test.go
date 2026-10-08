package shim_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/shim"
)

// fixture is a fake headroom binary and a few PATH dirs.
type fixture struct {
	t    testing.TB
	root string
	self string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, root: root, self: filepath.Join(root, "bin", "headroom")}
	f.file("bin/headroom", "#!headroom\n", 0o755)
	return f
}

func (f *fixture) file(rel, body string, mode os.FileMode) string {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) link(rel, target string) string {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) dir(rel string) string { return filepath.Join(f.root, rel) }

func (f *fixture) env(path ...string) func(string) string {
	return func(k string) string {
		switch k {
		case "PATH":
			return strings.Join(path, string(os.PathListSeparator))
		case "HOME":
			return f.dir("home")
		}
		return ""
	}
}

func (f *fixture) resolve(name string, path ...string) (shim.Target, error) {
	return shim.Resolve(name, []string{f.self}, f.env(path...), nil)
}

func TestFindsTheRealBinaryAfterTheShim(t *testing.T) {
	f := newFixture(t)
	f.link("shims/docker", f.self)
	realBin := f.file("usr/bin/docker", "#!real\n", 0o755)
	f.file("later/docker", "#!later\n", 0o755)
	got, err := f.resolve("docker", f.dir("shims"), f.dir("usr/bin"), f.dir("later"))
	if err != nil || got.Path != realBin || shim.Engine("docker", got.Path) != "docker" {
		t.Fatalf("got %+v, %v; want %s", got, err, realBin)
	}
}

func TestNeverItself(t *testing.T) {
	f := newFixture(t)
	f.link("shims/docker", f.self)
	f.link("shims2/docker", f.dir("shims/docker")) // a chain
	if err := os.MkdirAll(f.dir("hard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(f.self, f.dir("hard/docker")); err != nil { // a hard link
		t.Fatal(err)
	}
	realBin := f.file("usr/bin/docker", "#!real\n", 0o755)
	got, err := f.resolve("docker", f.dir("shims"), f.dir("shims2"), f.dir("hard"), f.dir("usr/bin"))
	if err != nil || got.Path != realBin {
		t.Fatalf("got %+v, %v; want %s", got, err, realBin)
	}
}

func TestSkipsUnsafeAndUnusableEntries(t *testing.T) {
	f := newFixture(t)
	f.file("noexec/docker", "#!not executable\n", 0o644)
	f.file("isdir/docker/.keep", "", 0o644) // docker is a directory here
	realBin := f.file("usr/bin/docker", "#!real\n", 0o755)
	t.Chdir(f.dir("noexec"))
	f.file("noexec/rel/docker", "#!relative\n", 0o755)
	got, err := f.resolve("docker", "", ".", "rel", f.dir("noexec"), f.dir("isdir"), f.dir("usr/bin"))
	if err != nil || got.Path != realBin {
		t.Fatalf("got %+v, %v; want %s", got, err, realBin)
	}
}

func TestFallsBackToUsualLocations(t *testing.T) {
	f := newFixture(t)
	tart := f.file("home/.local/bin/tart", "#!tart\n", 0o755)
	got, err := shim.Resolve("tart", []string{f.self}, f.env(f.dir("empty")), []string{"~/.local/bin/tart"})
	if err != nil || got.Path != tart || shim.Engine("tart", got.Path) != "tart" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestNotFound(t *testing.T) {
	f := newFixture(t)
	f.link("shims/docker", f.self)
	_, err := f.resolve("docker", f.dir("shims"))
	if !errors.Is(err, shim.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestDockerThatIsPodman(t *testing.T) {
	f := newFixture(t)
	podman := f.file("opt/podman/bin/podman", "#!podman\n", 0o755)
	f.link("viaLink/docker", podman)
	f.file("viaScript/docker", "#!/bin/sh\n[ -e /etc/containers/nodocker ] || echo emulate\nexec /opt/podman/bin/podman \"$@\"\n", 0o755)
	for _, dir := range []string{"viaLink", "viaScript"} {
		got, err := f.resolve("docker", f.dir(dir))
		if err != nil || shim.Engine("docker", got.Path) != "podman" {
			t.Errorf("%s: got %+v, %v; want engine podman", dir, got, err)
		}
	}
	if got, _ := f.resolve("podman", f.dir("opt/podman/bin")); shim.Engine("podman", got.Path) != "podman" {
		t.Errorf("podman itself: %+v", got)
	}
	// A wrapper that only mentions podman is docker.
	w := f.file("wrap/docker", "#!/bin/sh\n# not podman: force the desktop context\nexec /usr/local/bin/docker.real \"$@\"\n", 0o755)
	if e := shim.Engine("docker", w); e != "docker" {
		t.Errorf("wrapper mentioning podman: engine %s", e)
	}
}

func BenchmarkResolve(b *testing.B) {
	f := &fixture{t: b, root: b.TempDir()}
	f.self = f.file("bin/headroom", "#!headroom\n", 0o755)
	f.link("shims/docker", f.self)
	f.file("usr/bin/docker", "#!real\n", 0o755)
	env := f.env(f.dir("shims"), f.dir("a"), f.dir("b"), f.dir("usr/bin"))
	for b.Loop() {
		if _, err := shim.Resolve("docker", []string{f.self}, env, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSkipsEveryHeadroomOnTheWay(t *testing.T) {
	// Two headroom builds, each with a shim dir on PATH: B, exec'd by A's
	// shim, must not exec A back.
	f := newFixture(t)
	other := f.file("other/headroom", "#!another headroom build\n", 0o755)
	f.link("shimsA/docker", f.self)
	f.link("shimsB/docker", other)
	realBin := f.file("usr/bin/docker", "#!real\n", 0o755)
	got, err := shim.Resolve("docker", []string{other, f.self}, f.env(f.dir("shimsA"), f.dir("shimsB"), f.dir("usr/bin")), nil)
	if err != nil || got.Path != realBin {
		t.Fatalf("got %+v, %v; want %s", got, err, realBin)
	}
}

func TestSkipsWhatThisUserCannotRun(t *testing.T) {
	f := newFixture(t)
	f.file("others/docker", "#!only for group and others\n", 0o011)
	realBin := f.file("usr/bin/docker", "#!real\n", 0o755)
	got, err := f.resolve("docker", f.dir("others"), f.dir("usr/bin"))
	if err != nil || got.Path != realBin {
		t.Fatalf("got %+v, %v; want %s", got, err, realBin)
	}
}
