package shim

import (
	"context"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Remote reports whether a container call goes to an engine on another
// machine, whose memory is not this Mac's: a tcp:// or ssh:// endpoint on
// a host other than this one. The endpoint is the call's own
// (the last of Call.Host and Call.Context), else the engine's variable: DOCKER_HOST for docker,
// CONTAINER_HOST for podman. Named contexts and connections count as local,
// and so do podman machine's ssh://…@127.0.0.1, this Mac's own names and
// a tcp host that resolves to this Mac (see thisMac).
func Remote(name, endpoint string, getenv func(string) string) bool {
	if endpoint == "" {
		endpoint = getenv(map[string]string{"docker": "DOCKER_HOST", "podman": "CONTAINER_HOST"}[name])
	}
	if endpoint != "" && !strings.Contains(endpoint, "://") && strings.Contains(endpoint, ":") {
		endpoint = "tcp://" + endpoint // docker reads a bare host:port as TCP
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "tcp" && u.Scheme != "ssh") || u.Hostname() == "" {
		return false
	}
	host := u.Hostname()
	if ip := net.ParseIP(u.Host); ip != nil {
		// An unbracketed IPv6 address, which Hostname splits: the docker CLI
		// reads -H ::1 as tcp://[::1]:2375 (ParseTCPAddr in docker/cli
		// v29.0.0's opts/hosts.go).
		host = u.Host
	}
	return !thisMac(host, u.Scheme == "tcp")
}

// lookupIP resolves a host name; a test replaces it.
var lookupIP = net.DefaultResolver.LookupIPAddr

// resolveTimeout bounds how long a call waits for lookupIP.
const resolveTimeout = 200 * time.Millisecond

// ownAddress reports whether ip is one of this machine's interface addresses.
func ownAddress(ip net.IP) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// ownIP reports whether ip is this machine's.
func ownIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsUnspecified() || ownAddress(ip)
}

// thisMac reports whether host names this machine. A tcp host is resolved
// as the docker CLI dials it: with a net.Dialer (ConfigureTransport in
// docker/go-connections v0.6.0's sockets/sockets.go), so Go's resolver,
// which on macOS is getaddrinfo even without cgo and reads 127.1 as
// 127.0.0.1. One that does not resolve within resolveTimeout is this Mac's.
// An ssh host is not resolved: the CLI hands it to the ssh binary
// (getConnectionHelper in docker/cli v29.0.0's cli/connhelper/connhelper.go),
// whose config may alias it. A number is a number on either scheme, read
// as getaddrinfo reads it (see inetAton), and a trailing dot ends a name.
func thisMac(host string, tcp bool) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		ip = inetAton(host)
	}
	if ip != nil {
		return ownIP(ip)
	}
	name := strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(host), "."), ".local")
	if name == "localhost" || name == "host.docker.internal" {
		return true
	}
	if me, err := os.Hostname(); err == nil && name == strings.TrimSuffix(strings.ToLower(me), ".local") {
		return true
	}
	if !tcp {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	addrs, err := lookupIP(ctx, host)
	if err != nil {
		return true // gated, rather than a local call skipped as remote
	}
	for _, a := range addrs {
		if ownIP(a.IP) {
			return true
		}
	}
	return false
}

// inetAton reads s as inet_aton(3) does, which getaddrinfo and ssh use for
// a numeric host: one to four parts, each decimal, 0-prefixed octal or
// 0x-prefixed hex, the last filling the remaining bytes (127.1 is
// 127.0.0.1, 2130706433 is too). It returns nil for anything else.
func inetAton(s string) net.IP {
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return nil
	}
	var v uint64
	for i, p := range parts {
		base := 10
		if len(p) > 2 && (p[:2] == "0x" || p[:2] == "0X") {
			p, base = p[2:], 16
		} else if len(p) > 1 && p[0] == '0' {
			p, base = p[1:], 8
		}
		n, err := strconv.ParseUint(p, base, 32)
		if err != nil {
			return nil
		}
		bits := uint(8)
		if i == len(parts)-1 {
			bits = 8 * uint(5-len(parts))
		}
		if n >= 1<<bits {
			return nil
		}
		v = v<<bits | n
	}
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
