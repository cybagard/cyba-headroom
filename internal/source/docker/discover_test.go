package docker_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

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

	got := docker.Candidates(cfg, env(map[string]string{"HOME": home, "DOCKER_HOST": "unix://" + host}))
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

	got := docker.Candidates(filepath.Join(home, "none.sock"), env(map[string]string{"HOME": home, "DOCKER_HOST": "tcp://10.0.0.1:2375"}))
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

	got := docker.Candidates(link, env(map[string]string{"HOME": home, "DOCKER_HOST": "unix://" + desktop}))
	if fmt.Sprint(got) != fmt.Sprint([]docker.Candidate{{link, true}}) {
		t.Fatalf("candidates = %v, want only %v", got, link)
	}
	// A context first, then Docker Desktop's socket: one candidate, trusted.
	got = docker.Candidates("", env(map[string]string{"HOME": home}))
	if fmt.Sprint(got) != fmt.Sprint([]docker.Candidate{{desktop, true}}) {
		t.Fatalf("candidates = %v, want only %v, trusted", got, desktop)
	}
	got = docker.Candidates(link, env(map[string]string{"HOME": home, "DOCKER_HOST": "unix://" + desktop}))
	if fmt.Sprint(got) != fmt.Sprint([]docker.Candidate{{link, true}}) {
		t.Fatalf("candidates = %v, want only %v", got, link)
	}
}

// With no HOME, there are no contexts and no Docker Desktop socket to find.
func TestCandidatesWithoutHome(t *testing.T) {
	if got := docker.Candidates("", env(nil)); len(got) != 0 {
		t.Fatalf("candidates = %v, want none", got)
	}
}
