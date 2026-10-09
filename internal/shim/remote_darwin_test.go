//go:build darwin

package shim

import (
	"context"
	"testing"
)

// macOS resolves 127.1 and localhost itself, with getaddrinfo, as the
// docker CLI's dialer does: the shim's resolver finds the loopback (#118).
func TestTheResolverFindsTheLoopback(t *testing.T) {
	for _, host := range []string{"127.1", "localhost"} {
		ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
		addrs, err := lookupIP(ctx, host)
		cancel()
		if err != nil {
			t.Errorf("%s: %v", host, err)
			continue
		}
		loopback := false
		for _, a := range addrs {
			loopback = loopback || a.IP.IsLoopback()
		}
		if !loopback {
			t.Errorf("%s resolves to %v, want a loopback address", host, addrs)
		}
	}
	if Remote("docker", "tcp://127.1:2375", func(string) string { return "" }) {
		t.Error("tcp://127.1:2375 is remote, want this Mac's")
	}
}

// The host a call reaches is the one its client dials on this Mac, read
// per scheme: tcp:// as docker's ParseTCPAddr reads it (port 2375 when it
// names none) and dialed through the system resolver; ssh:// as docker's
// connhelper hands it to ssh ("ssh -p PORT -- HOST", HOST being
// url.Hostname) and ssh resolves it. Each row's reach is what docker 29.8.1
// dialed, or what "ssh -G" and "ssh -v" connected to, on macOS 26. A
// number goes to the resolver on either scheme; a name is resolved on
// tcp:// only, so an ssh name stays remote (#118).
func TestTheHostIsReadAsTheClientReadsIt(t *testing.T) {
	for _, c := range []struct {
		endpoint string
		thisMac  bool
		reaches  string
	}{
		{"tcp://127.1:2375", true, "127.0.0.1"},
		{"tcp://127.0.1:2375", true, "127.0.0.1"},
		{"tcp://2130706433:2375", true, "127.0.0.1"},
		{"tcp://0x7f000001:2375", true, "127.0.0.1"},
		{"tcp://0127.0.0.1:2375", true, "127.0.0.1: a dotted quad's leading zeros are decimal"},
		{"tcp://0177.0.0.1:2375", false, "177.0.0.1"},
		{"tcp://6425673729:2375", true, "127.0.0.1: a one-part number wraps"},
		{"tcp://0x17f000001:2375", true, "127.0.0.1"},
		{"tcp://4294967296:2375", true, "0.0.0.0"},
		{"tcp://127.0x.1:2375", true, "127.0.0.1: a bare 0x is 0"},
		{"tcp://0x.0:2375", true, "0.0.0.0"},
		{"tcp://[::1]:2375", true, "::1"},
		{"tcp://::1", true, "[::1]:2375"},
		{"tcp://::1:2375", false, "[::1:2375]:2375, not the loopback"},
		{"tcp://[::1]", true, "nothing: docker rejects it, so it is gated"},
		{"tcp://[::1%25lo0]:2375", true, "nothing: docker rejects the escape, so it is gated"},
		{"tcp://2001:db8::1", false, "[2001:db8::1]:2375"},
		{"tcp://[2001:db8::1]:2376", false, "2001:db8::1"},
		{"tcp://203.0.113.7:2376", false, "203.0.113.7"},
		{"tcp://localhost:2375", true, "the loopback"},
		{"tcp://localhost.:2375", true, "the loopback"},
		{"tcp://:2375", true, "localhost:2375"},
		{"tcp://no-such-box.invalid:2375", true, "nothing: an unresolved name is gated"},

		{"127.1:2375", true, "tcp://127.1:2375"},
		{"::1", true, "tcp://[::1]:2375"},
		{"::1:2375", false, "tcp://[::1:2375]:2375"},
		{"[::1]:2375", true, "tcp://[::1]:2375"},
		{"203.0.113.7:2375", false, "tcp://203.0.113.7:2375"},

		{"ssh://core@::1:22", true, "ssh -p 22 -- ::1"},
		{"ssh://core@::1:2222/run/podman/podman.sock", true, "ssh -p 2222 -- ::1"},
		{"ssh://core@[::1]:22", true, "ssh -p 22 -- ::1"},
		{"ssh://core@[::1%25lo0]:22", true, "ssh -p 22 -- ::1%lo0, which connects to ::1"},
		{"ssh://core@::1", true, "ssh -p 1 -- :, which does not resolve, so it is gated"},
		{"ssh://core@2001:db8::1", true, "ssh -p 1 -- 2001:db8:, which does not resolve, so it is gated"},
		{"ssh://core@[2001:db8::1]:22", false, "2001:db8::1"},
		{"ssh://core@127.1:2222", true, "127.0.0.1"},
		{"ssh://core@0127.0.0.1:2222", true, "127.0.0.1"},
		{"ssh://core@0177.0.0.1:2222", false, "177.0.0.1"},
		{"ssh://core@6425673729:2222", true, "127.0.0.1"},
		{"ssh://core@0x.0:2222", true, "0.0.0.0"},
		{"ssh://core@203.0.113.7:2222", false, "203.0.113.7"},
		{"ssh://core@localhost:2222", true, "the loopback"},
		{"ssh://core@localhost.:2222", true, "the loopback"},
		{"ssh://dev@build-box", false, "build-box, which ssh's config may alias"},
		{"ssh://dev@no-such-box.invalid", false, "a name, not resolved"},
	} {
		if got := Remote("docker", c.endpoint, func(string) string { return "" }); got == c.thisMac {
			t.Errorf("Remote(docker, %q) = %v, want %v: the client reaches %s", c.endpoint, got, !c.thisMac, c.reaches)
		}
	}
}
