package shim

import (
	"context"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// Remote reports whether a container call goes to an engine on another
// machine, whose memory is not this Mac's: a tcp:// or ssh:// endpoint on
// a host other than this one. The endpoint is the call's own
// (the last of Call.Host and Call.Context), else the engine's variable: DOCKER_HOST for docker,
// CONTAINER_HOST for podman. Named contexts and connections count as local,
// and so do podman machine's ssh://…@127.0.0.1 and any host thisMac finds
// here. The host is the one the client dials (see dialed).
func Remote(name, endpoint string, getenv func(string) string) bool {
	if endpoint == "" {
		endpoint = getenv(map[string]string{"docker": "DOCKER_HOST", "podman": "CONTAINER_HOST"}[name])
	}
	if endpoint != "" && !strings.Contains(endpoint, "://") && strings.Contains(endpoint, ":") {
		endpoint = "tcp://" + endpoint // docker reads a bare host:port as TCP
	}
	host, tcp, ok := dialed(endpoint)
	return ok && !thisMac(host, tcp)
}

// dialed is the host the client dials for endpoint, read per scheme as the
// docker CLI reads it. A tcp:// host is split from its port as ParseTCPAddr
// in docker/cli v29.0.0's opts/hosts.go splits it, with port 2375 when it
// names none, so an unbracketed ::1 is the host ::1 and ::1:2375 the host
// ::1:2375; an empty one is localhost. An ssh:// host is url.Hostname, which
// the CLI hands to ssh as "ssh -p PORT -- HOST" (getConnectionHelper in
// cli/connhelper/connhelper.go, and Spec in its ssh package): ssh://u@::1:22
// is ::1 on port 22. ok is false for any other scheme, or a host the CLI
// cannot read.
func dialed(endpoint string) (host string, tcp, ok bool) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false, false
	}
	switch u.Scheme {
	case "tcp":
		host, _, err := net.SplitHostPort(u.Host)
		if err != nil {
			host, _, err = net.SplitHostPort(net.JoinHostPort(u.Host, "2375"))
		}
		if err != nil {
			return "", false, false
		}
		if host == "" {
			host = "localhost"
		}
		return host, true, true
	case "ssh":
		return u.Hostname(), false, u.Hostname() != ""
	}
	return "", false, false
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

// thisMac reports whether host, as the client dials it (see dialed), is
// this machine. The rule, on either scheme:
//
//  1. An address is this Mac's when it is a loopback, unspecified or own
//     address (see ownIP).
//  2. A number (see numeric) goes to the system resolver, which reads it as
//     getaddrinfo and ssh do.
//  3. localhost, host.docker.internal and this Mac's hostname are this
//     Mac's, with or without a trailing dot or .local.
//  4. Any other name goes to the resolver on tcp only, as the docker CLI
//     dials it: with a net.Dialer (ConfigureTransport in
//     docker/go-connections v0.6.0's sockets/sockets.go), so Go's resolver,
//     which on macOS is getaddrinfo even without cgo. On ssh it is remote:
//     the ssh binary's config may alias it.
//
// A host the resolver does not answer within resolveTimeout is this Mac's.
func thisMac(host string, tcp bool) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ownIP(ip)
	}
	if numeric(host) {
		return resolvesHere(host)
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
	return resolvesHere(host)
}

// resolvesHere reports whether host resolves to this machine, or does not
// resolve within resolveTimeout.
func resolvesHere(host string) bool {
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

// numeric reports whether host is an address rather than a name: an IPv6
// address, which has a colon, or what inet_aton may read, digits, dots and
// hex, starting with a digit and not ending with a dot (0127.0.0.1,
// 6425673729, 127.0x.1). On macOS the resolver parses these itself, with
// getaddrinfo, without DNS.
func numeric(host string) bool {
	if strings.Contains(host, ":") {
		return true
	}
	if host == "" || host[0] < '0' || host[0] > '9' || strings.HasSuffix(host, ".") {
		return false
	}
	return strings.Trim(strings.ToLower(host), "0123456789abcdefx.") == ""
}
