package shim

import (
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
	return !thisMac(u.Hostname())
}

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

// thisMac reports whether host names this machine.
func thisMac(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified() || ownAddress(ip)
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".local")
	if host == "localhost" || host == "host.docker.internal" {
		return true
	}
	me, err := os.Hostname()
	return err == nil && host == strings.TrimSuffix(strings.ToLower(me), ".local")
}
