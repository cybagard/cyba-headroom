//go:build darwin

package shim

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
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

// A child's ParentSelf is this process's Self: the marker a shim that
// exec'd into its parent left for it (#354).
func TestParentSelfIsTheParentsSelf(t *testing.T) {
	if os.Getenv("HEADROOM_TEST_PARENT_SELF") == "1" {
		fmt.Println(ParentSelf())
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestParentSelfIsTheParentsSelf$")
	cmd.Env = append(os.Environ(), "HEADROOM_TEST_PARENT_SELF=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.SplitN(string(out), "\n", 2)[0], Self(); got != want {
		t.Fatalf("the child's ParentSelf() = %q, want this process's Self() %q", got, want)
	}
}
