package shim

import (
	"context"
	"net"
	"net/url"
	"os"
	"strings"
)

// Remote reports whether a container call goes to an engine on another
// machine, whose memory is not this Mac's: a tcp:// or ssh:// endpoint on
// a host other than this one. The endpoint is the call's own
// (the last of Call.Host and Call.Context), else the engine's variable: DOCKER_HOST for docker,
// CONTAINER_HOST for podman. Named contexts and connections count as local,
// and so do podman machine's ssh://…@127.0.0.1 and this Mac's own names.
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
		host = u.Host // an unbracketed IPv6 address, which Hostname splits
	}
	return !thisMac(host, u.Scheme == "tcp")
}

// lookupIP resolves a host name; a test replaces it.
var lookupIP = net.DefaultResolver.LookupIPAddr

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
// as the docker CLI dials it, with Go's resolver: getaddrinfo on macOS,
// which reads 127.1 as 127.0.0.1. An ssh host is not: the CLI hands it to
// the ssh binary, whose config may alias it.
func thisMac(host string, tcp bool) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ownIP(ip)
	}
	name := strings.TrimSuffix(strings.ToLower(host), ".local")
	if name == "localhost" || name == "host.docker.internal" {
		return true
	}
	if me, err := os.Hostname(); err == nil && name == strings.TrimSuffix(strings.ToLower(me), ".local") {
		return true
	}
	if !tcp {
		return false
	}
	addrs, err := lookupIP(context.Background(), host)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ownIP(a.IP) {
			return true
		}
	}
	return false
}
