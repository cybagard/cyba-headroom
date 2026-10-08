//go:build darwin

package shim

import (
	"strings"
	"testing"
)

// On macOS the marker also holds the start time, so a reused PID differs.
func TestSelfHasTheStartTime(t *testing.T) {
	if s := Self(); !strings.Contains(s, "@") {
		t.Fatalf("Self() = %q, want pid@start", s)
	}
}
