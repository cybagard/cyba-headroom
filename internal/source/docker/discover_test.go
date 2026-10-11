package docker_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/source/docker"
)

// listen makes a Unix socket at path, as an engine would.
func listen(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return path
}

// dockerContext writes a Docker context named name whose endpoint is host,
// as the docker CLI does.
func dockerContext(t *testing.T, home, name, host string) {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	dir := filepath.Join(home, ".docker", "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"Name":%q,"Metadata":{},"Endpoints":{"docker":{"Host":%q,"SkipTLSVerify":false}}}`, name, host)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// The candidates come in order: the configured socket, DOCKER_HOST, each
// context's endpoint, Docker Desktop's socket. Only a context's is not
// trusted: anything may write it.
func TestCandidatesInOrder(t *testing.T) {
	home := shortDir(t)
	desktop := listen(t, filepath.Join(home, ".docker", "run", "docker.sock"))
	cfg := listen(t, filepath.Join(home, "cfg.sock"))
	host := listen(t, filepath.Join(home, "host.sock"))
	colima := listen(t, filepath.Join(home, "colima.sock"))
	dockerContext(t, home, "colima", "unix://"+colima)

	got := docker.Candidates(cfg, env(map[string]string{"HOME": home, "DOCKER_HOST": "unix://" + host}), quiet)
	want := []docker.Candidate{{Socket: cfg, Trusted: true}, {Socket: host, Trusted: true}, {Socket: colima}, {Socket: desktop, Trusted: true}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

// A path that does not exist is no candidate; nor is a TCP DOCKER_HOST or
// context, whose memory is not this Mac's.
func TestCandidatesSkipMissingAndRemote(t *testing.T) {
	home := shortDir(t)
	colima := listen(t, filepath.Join(home, "colima.sock"))
	dockerContext(t, home, "gone", "unix://"+filepath.Join(home, "gone.sock"))
	dockerContext(t, home, "remote", "tcp://10.0.0.1:2376")
	dockerContext(t, home, "colima", "unix://"+colima)

	got := docker.Candidates(filepath.Join(home, "none.sock"), env(map[string]string{"HOME": home, "DOCKER_HOST": "tcp://10.0.0.1:2375"}), quiet)
	if fmt.Sprint(got) != fmt.Sprint([]docker.Candidate{{Socket: colima}}) {
		t.Fatalf("candidates = %v, want only %v (Docker Desktop's socket is missing too)", got, colima)
	}
}

// A socket named twice, directly or through a link, is one candidate: the
// first name of it, trusted if any name of it is (Docker Desktop's own
// context names its socket).
func TestCandidatesNameASocketOnce(t *testing.T) {
	home := shortDir(t)
	desktop := listen(t, filepath.Join(home, ".docker", "run", "docker.sock"))
	link := filepath.Join(home, "docker.sock")
	if err := os.Symlink(desktop, link); err != nil {
		t.Fatal(err)
	}
	dockerContext(t, home, "desktop-linux", "unix://"+desktop)

	got := docker.Candidates(link, env(map[string]string{"HOME": home, "DOCKER_HOST": "unix://" + desktop}), quiet)
	if fmt.Sprint(got) != fmt.Sprint([]docker.Candidate{{Socket: link, Trusted: true}}) {
		t.Fatalf("candidates = %v, want only %v", got, link)
	}
	// A context first, then Docker Desktop's socket: one candidate, trusted.
	got = docker.Candidates("", env(map[string]string{"HOME": home}), quiet)
	if fmt.Sprint(got) != fmt.Sprint([]docker.Candidate{{Socket: desktop, Trusted: true}}) {
		t.Fatalf("candidates = %v, want only %v, trusted", got, desktop)
	}
	got = docker.Candidates(link, env(map[string]string{"HOME": home, "DOCKER_HOST": "unix://" + desktop}), quiet)
	if fmt.Sprint(got) != fmt.Sprint([]docker.Candidate{{Socket: link, Trusted: true}}) {
		t.Fatalf("candidates = %v, want only %v", got, link)
	}
}

// With no HOME, there are no contexts and no Docker Desktop socket to find.
func TestCandidatesWithoutHome(t *testing.T) {
	if got := docker.Candidates("", env(nil), quiet); len(got) != 0 {
		t.Fatalf("candidates = %v, want none", got)
	}
}

// contextMeta makes the meta.json of a Docker context named name, with
// create, as an agent could: a context's files are anyone's to write.
func contextMeta(t *testing.T, home, name string, create func(path string) error) {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	dir := filepath.Join(home, ".docker", "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := create(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatal(err)
	}
}

// candidatesWithin returns Candidates, or fails if it takes more than
// limit: discovery runs every tick, before any deadline.
func candidatesWithin(t *testing.T, limit time.Duration, config string, getenv func(string) string) []docker.Candidate {
	t.Helper()
	done := make(chan []docker.Candidate, 1)
	go func() { done <- docker.Candidates(config, getenv, quiet) }()
	select {
	case got := <-done:
		return got
	case <-time.After(limit):
		t.Fatalf("Candidates still running after %v", limit)
		return nil
	}
}

// A context's meta.json that is no regular file is skipped at once: a FIFO
// (whose open waits for a writer), a device (/dev/zero never ends), a
// directory. The contexts beside it are read as ever (#227, review round 3).
func TestAContextFileThatIsNotRegularIsSkipped(t *testing.T) {
	for name, create := range map[string]func(string) error{
		"fifo":      func(p string) error { return syscall.Mkfifo(p, 0o644) },
		"dev zero":  func(p string) error { return os.Symlink("/dev/zero", p) },
		"directory": func(p string) error { return os.Mkdir(p, 0o755) },
	} {
		t.Run(name, func(t *testing.T) {
			home := shortDir(t)
			colima := listen(t, filepath.Join(home, "colima.sock"))
			dockerContext(t, home, "colima", "unix://"+colima)
			contextMeta(t, home, "bad", create)
			got := candidatesWithin(t, 300*time.Millisecond, "", env(map[string]string{"HOME": home}))
			if fmt.Sprint(got) != fmt.Sprint([]docker.Candidate{{Socket: colima}}) {
				t.Fatalf("candidates = %v, want only %v", got, colima)
			}
		})
	}
}

// A context's meta.json is read up to 64 KiB: a larger one is skipped,
// whatever it names. The docker CLI's are a few hundred bytes.
func TestAContextFileIsReadWithABound(t *testing.T) {
	home := shortDir(t)
	big := listen(t, filepath.Join(home, "big.sock"))
	contextMeta(t, home, "big", func(p string) error {
		body := `{"Name":"big","Endpoints":{"docker":{"Host":"unix://` + big + `"}}}` + strings.Repeat(" ", 1<<20)
		return os.WriteFile(p, []byte(body), 0o644)
	})
	got := candidatesWithin(t, time.Second, "", env(map[string]string{"HOME": home}))
	if len(got) != 0 {
		t.Fatalf("candidates = %v, want none: the context's file is over the bound", got)
	}
}

// quiet is a logger that drops what it is given.
var quiet = slog.New(slog.DiscardHandler)

// warns records the warnings a logger is given.
type warns struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (w *warns) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }
func (w *warns) Handle(_ context.Context, r slog.Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recs = append(w.recs, r)
	return nil
}
func (w *warns) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warns) WithGroup(string) slog.Handler      { return w }

// skipped returns the skipped count of each warning so far.
func (w *warns) skipped() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []int64
	for _, r := range w.recs {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "skipped" {
				out = append(out, a.Value.Int64())
			}
			return true
		})
	}
	return out
}

