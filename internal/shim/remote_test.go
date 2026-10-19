package shim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemote(t *testing.T) {
	resolving(t, map[string]string{"build-box": "203.0.113.7"})
	sshConfigs(t, nil)
	host, _ := os.Hostname()
	lan := lanAddress()
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
		if got := Remote("docker", c.endpoint, os.Getenv); got != c.want {
			t.Errorf("Remote(%q) = %v, want %v", c.endpoint, got, c.want)
		}
	}
}

// lanAddress is one of this machine's IPv4 addresses other than the
// loopback, or "".
func lanAddress() string {
	lan := ""
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
				lan = ipn.IP.String()
			}
		}
	}
	return lan
}

// sshConfigs makes "ssh -G" print configs[host] for host, fail for a host
// mapped to "", and print host as its own hostname for any other.
func sshConfigs(t *testing.T, configs map[string]string) {
	t.Helper()
	old := sshConfig
	t.Cleanup(func() { sshConfig = old })
	sshConfig = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		host := args[len(args)-1]
		out, ok := configs[host]
		if !ok {
			return []byte("user dev\nhostname " + host + "\nport 22\n"), nil
		}
		if out == "" {
			return nil, errors.New("exit status 255")
		}
		return []byte(out), nil
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
	sshConfigs(t, nil)
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
		if Remote("docker", endpoint, os.Getenv) {
			t.Errorf("Remote(%q) = true, want this Mac's", endpoint)
		}
	}
}

// A tcp host that resolves only to another machine stays remote (#118).
func TestARemoteTCPHostStaysRemote(t *testing.T) {
	resolving(t, map[string]string{"build-box": "203.0.113.7"})
	sshConfigs(t, nil)
	for _, endpoint := range []string{
		"tcp://build-box:2376",
		"build-box:2376",
		"tcp://203.0.113.7:2376",
		"tcp://[2001:db8::1]:2376",
		"2001:db8::1",
	} {
		if !Remote("docker", endpoint, os.Getenv) {
			t.Errorf("Remote(%q) = false, want remote", endpoint)
		}
	}
}

// A tcp host that does not resolve, or not in time, is this Mac's: the
// shim gates it rather than skip a local call, and waits about 200 ms
// for a slow resolver (#118).
func TestAnUnresolvedTCPHostIsThisMacs(t *testing.T) {
	resolving(t, nil)
	if Remote("docker", "tcp://no-such-box:2375", os.Getenv) {
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
	if Remote("docker", "tcp://slow-box:2375", os.Getenv) {
		t.Error("a host that resolves too late is remote, want this Mac's")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the shim waited %v for the resolver, want about 200ms", took)
	}
}

// A numeric host is resolved on either scheme, as getaddrinfo reads it, and
// never taken for an ssh alias: 127.1, 127.0.1, 2130706433 and 0x7f000001
// are this Mac's when they resolve to it, and 0177.0.0.1, read as
// 177.0.0.1, stays remote. localhost. is a name (#118), and so is cafe,
// which on ssh is resolved as a name since #256.
func TestANumericHostIsResolvedOnEitherScheme(t *testing.T) {
	resolving(t, map[string]string{
		"127.1": "127.0.0.1", "127.0.1": "127.0.0.1", "2130706433": "127.0.0.1",
		"0x7f000001": "127.0.0.1", "0177.0.0.1": "177.0.0.1", "cafe": "203.0.113.7",
	})
	sshConfigs(t, nil)
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
			if got := Remote("docker", endpoint, os.Getenv); got != c.remote {
				t.Errorf("Remote(%q) = %v, want %v", endpoint, got, c.remote)
			}
		}
	}
	if !Remote("docker", "ssh://dev@cafe", os.Getenv) {
		t.Error("ssh://dev@cafe is this Mac's, want remote: a name for 203.0.113.7")
	}
}

