package shim

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// The marker names this process, and stays the same across calls (an exec
// keeps it).
func TestSelf(t *testing.T) {
	a, b := Self(), Self()
	if a != b || !strings.HasPrefix(a, strconv.Itoa(os.Getpid())) {
		t.Fatalf("Self() = %q then %q", a, b)
	}
}

// The parent's marker starts with its PID.
func TestParentSelf(t *testing.T) {
	pid := strconv.Itoa(os.Getppid())
	if p := ParentSelf(); p != pid && !strings.HasPrefix(p, pid+"@") {
		t.Fatalf("ParentSelf() = %q, want the parent %d", p, os.Getppid())
	}
}
