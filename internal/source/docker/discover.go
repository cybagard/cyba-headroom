package docker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Candidate is a socket an engine may answer on. Trusted is false for one
// only a Docker context names: a file anything may write (#227).
type Candidate struct {
	Socket  string
	Trusted bool
}

// Candidates returns the Unix sockets an engine may answer on, in order
// (#227): the configured socket, a unix:// DOCKER_HOST, the unix endpoint of
// every Docker context, and Docker Desktop's per-user socket. A TCP endpoint
// is a remote engine whose memory is not this Mac's. A path that does not
// exist is left out without dialing, and a socket named twice, directly or
// through a link, comes once, by its first name; it is trusted if any name
// of it is.
func Candidates(config string, getenv func(string) string) []Candidate {
	paths := []Candidate{{config, true}}
	if p, ok := strings.CutPrefix(getenv("DOCKER_HOST"), "unix://"); ok {
		paths = append(paths, Candidate{p, true})
	}
	if home := getenv("HOME"); home != "" {
		for _, p := range contextSockets(filepath.Join(home, ".docker")) {
			paths = append(paths, Candidate{p, false})
		}
		paths = append(paths, Candidate{filepath.Join(home, ".docker", "run", "docker.sock"), true})
	}
	var out []Candidate
	at := map[string]int{}
	for _, c := range paths {
		if c.Socket == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(c.Socket)
		if err != nil {
			continue // missing
		}
		if i, ok := at[resolved]; ok {
			out[i].Trusted = out[i].Trusted || c.Trusted
			continue
		}
		at[resolved] = len(out)
		out = append(out, c)
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
