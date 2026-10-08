package shim

import (
	"slices"
	"strings"
	"testing"
)

func TestLabelled(t *testing.T) {
	const l = "dev.headroom.lease=lease-1"
	for _, tc := range []struct {
		name, args, want string
	}{
		{"docker", "run --rm alpine true", "run --label " + l + " --rm alpine true"},
		{"docker", "--context default run -d alpine", "--context default run --label " + l + " -d alpine"},
		{"docker", "container create --name db postgres", "container create --label " + l + " --name db postgres"},
		{"podman", "run alpine", "run --label " + l + " alpine"},
		// Nothing to label: the container exists, or compose and tart
		// start their own.
		{"docker", "start db", ""},
		{"docker", "compose up -d", ""},
		{"tart", "run vm", ""},
		{"docker", "run --help", ""},
		{"docker", "ps", ""},
	} {
		got, ok := Labelled(tc.name, strings.Fields(tc.args), "dev.headroom.lease", "lease-1")
		if tc.want == "" {
			if ok {
				t.Errorf("%s %s: labelled as %q", tc.name, tc.args, got)
			}
			continue
		}
		if !ok || !slices.Equal(got, strings.Fields(tc.want)) {
			t.Errorf("%s %s = %q, %v; want %q", tc.name, tc.args, got, ok, tc.want)
		}
	}
}

func TestLabelledLeavesTheArgsAlone(t *testing.T) {
	args := []string{"run", "alpine"}
	Labelled("docker", args, "k", "v")
	if !slices.Equal(args, []string{"run", "alpine"}) {
		t.Fatalf("args changed: %q", args)
	}
}
