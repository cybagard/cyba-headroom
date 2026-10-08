package shim

import (
	"net"
	"net/url"
)

// Remote reports whether a container call goes to an engine on another
// machine, whose memory is not this Mac's: a tcp:// or ssh:// endpoint on a
// host other than this one. The endpoint is the call's own (Call.Endpoint),
// else DOCKER_HOST, else CONTAINER_HOST. Named contexts and connections
// count as local. Podman machine's ssh://…@127.0.0.1 is local.
func Remote(endpoint string, getenv func(string) string) bool {
	for _, e := range []string{endpoint, getenv("DOCKER_HOST"), getenv("CONTAINER_HOST")} {
		if e == "" {
			continue
		}
		u, err := url.Parse(e)
		if err != nil || (u.Scheme != "tcp" && u.Scheme != "ssh") {
			return false
		}
		h := u.Hostname()
		if ip := net.ParseIP(h); h == "" || h == "localhost" || (ip != nil && ip.IsLoopback()) {
			return false
		}
		return true
	}
	return false
}
