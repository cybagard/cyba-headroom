package config

import (
	"strings"
	"testing"
)

func TestMerge(t *testing.T) {
	in := `# my headroom config
socket = "/tmp/hr.sock"

[budget]
host_baseline_gb = 10   # set by hand
docker_overhead_gb = 1.6

[policy]
# keep a floor
lease_timeout = "2m"

[samples]
host_baseline_gb_note = "not a key we touch"
`
	out, err := Merge([]byte(in), []Setting{
		{"budget", "host_baseline_gb", "14.5"},
		{"budget", "lmstudio_idle_gb", "0.7"},
		{"policy", "min_headroom_gb", "6"},
		{"newsec", "x", "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `# my headroom config
socket = "/tmp/hr.sock"

[budget]
host_baseline_gb = 14.5   # set by hand
docker_overhead_gb = 1.6
lmstudio_idle_gb = 0.7

[policy]
# keep a floor
lease_timeout = "2m"
min_headroom_gb = 6

[samples]
host_baseline_gb_note = "not a key we touch"

[newsec]
x = 1
`
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestMergeIntoEmptyFile(t *testing.T) {
	out, err := Merge(nil, []Setting{{"budget", "host_baseline_gb", "8"}})
	if err != nil || string(out) != "[budget]\nhost_baseline_gb = 8\n" {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestMergeOnlyTouchesItsSection(t *testing.T) {
	in := "[policy]\nmin_headroom_gb = 2\n\n[budget]\n"
	out, err := Merge([]byte(in), []Setting{{"budget", "min_headroom_gb", "9"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "[policy]\nmin_headroom_gb = 2\n") || !strings.Contains(string(out), "[budget]\nmin_headroom_gb = 9\n") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestMergeRefusesArraysOfTables(t *testing.T) {
	if _, err := Merge([]byte("[[budget]]\nx = 1\n"), []Setting{{"budget", "x", "2"}}); err == nil {
		t.Fatal("want an error for [[budget]]")
	}
}
