package docker

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// Candidate is a socket an engine may answer on. Trusted is false for one
// only a Docker context names: a file anything may write (#227). Machine is
// the Podman machine whose socket it is, from podman machine inspect (#228).
type Candidate struct {
	Socket  string
	Trusted bool
	Machine string
}

// Candidates returns the Unix sockets an engine may answer on, in order
// (#227): the configured socket, a unix:// DOCKER_HOST, the unix endpoint of
// every Docker context, Docker Desktop's per-user socket, and then more
// (Podman's, #228). A TCP endpoint
// is a remote engine whose memory is not this Mac's. A path that does not
// exist is left out without dialing, and a socket named twice, directly or
// through a link, comes once, by its first name; it is trusted if any name
// of it is, and of the machine any name of it is. A context skipped is
// logged to log at debug, and the contexts over maxContexts at warn: a
// Discovery warns of those only when their count changes.
func Candidates(config string, getenv func(string) string, log *slog.Logger, more ...Candidate) []Candidate {
	return NewDiscovery(log).Candidates(config, getenv, more...)
}

// Discovery finds the candidates tick after tick, as Candidates does, and
// warns of the contexts over maxContexts once, then again only when their
// count changes (#236).
type Discovery struct {
	log     *slog.Logger
	mu      sync.Mutex
	skipped int
}

// NewDiscovery returns a Discovery that logs to log.
func NewDiscovery(log *slog.Logger) *Discovery { return &Discovery{log: log} }

// Candidates returns the candidates, as the package's Candidates does.
func (d *Discovery) Candidates(config string, getenv func(string) string, more ...Candidate) []Candidate {
	out, skipped := candidates(config, getenv, d.log, more)
	d.mu.Lock()
	defer d.mu.Unlock()
	if skipped > 0 && skipped != d.skipped {
		d.log.Warn("docker contexts skipped: over the limit read per tick", "skipped", skipped, "limit", maxContexts)
	}
	d.skipped = skipped
	return out
}

// candidates returns Candidates, and the count of contexts not read.
func candidates(config string, getenv func(string) string, log *slog.Logger, more []Candidate) ([]Candidate, int) {
	skipped := 0
	paths := []Candidate{{Socket: config, Trusted: true}}
	if p, ok := strings.CutPrefix(getenv("DOCKER_HOST"), "unix://"); ok {
		paths = append(paths, Candidate{Socket: p, Trusted: true})
	}
	if home := getenv("HOME"); home != "" {
		var sockets []string
		sockets, skipped = contextSockets(filepath.Join(home, ".docker"), log)
		for _, p := range sockets {
			paths = append(paths, Candidate{Socket: p})
		}
		paths = append(paths, Candidate{Socket: filepath.Join(home, ".docker", "run", "docker.sock"), Trusted: true})
	}
	paths = append(paths, more...)
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
			out[i].Machine = cmp.Or(out[i].Machine, c.Machine)
			continue
		}
		at[resolved] = len(out)
		out = append(out, c)
	}
	return out, skipped
}

// maxMeta bounds the read of a context's meta.json: the docker CLI's are
// a few hundred bytes.
const maxMeta = 64 << 10

// maxContexts bounds the Docker contexts read per tick: each read is
// bounded, but 20,000 of them took 2 s of the source's 3 (#236).
const maxContexts = 32

// contextSockets returns the unix endpoint of each Docker context in dir,
// as the docker CLI stores them: contexts/meta/<sha256 of name>/meta.json.
// Only the first maxContexts by that name are read, so the choice is
// stable; it returns the count of the others. One that cannot be read is
// skipped.
func contextSockets(dir string, log *slog.Logger) ([]string, int) {
	meta := filepath.Join(dir, "contexts", "meta")
	entries, _ := os.ReadDir(meta) // sorted by name
	skipped := max(0, len(entries)-maxContexts)
	var out []string
	for _, e := range entries[:len(entries)-skipped] {
		m := filepath.Join(meta, e.Name(), "meta.json")
		b, err := readMeta(m)
		if err != nil {
			log.Debug("docker context skipped", "meta", m, "err", err)
			continue
		}
		var meta struct {
			Endpoints map[string]struct{ Host string }
		}
		if err := json.Unmarshal(b, &meta); err != nil {
			log.Debug("docker context skipped", "meta", m, "err", err)
			continue
		}
		if p, ok := strings.CutPrefix(meta.Endpoints["docker"].Host, "unix://"); ok && p != "" {
			out = append(out, p)
		}
	}
	return out, skipped
}

// readMeta reads a context's meta.json, a file anything may write (#227,
// review round 3): only a regular file, opened without waiting (a FIFO's
// open waits for a writer), and only up to maxMeta (a device such as
// /dev/zero never ends).
func readMeta(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %v", fi.Mode().Type())
	}
	b, err := io.ReadAll(io.LimitReader(f, maxMeta+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxMeta {
		return nil, fmt.Errorf("over %d bytes", maxMeta)
	}
	return b, nil
}
