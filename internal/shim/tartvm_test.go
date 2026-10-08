package shim

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTartVM(t *testing.T) {
	home := t.TempDir()
	for name, cfg := range map[string]string{
		"mac-ci":   `{"os":"darwin","memorySize":17179869184,"cpuCount":8}`,
		"linux-ci": `{"os":"linux","memorySize":4294967296}`,
		"broken":   `{"os":`,
		"old-mac":  `{"memorySize":8589934592}`, // before Linux support: macOS
	} {
		if err := os.MkdirAll(filepath.Join(home, "vms", name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "vms", name, "config.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oci := filepath.Join(home, "cache", "OCIs", "ghcr.io", "cirruslabs", "macos-sequoia-base")
	for _, ref := range []string{"latest", "sha256:abc"} {
		if err := os.MkdirAll(filepath.Join(oci, ref), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(oci, ref, "config.json"), []byte(`{"os":"darwin","memorySize":8589934592}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	getenv := func(k string) string { return map[string]string{"TART_HOME": home}[k] }
	for _, c := range []struct {
		name  string
		macOS bool
		mem   uint64
		ok    bool
	}{
		{"mac-ci", true, 16 << 30, true},
		{"linux-ci", false, 4 << 30, true},
		{"broken", false, 0, false},
		{"old-mac", true, 8 << 30, true},
		{"missing", false, 0, false},
		{"ghcr.io/cirruslabs/macos-sequoia-base:latest", true, 8 << 30, true}, // a cached OCI image
		{"ghcr.io/cirruslabs/macos-sequoia-base", true, 8 << 30, true},        // :latest
		{"ghcr.io/cirruslabs/macos-sequoia-base@sha256:abc", true, 8 << 30, true},
		{"ghcr.io/cirruslabs/macos-sequoia-base:sequoia@sha256:abc", true, 8 << 30, true}, // tag and digest: the digest
		{"ghcr.io/cirruslabs/not-cached:latest", false, 0, false},
		{"ghcr.io/../../vms/mac-ci:latest", false, 0, false},
		{"../vms/mac-ci", false, 0, false},
		{"..", false, 0, false},
		{"", false, 0, false},
	} {
		macOS, mem, ok := TartVM(c.name, getenv)
		if macOS != c.macOS || mem != c.mem || ok != c.ok {
			t.Errorf("TartVM(%q) = %v, %d, %v; want %v, %d, %v", c.name, macOS, mem, ok, c.macOS, c.mem, c.ok)
		}
	}
	if _, _, ok := TartVM("mac-ci", func(k string) string { return map[string]string{"TART_HOME": "rel"}[k] }); ok {
		t.Error("a relative TART_HOME was read")
	}
	// Without TART_HOME, ~/.tart.
	if err := os.MkdirAll(filepath.Join(home, ".tart", "vms", "v"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".tart", "vms", "v", "config.json"), []byte(`{"os":"darwin","memorySize":1073741824}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if macOS, mem, ok := TartVM("v", func(k string) string { return map[string]string{"HOME": home}[k] }); !macOS || mem != 1<<30 || !ok {
		t.Errorf("under HOME: %v %d %v", macOS, mem, ok)
	}
}
