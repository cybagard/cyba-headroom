package docker_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	want := []docker.Candidate{{cfg, true}, {host, true}, {colima, false}, {desktop, true}}
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
	if fmt.Sprint(got) != fmt.Sprint([]docker.Candidate{{colima, false}}) {
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
	if fmt.Sprint(got) != fmt.Sprint([]docker.Candidate{{link, true}}) {
		t.Fatalf("candidates = %v, want only %v", got, link)
	}
	// A context first, then Docker Desktop's socket: one candidate, trusted.
	got = docker.Candidates("", env(map[string]string{"HOME": home}), quiet)
	if fmt.Sprint(got) != fmt.Sprint([]docker.Candidate{{desktop, true}}) {
		t.Fatalf("candidates = %v, want only %v, trusted", got, desktop)
	}
	got = docker.Candidates(link, env(map[string]string{"HOME": home, "DOCKER_HOST": "unix://" + desktop}), quiet)
	if fmt.Sprint(got) != fmt.Sprint([]docker.Candidate{{link, true}}) {
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
			if fmt.Sprint(got) != fmt.Sprint([]docker.Candidate{{colima, false}}) {
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
