package ollama

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cybagard/cyba-headroom/internal/binpath"
)

// Locations are where Ollama installs itself: Homebrew's ollama and
// Ollama.app. Searched after PATH; "~/" is under HOME.
var Locations = []string{
	"/opt/homebrew/bin/ollama",
	"/usr/local/bin/ollama",
	"/Applications/Ollama.app",
	"~/Applications/Ollama.app",
}

// Installed reports whether Ollama is installed: the configured binary, an
// ollama binary on PATH, or one of locations (a binary or an app bundle).
func Installed(configured string, getenv func(string) string, locations []string) bool {
	var bins []string
	for _, p := range locations {
		if strings.HasSuffix(p, ".app") {
			if rest, ok := strings.CutPrefix(p, "~/"); ok {
				p = filepath.Join(getenv("HOME"), rest)
			}
			if !filepath.IsAbs(p) {
				continue // a relative HOME would resolve against the cwd
			}
			if fi, err := os.Stat(p); err == nil && fi.IsDir() {
				return true
			}
			continue
		}
		bins = append(bins, p)
	}
	return binpath.Find(configured, "ollama", getenv, bins...) != ""
}

const defaultPort = "11434"

// Endpoint returns the base URL of Ollama's API: the configured host, else
// OLLAMA_HOST, else 127.0.0.1:11434, in any form Ollama accepts (host,
// host:port, :port, scheme://host:port/path). ok is false when it is not on
// this Mac: a remote engine's models do not use this Mac's memory, so
// headroom never contacts it. A server listening on every address
// (0.0.0.0, ::) is asked over loopback. The error says why a host is not
// used.
func Endpoint(configured, ollamaHost string) (string, error) {
	raw := configured
	if raw == "" {
		raw = ollamaHost
	}
	raw = strings.Trim(strings.TrimSpace(raw), `"'`)
	if raw == "" {
		raw = "127.0.0.1"
	}
	fail := func(why string) (string, error) {
		return "", fmt.Errorf("ollama host %q %s; headroom does not ask it (set [ollama] host)", raw, why)
	}
	// As Ollama reads it: an explicit scheme brings its own default port.
	scheme, hostport, found := strings.Cut(raw, "://")
	port := defaultPort
	switch {
	case !found:
		scheme, hostport = "http", raw
	case scheme == "http":
		port = "80"
	case scheme == "https":
		port = "443"
	default:
		return fail("has scheme " + scheme + ", not http or https")
	}
	hostport, path, _ := strings.Cut(hostport, "/")
	if strings.Contains(hostport, "@") {
		return fail("has a user name")
	}
	host := hostport
	// Ollama falls back to the scheme's default for a port it cannot read.
	schemePort := port
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		host, port = h, p
	} else {
		host = strings.Trim(host, "[]")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		port = schemePort
	} else if n == 0 {
		return fail("has port 0, a random port")
	}
	switch ip := net.ParseIP(host); {
	case host == "":
		host = "127.0.0.1"
	case host == "localhost":
	case ip == nil:
		return fail("is not a loopback address")
	case ip.IsUnspecified() && ip.To4() != nil:
		host = "127.0.0.1"
	case ip.IsUnspecified():
		host = "::1"
	case !ip.IsLoopback():
		return fail("is not a loopback address")
	}
	u := url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port), Path: strings.TrimSuffix("/"+path, "/")}
	return u.String(), nil
}

// maxPS caps the /api/ps response: a few models' entries are a few KB.
const maxPS = 1 << 20

// client uses no proxy and follows no redirect: the API is on this Mac.
var client = &http.Client{
	Transport: &http.Transport{Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// HTTP reads /api/ps from Ollama's API at Base. A zero Timeout means 2 s.
type HTTP struct {
	Base    string
	Timeout time.Duration
}

// NewHTTP returns a client for the API at base, as Endpoint returns it.
func NewHTTP(base string) *HTTP { return &HTTP{Base: base, Timeout: 2 * time.Second} }

// PS implements API.
func (h *HTTP) PS(ctx context.Context) ([]byte, error) {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.Base+"/api/ps", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/ps: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPS+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxPS {
		return nil, errors.New("GET /api/ps: response over 1 MiB")
	}
	return b, nil
}