// An ssh host is the hostname "ssh -G" gives for it, resolved as a tcp host
// is, for either client: an alias for this Mac, or a name that resolves to
// it, is this Mac's; one for another machine is remote; and when ssh -G or
// the resolver fails, the call is gated (#256). The far aliases' own names
// resolve away too, as podman may dial them.
func TestAnSSHHostIsTheOneSSHConfigNames(t *testing.T) {
	resolving(t, map[string]string{
		"lo-name": "127.0.0.1", "far-name": "203.0.113.5", "lan-name": lanAddress(),
		"far-alias": "203.0.113.8", "name-alias": "203.0.113.8",
	})
	sshConfigs(t, map[string]string{
		"lo-alias":   "user dev\nhostname 127.0.0.1\nport 22\n",
		"far-alias":  "user dev\nhostname 192.0.2.1\nport 22\n",
		"name-alias": "user dev\nhostname far-name\nport 22\n",
		"broken":     "",
		"no-host":    "user dev\nport 22\n",
	})
	rows := []struct {
		endpoint string
		remote   bool
	}{
		{"ssh://dev@lo-alias", false},
		{"ssh://dev@far-alias", true},
		{"ssh://dev@name-alias", true},
		{"ssh://dev@lo-name", false},
		{"ssh://dev@far-name", true},
		{"ssh://dev@no-such-box", false}, // does not resolve
		{"ssh://dev@broken", false},
		{"ssh://dev@no-host", false},
		{"ssh://core@127.0.0.1:52000/run/podman/podman.sock", false}, // podman machine
	}
	if lanAddress() != "" {
		rows = append(rows, struct {
			endpoint string
			remote   bool
		}{"ssh://dev@lan-name", false})
	}
	for _, client := range []string{"docker", "podman"} {
		for _, c := range rows {
			if got := Remote(client, c.endpoint, os.Getenv); got != c.remote {
				t.Errorf("%s: Remote(%q) = %v, want %v", client, c.endpoint, got, c.remote)
			}
		}
	}
}

// The shim asks ssh -G for the host as the docker CLI hands it to ssh,
// with its user and port (#256).
func TestSSHConfigIsAskedForTheHostTheClientDials(t *testing.T) {
	resolving(t, nil)
	var asked [][]string
	old := sshConfig
	t.Cleanup(func() { sshConfig = old })
	sshConfig = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		asked = append(asked, args)
		return []byte("hostname 203.0.113.5\n"), nil
	}
	Remote("docker", "ssh://dev@box:2222/run/docker.sock", os.Getenv)
	Remote("docker", "ssh://box", os.Getenv)
	want := [][]string{{"-G", "-l", "dev", "-p", "2222", "--", "box"}, {"-G", "--", "box"}}
	if fmt.Sprint(asked) != fmt.Sprint(want) {
		t.Errorf("ssh was asked %q, want %q", asked, want)
	}
}

// An ssh -G that does not answer within resolveTimeout gates the call
// (#256).
func TestASlowSSHConfigIsThisMacs(t *testing.T) {
	resolving(t, map[string]string{"far-name": "203.0.113.5"})
	old := sshConfig
	t.Cleanup(func() { sshConfig = old })
	sshConfig = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return []byte("hostname 203.0.113.5\n"), nil
		}
	}
	for _, client := range []string{"docker", "podman"} {
		start := time.Now()
		if Remote(client, "ssh://dev@far-name", os.Getenv) {
			t.Errorf("%s: a host ssh -G names too late is remote, want this Mac's", client)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("%s: the shim waited %v for ssh -G, want about 200ms", client, took)
		}
	}
}

