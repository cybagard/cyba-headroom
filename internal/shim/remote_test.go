package shim

import (
	"os"
	"strings"
	"testing"
)

func TestRemote(t *testing.T) {
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	for _, c := range []struct {
		name, endpoint, dockerHost, containerHost string
		want                                      bool
	}{
		{"docker", "", "", "", false},
		{"docker", "desktop-linux", "", "", false}, // a context name: local in #28
		{"docker", "unix:///var/run/docker.sock", "", "", false},
		{"docker", "ssh://dev@build-box", "", "", true},
		{"docker", "tcp://10.0.0.5:2376", "", "", true},
		{"docker", "tcp://localhost:2375", "", "", false},
		{"docker", "tcp://0.0.0.0:2375", "", "", false},
		{"docker", "tcp://host.docker.internal:2375", "", "", false},
		{"docker", "ssh://me@" + host + ".local", "", "", false},
		{"docker", "ssh://me@" + strings.ToUpper(host), "", "", false},
		{"podman", "ssh://core@127.0.0.1:52000/run/podman/podman.sock", "", "", false}, // podman machine
		{"docker", "tcp://[::1]:2375", "", "", false},
		{"docker", "", "ssh://dev@build-box", "", true},
		{"podman", "", "", "tcp://build-box:8080", true},
		// Each engine reads its own variable only.
		{"docker", "", "", "ssh://dev@build-box", false},
		{"podman", "", "ssh://dev@build-box", "", false},
		{"docker", "unix:///x.sock", "ssh://dev@build-box", "", false}, // the flag wins
		{"docker", "ssh://", "", "", false},
		{"docker", "::bad", "", "", false},
	} {
		env := map[string]string{"DOCKER_HOST": c.dockerHost, "CONTAINER_HOST": c.containerHost}
		if got := Remote(c.name, c.endpoint, func(k string) string { return env[k] }); got != c.want {
			t.Errorf("Remote(%s, %q, DOCKER_HOST=%q, CONTAINER_HOST=%q) = %v, want %v",
				c.name, c.endpoint, c.dockerHost, c.containerHost, got, c.want)
		}
	}
}
