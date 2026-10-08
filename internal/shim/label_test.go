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
		// Last before the image: Docker keeps the last --label of a key,
		// and applies --label-file before --label.
		{"docker", "run --rm alpine true", "run --rm --label " + l + " alpine true"},
		{"docker", "--context default run -d alpine", "--context default run -d --label " + l + " alpine"},
		{"docker", "container create --name db postgres", "container create --name db --label " + l + " postgres"},
		{"podman", "run alpine", "run --label " + l + " alpine"},
		{"docker", "run --label dev.headroom.lease=forged --label-file f alpine", "run --label dev.headroom.lease=forged --label-file f --label " + l + " alpine"},
		{"docker", "run -e X=1 -- alpine", "run -e X=1 --label " + l + " -- alpine"},
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