// docker's ssh honours ProxyJump and ProxyCommand, so a host reached
// through one is remote whatever its hostname; podman ignores both (#256).
func TestAProxiedSSHHostIsRemoteForDockerOnly(t *testing.T) {
	resolving(t, map[string]string{"far-jump": "203.0.113.7"})
	sshConfigs(t, map[string]string{
		"jump":     "hostname 127.0.0.1\nproxyusefdpass no\nproxyjump hop\n",
		"command":  "hostname 127.0.0.1\nproxyusefdpass no\nproxycommand nc %h %p\n",
		"none":     "hostname 127.0.0.1\nproxyjump none\nproxycommand none\n",
		"far-jump": "hostname 192.0.2.1\nproxyjump hop\n",
	})
	for _, c := range []struct {
		host           string
		docker, podman bool
	}{
		{"jump", true, false},
		{"command", true, false},
		{"none", false, false},
		{"far-jump", true, true},
	} {
		endpoint := "ssh://dev@" + c.host
		if got := Remote("docker", endpoint, os.Getenv); got != c.docker {
			t.Errorf("docker: Remote(%q) = %v, want %v", endpoint, got, c.docker)
		}
		if got := Remote("podman", endpoint, os.Getenv); got != c.podman {
			t.Errorf("podman: Remote(%q) = %v, want %v", endpoint, got, c.podman)
		}
	}
}

// The ssh asked is the first on the call's PATH, as docker's connection
// helper runs it; with none there, the call is gated (#256).
func TestTheSSHAskedIsTheOneOnTheCallsPath(t *testing.T) {
	resolving(t, nil)
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'hostname 127.0.0.1'\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	path := func(p string) func(string) string {
		return func(k string) string { return map[string]string{"PATH": p}[k] }
	}
	for _, client := range []string{"docker", "podman"} {
		if Remote(client, "ssh://dev@203.0.113.5", path(dir)) {
			t.Errorf("%s: the ssh on PATH names 127.0.0.1, want this Mac's", client)
		}
		if Remote(client, "ssh://dev@203.0.113.5", path(t.TempDir())) {
			t.Errorf("%s: no ssh on PATH, want this Mac's", client)
		}
	}
}

// An ssh on PATH that leaves a child holding its stdout is not waited for
// past resolveTimeout (#256, #277).
func TestASlowSSHWrapperIsNotWaitedFor(t *testing.T) {
	resolving(t, map[string]string{"box": "203.0.113.5"})
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 3\necho 'hostname 203.0.113.5'\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string { return map[string]string{"PATH": dir}[k] }
	start := time.Now()
	if Remote("docker", "ssh://u@box", getenv) {
		t.Error("a host ssh -G names too late is remote, want this Mac's")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the shim waited %v for ssh -G, want about 200ms", took)
	}
}

// podman dials either the URL's own host or the hostname ssh -G gives for
// it, as its connections file goes, so it is remote only when both are;
// proxies are ignored. docker dials ssh -G's
// hostname (#256, plan amendment 2).
func TestAPodmanSSHHostIsRemoteOnlyWhenBothHostsAre(t *testing.T) {
	resolving(t, map[string]string{
		"lo-alias": "203.0.113.5", "here-alias": "127.0.0.1",
		"far-both": "203.0.113.6", "far-jump": "203.0.113.7",
	})
	sshConfigs(t, map[string]string{
		"lo-alias":   "hostname 127.0.0.1\n",
		"here-alias": "hostname 192.0.2.1\n",
		"far-both":   "hostname 192.0.2.2\n",
		"far-jump":   "hostname 192.0.2.3\nproxyjump hop\n",
	})
	for _, c := range []struct {
		host           string
		docker, podman bool
	}{
		{"lo-alias", false, false},  // alias local, raw name remote
		{"here-alias", true, false}, // alias remote, raw name this Mac's
		{"far-both", true, true},
		{"far-jump", true, true},
	} {
		endpoint := "ssh://u@" + c.host
		if got := Remote("docker", endpoint, os.Getenv); got != c.docker {
			t.Errorf("docker: Remote(%q) = %v, want %v", endpoint, got, c.docker)
		}
		if got := Remote("podman", endpoint, os.Getenv); got != c.podman {
			t.Errorf("podman: Remote(%q) = %v, want %v", endpoint, got, c.podman)
		}
	}
}
