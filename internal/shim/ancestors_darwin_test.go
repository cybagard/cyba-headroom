//go:build darwin

package shim

import (
	"os"
	"testing"
)

func TestAncestorsOnThisMac(t *testing.T) {
	a := Ancestors()
	if len(a) == 0 || a[0] != os.Getppid() {
		t.Fatalf("Ancestors() = %v, want it to start with the parent %d", a, os.Getppid())
	}
	if a[len(a)-1] != 1 && len(a) != maxAncestors {
		t.Fatalf("Ancestors() = %v, want it to end at launchd (1)", a)
	}
}
