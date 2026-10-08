package shim

import "testing"

func TestRemote(t *testing.T) {
	for _, c := range []struct {
		endpoint, dockerHost, containerHost string
		want                                bool
	}{
		{"", "", "", false},
		{"desktop-linux", "", "", false}, // a context name: local in #28
		{"unix:///var/run/docker.sock", "", "", false},
		{"ssh://dev@build-box", "", "", true},
		{"tcp://10.0.0.5:2376", "", "", true},
		{"tcp://localhost:2375", "", "", false},
		{"ssh://core@127.0.0.1:52000/run/podman/podman.sock", "", "", false}, // podman machine
		{"tcp://[::1]:2375", "", "", false},
		{"", "ssh://dev@build-box", "", true},
		{"", "", "tcp://build-box:8080", true},
		{"unix:///x.sock", "ssh://dev@build-box", "", false}, // the flag wins
		{"ssh://", "", "", false},
		{"::bad", "", "", false},
	} {
		env := map[string]string{"DOCKER_HOST": c.dockerHost, "CONTAINER_HOST": c.containerHost}
		if got := Remote(c.endpoint, func(k string) string { return env[k] }); got != c.want {
			t.Errorf("Remote(%q, DOCKER_HOST=%q, CONTAINER_HOST=%q) = %v, want %v", c.endpoint, c.dockerHost, c.containerHost, got, c.want)
		}
	}
}
