package ollama_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/source/ollama"
)

func TestEndpoint(t *testing.T) {
	for _, tc := range []struct {
		configured, env string
		want            string
		ok              bool
	}{
		{"", "", "http://127.0.0.1:11434", true},
		{"", "127.0.0.1", "http://127.0.0.1:11434", true},
		{"", "localhost:8080", "http://localhost:8080", true},
		{"", ":8080", "http://127.0.0.1:8080", true},
		{"", "0.0.0.0", "http://127.0.0.1:11434", true}, // listens everywhere, ask over loopback
		{"", "0.0.0.0:9000", "http://127.0.0.1:9000", true},
		{"", "[::]:9000", "http://[::1]:9000", true},
		{"", "[::1]:9000", "http://[::1]:9000", true},
		{"", "http://127.0.0.1:11434/", "http://127.0.0.1:11434", true},
		{"", "https://localhost", "https://localhost:443", true},
		{"", "http://127.0.0.1", "http://127.0.0.1:80", true}, // as Ollama: an explicit scheme's own port
		{"", "http://127.0.0.1:11434/ollama", "http://127.0.0.1:11434/ollama", true},
		{"", "  127.0.0.1  ", "http://127.0.0.1:11434", true},
		{"", `"127.0.0.1:1234"`, "http://127.0.0.1:1234", true}, // quoted, as Ollama accepts
		{"127.0.0.1:5000", "127.0.0.1:6000", "http://127.0.0.1:5000", true},
		// Not this Mac's memory: never contacted.
		{"", "10.0.0.5", "", false},
		{"", "gpu-box.local:11434", "", false},
		{"", "https://ollama.example.com", "", false},
		{"", "ftp://127.0.0.1", "", false},
		{"", "http://someone@127.0.0.1", "", false},
		// An empty or invalid port is Ollama's default, as Ollama reads it.
		{"", "localhost:", "http://localhost:11434", true},
		{"", "127.0.0.1:notaport", "http://127.0.0.1:11434", true},
		{"", "127.0.0.1:99999", "http://127.0.0.1:11434", true},
		{"", "127.0.0.1:0", "", false},
	} {
		got, err := ollama.Endpoint(tc.configured, tc.env)
		if ok := err == nil; got != tc.want || ok != tc.ok {
			t.Errorf("Endpoint(%q, %q) = %q, %v; want %q, %v", tc.configured, tc.env, got, ok, tc.want, tc.ok)
		}
	}
}

func TestEndpointSaysWhy(t *testing.T) {
	for host, why := range map[string]string{
		"10.0.0.5":            "not a loopback address",
		"gpu-box.local:11434": "not a loopback address",
		"unix:///tmp/o.sock":  "scheme",
		"127.0.0.1:0":         "port 0",
		"http://u@127.0.0.1":  "user",
	} {
		if _, err := ollama.Endpoint(host, ""); err == nil || !strings.Contains(err.Error(), why) || !strings.Contains(err.Error(), host) {
			t.Errorf("Endpoint(%q) err = %v, want the host and %q", host, err, why)
		}
	}
}

func TestHTTPReadsPS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/ps" {
			t.Errorf("got %s %s, want GET /api/ps", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	out, err := ollama.NewHTTP(srv.URL).PS(context.Background())
	if err != nil || string(out) != `{"models":[]}` {
		t.Fatalf("PS = %q, %v", out, err)
	}
}

func TestHTTPFailures(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"status": func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nope", http.StatusInternalServerError) },
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://198.51.100.1/api/ps", http.StatusFound)
		},
		"too large": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"models":[` + strings.Repeat(" ", 2<<20) + `]}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			if _, err := ollama.NewHTTP(srv.URL).PS(context.Background()); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestHTTPLiteralDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	if _, err := (&ollama.HTTP{Base: srv.URL}).PS(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	c := ollama.NewHTTP(srv.URL)
	c.Timeout = 50 * time.Millisecond
	start := time.Now()
	if _, err := c.PS(context.Background()); err == nil {
		t.Fatal("want timeout error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestHTTPIgnoresProxyEnv(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://198.51.100.1:1")
	t.Setenv("http_proxy", "http://198.51.100.1:1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	if _, err := ollama.NewHTTP(srv.URL).PS(context.Background()); err != nil {
		t.Fatalf("PS through a proxy: %v", err)
	}
}

func TestInstalled(t *testing.T) {
	exe := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	exe(filepath.Join(bin, "ollama"))
	home := t.TempDir()
	app := filepath.Join(home, "Applications", "Ollama.app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		configured string
		env        map[string]string
		want       bool
	}{
		"on PATH":               {"", map[string]string{"PATH": bin}, true},
		"configured":            {filepath.Join(bin, "ollama"), nil, true},
		"app in ~/Applications": {"", map[string]string{"HOME": home}, true},
		"nowhere":               {"", map[string]string{"PATH": t.TempDir(), "HOME": t.TempDir()}, false},
	} {
		t.Run(name, func(t *testing.T) {
			got := ollama.Installed(tc.configured, func(k string) string { return tc.env[k] }, []string{"/nonexistent/ollama", "~/Applications/Ollama.app"})
			if got != tc.want {
				t.Fatalf("Installed = %v, want %v", got, tc.want)
			}
		})
	}
}
