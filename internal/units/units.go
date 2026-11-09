// Package units formats memory sizes the way headroom shows them everywhere:
// GB meaning GiB, as macOS reports memory, to one decimal.
package units

import "fmt"

// GB formats b bytes as GB to one decimal: "2.5".
func GB(b uint64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<30)) }

// SignedGB formats a signed byte count, such as a negative headroom.
func SignedGB(b int64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<30)) }

// Size formats b bytes in the largest unit it reaches, for limits people
// give in any unit: "512 B", "64 MB", "1.5 GB".
func Size(b uint64) string {
	switch {
	case b >= 1<<30:
		return GB(b) + " GB"
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

// MaxBytes bounds every byte count headroom adds up: far above any Mac's
// memory, so a bogus size an engine or model server reports only shrinks
// headroom, and the arithmetic, int64 conversions included, cannot wrap.
const MaxBytes = uint64(1) << 42

// AddSat returns a + b, saturating at MaxBytes. Either argument may be
// above MaxBytes.
func AddSat(a, b uint64) uint64 {
	a, b = min(a, MaxBytes), min(b, MaxBytes)
	if b >= MaxBytes-a {
		return MaxBytes
	}
	return a + b
}
