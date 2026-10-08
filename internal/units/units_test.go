package units

import "testing"

func TestGB(t *testing.T) {
	if GB(5<<29) != "2.5" || SignedGB(-3<<30) != "-3.0" || GB(0) != "0.0" {
		t.Fatalf("%s %s %s", GB(5<<29), SignedGB(-3<<30), GB(0))
	}
}
