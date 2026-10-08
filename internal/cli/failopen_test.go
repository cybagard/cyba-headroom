package cli

import (
	"bufio"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// failOpenDaemon is a misbehaving daemon at the config dir's socket.
type failOpenDaemon func(t *testing.T, sock string)

// listen binds sock and keeps the file on Close, so a test can leave a
// stale socket behind.
func listen(t *testing.T, sock string) *net.UnixListener {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// serveOnce accepts connections and answers each with reply after delay,
// or never if reply is "".
func serveOnce(t *testing.T, sock, reply string, delay time.Duration) {
	ln := listen(t, sock)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = bufio.NewReader(c).ReadString('\n')
				if reply == "" {
					time.Sleep(2 * time.Second)
					return
				}
				time.Sleep(delay)
				_, _ = io.WriteString(c, reply+"\n")
			}()
		}
	}()
}

// R7: however the daemon fails, a gated call runs the real binary after
// one warning line, within the daemon timeout (500 ms) and a little.
func TestShimFailsOpenWithinTheTimeout(t *testing.T) {
	for name, c := range map[string]struct {
		daemon failOpenDaemon
		cause  string
	}{
		"no socket":     {func(*testing.T, string) {}, "not running"},
		"stale socket":  {func(t *testing.T, sock string) { ln := listen(t, sock); ln.SetUnlinkOnClose(false); _ = ln.Close() }, "not running"},
		"never accepts": {func(t *testing.T, sock string) { listen(t, sock) }, "no answer in 500ms"},
		"never replies": {func(t *testing.T, sock string) { serveOnce(t, sock, "", 0) }, "no answer in 500ms"},
		"replies late":  {func(t *testing.T, sock string) { serveOnce(t, sock, `{"v":1,"ok":true}`, time.Second) }, "no answer in 500ms"},
		"garbage":       {func(t *testing.T, sock string) { serveOnce(t, sock, "nope", 0) }, "bad reply"},
		"other version": {func(t *testing.T, sock string) { serveOnce(t, sock, `{"v":99,"ok":true}`, 0) }, "headroom install"},
		"old daemon": {func(t *testing.T, sock string) {
			serveOnce(t, sock, `{"v":1,"ok":false,"error":"unknown op \"check\""}`, 0)
		}, "headroom install"},
		"not a socket": {func(t *testing.T, sock string) {
			if err := os.WriteFile(sock, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "not running"},
		"no permission": {func(t *testing.T, sock string) {
			if os.Geteuid() == 0 {
				t.Skip("root connects whatever the socket's mode")
			}
			listen(t, sock)
			if err := os.Chmod(sock, 0); err != nil {
				t.Fatal(err)
			}
		}, "not accessible"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newShimRig(t)
			c.daemon(t, filepath.Join(r.dir, "d.sock"))
			start := time.Now()
			var errb strings.Builder
			code := Run(Env{Args: []string{"docker", "run", "alpine"}, Stdout: io.Discard, Stderr: &errb,
				Getenv: func(string) string { return "" }, Environ: func() []string { return append(r.env, "HEADROOM_WORKTREE=w") },
				exec: func(path string, _, _ []string) error { r.execed = path; return nil }})
			took := time.Since(start)
			stderr := errb.String()
			// The daemon timeout plus room for a loaded runner and -race.
			if code != 0 || r.execed == "" || took > 900*time.Millisecond {
				t.Fatalf("exit %d, exec %q, took %v", code, r.execed, took)
			}
			if strings.Count(stderr, "\n") != 1 || !strings.HasPrefix(stderr, "headroom: not gated (daemon ") ||
				!strings.Contains(stderr, c.cause) || !strings.Contains(stderr, "running `docker run alpine` anyway") {
				t.Fatalf("want one warning line with %q, got %q", c.cause, stderr)
			}
		})
	}
}

// A broken config is one line too, however many settings are wrong.
func TestShimFailsOpenOnABrokenConfigInOneLine(t *testing.T) {
	r := newShimRig(t)
	if err := os.WriteFile(filepath.Join(r.dir, "config.toml"), []byte("[daemon]\ninterval = \"0s\"\n[policy]\nlease_timeout = \"0s\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.ask = allow
	code, stderr := r.run("docker", "run", "alpine")
	if code != 0 || r.execed == "" || strings.Count(stderr, "\n") != 1 || !strings.HasPrefix(stderr, "headroom: not gated (config: ") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

// A call that starts nothing never reads the config or asks the daemon,
// whatever state they are in (R7).
func TestCallsThatStartNothingAreUntouched(t *testing.T) {
	for _, argv := range [][]string{{"docker", "ps"}, {"docker", "compose", "down"}, {"tart", "list"}, {"docker", "run", "--help"}} {
		r := newShimRig(t)
		if err := os.WriteFile(filepath.Join(r.dir, "config.toml"), []byte("not toml"), 0o644); err != nil {
			t.Fatal(err)
		}
		listen(t, filepath.Join(r.dir, "d.sock")) // a daemon that would hang
		r.ask = allow
		start := time.Now()
		code, stderr := r.run(argv...)
		if code != 0 || r.execed == "" || stderr != "" || len(r.asked) != 0 || time.Since(start) > 100*time.Millisecond {
			t.Errorf("%v: exit %d, stderr %q, asked %d, took %v", argv, code, stderr, len(r.asked), time.Since(start))
		}
	}
}
