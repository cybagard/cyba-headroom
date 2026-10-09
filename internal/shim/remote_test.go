package shim

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRemote(t *testing.T) {
	resolving(t, map[string]string{"build-box": "203.0.113.7"})
	host, _ := os.Hostname()
	lan := ""
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
				lan = ipn.IP.String()
			}
		}
	}
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
		{"docker", "tcp://" + lan + ":2375", "", "", false}, // this Mac's own address

		{"docker", "10.0.0.5:2375", "", "", true}, // docker reads host:port as tcp://
		{"docker", "", "build-box:2375", "", true},
		{"docker", "localhost:2375", "", "", false},
		{"docker", "::bad", "", "", false},
	} {
		env := map[string]string{"DOCKER_HOST": c.dockerHost, "CONTAINER_HOST": c.containerHost}
		if got := Remote(c.name, c.endpoint, func(k string) string { return env[k] }); got != c.want {
			t.Errorf("Remote(%s, %q, DOCKER_HOST=%q, CONTAINER_HOST=%q) = %v, want %v",
				c.name, c.endpoint, c.dockerHost, c.containerHost, got, c.want)
		}
	}
}

// resolving makes lookupIP answer from names for the test, and fail for
// any other name.
func resolving(t *testing.T, names map[string]string) {
	t.Helper()
	old := lookupIP
	t.Cleanup(func() { lookupIP = old })
	lookupIP = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if ip, ok := names[host]; ok {
			return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
}

// A tcp:// host is this Mac's when it resolves to one of its addresses, as
// the docker CLI dials it: 127.1, a bare ::1, a name for 127.0.0.1 (#118).
func TestALoopbackTCPHostIsThisMacs(t *testing.T) {
	resolving(t, map[string]string{"127.1": "127.0.0.1", "dev-box": "127.0.0.1", "dev-box6": "::1"})
	for _, c := range []struct{ endpoint, dockerHost string }{
		{"tcp://127.1:2375", ""},
		{"127.1:2375", ""},
		{"", "tcp://127.1:2375"},
		{"::1", ""},
		{"tcp://::1", ""},
		{"", "::1"},
		{"ssh://dev@::1", ""},
		{"tcp://dev-box:2375", ""},
		{"tcp://dev-box6:2375", ""},
	} {
		env := map[string]string{"DOCKER_HOST": c.dockerHost}
		if Remote("docker", c.endpoint, func(k string) string { return env[k] }) {
			t.Errorf("Remote(docker, %q, DOCKER_HOST=%q) = true, want this Mac's", c.endpoint, c.dockerHost)
		}
	}
}

// A tcp host that resolves only to another machine stays remote, and so
// does an ssh host, which is not resolved (#118).
func TestARemoteTCPHostStaysRemote(t *testing.T) {
	resolving(t, map[string]string{"build-box": "203.0.113.7"})
	for _, endpoint := range []string{
		"tcp://build-box:2376",
		"build-box:2376",
		"tcp://203.0.113.7:2376",
		"tcp://[2001:db8::1]:2376",
		"2001:db8::1",
		"ssh://dev@ssh-alias",
	} {
		if !Remote("docker", endpoint, func(string) string { return "" }) {
			t.Errorf("Remote(docker, %q) = false, want remote", endpoint)
		}
	}
}

// A tcp host that does not resolve, or not in time, is this Mac's: the
// shim gates it rather than skip a local call, and waits about 200 ms
// for a slow resolver (#118).
func TestAnUnresolvedTCPHostIsThisMacs(t *testing.T) {
	resolving(t, nil)
	if Remote("docker", "tcp://no-such-box:2375", func(string) string { return "" }) {
		t.Error("an unresolved host is remote, want this Mac's")
	}
	lookupIP = func(ctx context.Context, _ string) ([]net.IPAddr, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return []net.IPAddr{{IP: net.ParseIP("203.0.113.7")}}, nil
		}
	}
	start := time.Now()
	if Remote("docker", "tcp://slow-box:2375", func(string) string { return "" }) {
		t.Error("a host that resolves too late is remote, want this Mac's")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the shim waited %v for the resolver, want about 200ms", took)
	}
}
