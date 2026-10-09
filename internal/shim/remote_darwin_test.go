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
