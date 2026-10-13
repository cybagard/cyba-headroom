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
		endpoint string
		want     bool
	}{
		{"", false},
		{"desktop-linux", false}, // a context name: local in #28
		{"unix:///var/run/docker.sock", false},
		{"ssh://dev@build-box", true},
		{"tcp://10.0.0.5:2376", true},
		{"tcp://localhost:2375", false},
		{"tcp://0.0.0.0:2375", false},
		{"tcp://host.docker.internal:2375", false},
		{"ssh://me@" + host + ".local", false},
		{"ssh://me@" + strings.ToUpper(host), false},
		{"ssh://core@127.0.0.1:52000/run/podman/podman.sock", false}, // podman machine
		{"tcp://[::1]:2375", false},
		{"tcp://build-box:8080", true},
		{"ssh://", false},
		{"tcp://" + lan + ":2375", false}, // this Mac's own address

		{"10.0.0.5:2375", true}, // docker reads host:port as tcp://
		{"build-box:2375", true},
		{"localhost:2375", false},
		{"::bad", false},
	} {
		if got := Remote(c.endpoint); got != c.want {
			t.Errorf("Remote(%q) = %v, want %v", c.endpoint, got, c.want)
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
// the docker CLI dials it: 127.1, a name for 127.0.0.1; and so is a bare
// ::1 on either scheme, with or without a port (#118).
func TestALoopbackTCPHostIsThisMacs(t *testing.T) {
	resolving(t, map[string]string{"127.1": "127.0.0.1", "dev-box": "127.0.0.1", "dev-box6": "::1"})
	for _, endpoint := range []string{
		"tcp://127.1:2375",
		"127.1:2375",
		"::1",
		"tcp://::1",
		"ssh://dev@::1",
		"ssh://dev@::1:22", // ssh -p 22 -- ::1
		"ssh://core@::1:2222/run/podman/podman.sock",
		"tcp://dev-box:2375",
		"tcp://dev-box6:2375",
	} {
		if Remote(endpoint) {
			t.Errorf("Remote(%q) = true, want this Mac's", endpoint)
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
		if !Remote(endpoint) {
			t.Errorf("Remote(%q) = false, want remote", endpoint)
		}
	}
}

// A tcp host that does not resolve, or not in time, is this Mac's: the
// shim gates it rather than skip a local call, and waits about 200 ms
// for a slow resolver (#118).
func TestAnUnresolvedTCPHostIsThisMacs(t *testing.T) {
	resolving(t, nil)
	if Remote("tcp://no-such-box:2375") {
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
	if Remote("tcp://slow-box:2375") {
		t.Error("a host that resolves too late is remote, want this Mac's")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the shim waited %v for the resolver, want about 200ms", took)
	}
}

// A numeric host is resolved on either scheme, as getaddrinfo reads it, and
// never taken for an ssh alias: 127.1, 127.0.1, 2130706433 and 0x7f000001
// are this Mac's when they resolve to it, and 0177.0.0.1, read as
// 177.0.0.1, stays remote. localhost. is a name, and so is cafe, an ssh
// host that is not resolved (#118).
func TestANumericHostIsResolvedOnEitherScheme(t *testing.T) {
	resolving(t, map[string]string{
		"127.1": "127.0.0.1", "127.0.1": "127.0.0.1", "2130706433": "127.0.0.1",
		"0x7f000001": "127.0.0.1", "0177.0.0.1": "177.0.0.1",
	})
	for _, c := range []struct {
		host   string
		remote bool
	}{
		{"127.1", false},
		{"127.0.1", false},
		{"2130706433", false},
		{"0x7f000001", false},
		{"localhost.", false},
		{"0177.0.0.1", true},
	} {
		for _, endpoint := range []string{"tcp://" + c.host + ":2375", "ssh://core@" + c.host + ":2222"} {
			if got := Remote(endpoint); got != c.remote {
				t.Errorf("Remote(%q) = %v, want %v", endpoint, got, c.remote)
			}
		}
	}
	if !Remote("ssh://dev@cafe") {
		t.Error("ssh://dev@cafe is this Mac's, want remote: a name, not resolved")
	}
}
