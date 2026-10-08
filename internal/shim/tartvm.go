package shim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// TartVM reads a Tart VM's config (R6, #29): whether it is macOS, which
// takes one of the two macOS VM slots, and its memory. One small file read,
// no tart exec. name is a local VM (TART_HOME/vms/<name>) or a cached OCI
// image (TART_HOME/cache/OCIs/<repo>/<tag or digest>), which tart runs too.
// ok is false for a name that cannot be one of those, or a VM with no
// readable config.
func TartVM(name string, getenv func(string) string) (macOS bool, memoryBytes uint64, ok bool) {
	home := getenv("TART_HOME")
	if home == "" {
		h := getenv("HOME")
		if h == "" {
			return false, 0, false
		}
		home = filepath.Join(h, ".tart")
	}
	if !filepath.IsAbs(home) {
		return false, 0, false // relative: it would read from the cwd
	}
	var dir string
	if strings.Contains(name, "/") {
		rel, ok := ociDir(name)
		if !ok {
			return false, 0, false
		}
		dir = filepath.Join(home, "cache", "OCIs", rel)
	} else {
		if !plainSegment(name) {
			return false, 0, false
		}
		dir = filepath.Join(home, "vms", name)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return false, 0, false
	}
	var cfg struct {
		OS         string `json:"os"`
		MemorySize uint64 `json:"memorySize"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return false, 0, false
	}
	return cfg.OS == "darwin", cfg.MemorySize, true
}

// ociDir is where Tart caches an OCI reference: its repository path, then
// its tag (latest if none) or digest.
func ociDir(ref string) (string, bool) {
	repo, version := ref, "latest"
	if i := strings.Index(ref, "@"); i >= 0 {
		repo, version = ref[:i], ref[i+1:]
	} else if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo, version = ref[:i], ref[i+1:]
	}
	segs := append(strings.Split(repo, "/"), version)
	for _, s := range segs {
		if !plainSegment(s) {
			return "", false
		}
	}
	return filepath.Join(segs...), true
}

// plainSegment reports whether s is one path segment that stays where it
// is joined: not empty, . or .., and without separators.
func plainSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}
