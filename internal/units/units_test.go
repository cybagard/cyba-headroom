package units

import "testing"

func TestGB(t *testing.T) {
	if GB(5<<29) != "2.5" || SignedGB(-3<<30) != "-3.0" || GB(0) != "0.0" {
		t.Fatalf("%s %s %s", GB(5<<29), SignedGB(-3<<30), GB(0))
	}
}

func TestSize(t *testing.T) {
	for b, want := range map[uint64]string{512: "512 B", 512 << 10: "512 KB", 32 << 20: "32 MB", 3000 << 10: "3 MB", 3 << 29: "1.5 GB"} {
		if got := Size(b); got != want {
			t.Errorf("Size(%d) = %q, want %q", b, got, want)
		}
	}
}