// At most 32 Docker contexts are read per tick, the first by their
// directory's name, so the choice is stable: 20,000 of them took 2 s to
// read (#236). The skip is warned of once, and again only when the count
// of those skipped changes.
func TestAtMost32ContextsAreRead(t *testing.T) {
	home := shortDir(t)
	dirs := map[string]string{} // context dir name to its socket
	for i := range 40 {
		sock := listen(t, filepath.Join(home, fmt.Sprintf("%02d.sock", i)))
		name := fmt.Sprintf("context-%02d", i)
		dockerContext(t, home, name, "unix://"+sock)
		sum := sha256.Sum256([]byte(name))
		dirs[hex.EncodeToString(sum[:])] = sock
	}
	first := func() []docker.Candidate {
		names := slices.Sorted(func(yield func(string) bool) {
			for n := range dirs {
				if !yield(n) {
					return
				}
			}
		})
		var want []docker.Candidate
		for _, n := range names[:min(32, len(names))] {
			want = append(want, docker.Candidate{Socket: dirs[n]})
		}
		return want
	}
	w := &warns{}
	d := docker.NewDiscovery(slog.New(w))
	getenv := env(map[string]string{"HOME": home})
	for range 3 {
		if got, want := d.Candidates("", getenv), first(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("candidates = %v, want the first 32 by name: %v", got, want)
		}
	}
	if got := w.skipped(); fmt.Sprint(got) != "[8]" {
		t.Fatalf("warnings of skipped contexts = %v over 3 ticks, want one, of 8", got)
	}

	// Four fewer: warned of again, once.
	n := 0
	for name := range dirs {
		if n == 4 {
			break
		}
		if err := os.RemoveAll(filepath.Join(home, ".docker", "contexts", "meta", name)); err != nil {
			t.Fatal(err)
		}
		delete(dirs, name)
		n++
	}
	for range 3 {
		if got, want := d.Candidates("", getenv), first(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("candidates = %v, want the first 32 by name: %v", got, want)
		}
	}
	if got := w.skipped(); fmt.Sprint(got) != "[8 4]" {
		t.Fatalf("warnings of skipped contexts = %v, want one of 8, then one of 4", got)
	}
}
