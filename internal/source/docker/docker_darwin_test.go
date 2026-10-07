//go:build darwin

package docker_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

	d := collect(t, docker.New(sock, vmproc.New(vmproc.Host{}), time.Now))
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
