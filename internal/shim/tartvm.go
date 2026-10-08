package shim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// TartVM reads a local Tart VM's config (R6, #29): whether it is macOS,
// which takes one of the two macOS VM slots, and its memory. One small file
// read, no tart exec. ok is false for a name that is not a plain local VM
// name (an OCI reference, a path) or a VM with no readable config.
func TartVM(name string, getenv func(string) string) (macOS bool, memoryBytes uint64, ok bool) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return false, 0, false
	}
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
	b, err := os.ReadFile(filepath.Join(home, "vms", name, "config.json"))
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
