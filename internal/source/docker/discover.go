package docker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Candidates returns the Unix sockets an engine may answer on, in order
// (#227): the configured socket, a unix:// DOCKER_HOST, the unix endpoint of
// every Docker context, and Docker Desktop's per-user socket. A TCP endpoint
// is a remote engine whose memory is not this Mac's. A path that does not
// exist is left out without dialing, and a socket named twice, directly or
// through a link, comes once, by its first name.
func Candidates(config string, getenv func(string) string) []string {
	paths := []string{config}
	if p, ok := strings.CutPrefix(getenv("DOCKER_HOST"), "unix://"); ok {
		paths = append(paths, p)
	}
	if home := getenv("HOME"); home != "" {
		paths = append(paths, contextSockets(filepath.Join(home, ".docker"))...)
		paths = append(paths, filepath.Join(home, ".docker", "run", "docker.sock"))
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue // missing
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, p)
	}
	return out
}

// contextSockets returns the unix endpoint of each Docker context in dir,
// as the docker CLI stores them: contexts/meta/<sha256 of name>/meta.json.
// One that cannot be read is skipped.
func contextSockets(dir string) []string {
	metas, _ := filepath.Glob(filepath.Join(dir, "contexts", "meta", "*", "meta.json"))
	var out []string
	for _, m := range metas {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var meta struct {
			Endpoints map[string]struct{ Host string }
		}
		if json.Unmarshal(b, &meta) != nil {
			continue
		}
		if p, ok := strings.CutPrefix(meta.Endpoints["docker"].Host, "unix://"); ok && p != "" {
			out = append(out, p)
		}
	}
	return out
}
