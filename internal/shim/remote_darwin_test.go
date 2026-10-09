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

// A numeric host is read by the system resolver on either scheme, as
// getaddrinfo, docker and ssh read it: a dotted quad's leading zeros are
// decimal, a one-part number wraps, a bare 0x is 0 and a zone is dropped
// (#118).
func TestANumericHostIsReadAsMacOSReadsIt(t *testing.T) {
	for _, c := range []struct {
		host    string
		thisMac bool
	}{
		{"0127.0.0.1", true},
		{"0177.0.0.1", false}, // 177.0.0.1
		{"6425673729", true},
		{"0x17f000001", true},
		{"4294967296", true}, // 0.0.0.0
		{"127.0x.1", true},
		{"0x.0", true},
		{"[::1%25lo0]", true},
	} {
		for _, endpoint := range []string{"tcp://" + c.host + ":2375", "ssh://core@" + c.host + ":2222"} {
			if got := Remote("docker", endpoint, func(string) string { return "" }); got == c.thisMac {
				t.Errorf("Remote(docker, %q) = %v, want %v", endpoint, got, !c.thisMac)
			}
		}
	}
}
