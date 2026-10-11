//go:build darwin

package docker_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/source/docker"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

func TestRealDockerDesktop(t *testing.T) {
	sock := filepath.Join(os.Getenv("HOME"), ".docker", "run", "docker.sock")
	cli, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker CLI not installed")
	}
	// The docker CLI is the independent oracle; it goes through the same socket.
	out, err := exec.Command(cli, "info", "--format", "{{.MemTotal}}").Output()
	if err != nil {
		t.Skip("Docker Desktop not running")
	}
	limit, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	names, _ := exec.Command(cli, "ps", "--format", "{{.Names}}").Output()

	d := collect(t, docker.New(sock, vmproc.New(vmproc.Host{})))
	if !d.Running || d.VMLimitBytes != limit {
		t.Fatalf("running=%v limit=%d, docker info says %d", d.Running, d.VMLimitBytes, limit)
	}
	want := strings.Fields(string(names))
	if len(d.Containers) != len(want) {
		t.Fatalf("%d containers, docker ps lists %v", len(d.Containers), want)
	}
	if len(want) > 0 && (!d.VMRunning || d.VMFootprintBytes == 0) {
		t.Fatalf("containers running but VM running=%v footprint=%d", d.VMRunning, d.VMFootprintBytes)
	}
}

// A context naming a file that is no socket (ENOTSOCK on macOS) is an
// engine not running, and the engine after it is read as ever (#227,
// amendment 3).
func TestAContextNamingAFileIsNotRunning(t *testing.T) {
	file := filepath.Join(shortDir(t), "hosts")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err := collectWithin(t, docker.Candidate{Socket: file}, docker.Candidate{Socket: runningEngine(t), Trusted: true})
	if err != nil {
		t.Fatalf("Collect: %v, want a reading", err)
	}
	if len(got) != 2 || got[0].Socket != file || got[0].Running || !got[1].Running || len(got[1].Containers) != 1 {
		t.Fatalf("engines = %+v, want the file's not running, then the engine with its container", got)
	}
}

// A trusted socket that fails other than by being absent or refusing still
// fails the reading: the last good one stands.
func TestATrustedNonSocketFailsTheReading(t *testing.T) {
	file := filepath.Join(shortDir(t), "docker.sock")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	src := docker.Discover(func() []docker.Candidate { return []docker.Candidate{{Socket: file, Trusted: true}} }, fakeVMs{})
	if _, err := src.Collect(context.Background()); err == nil {
		t.Fatal("Collect: want an error for a configured socket that is no socket")
	}
}
