// Package units formats memory sizes the way headroom shows them everywhere:
// GB meaning GiB, as macOS reports memory, to one decimal.
package units

import "fmt"

// GB formats b bytes as GB to one decimal: "2.5".
func GB(b uint64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<30)) }

// SignedGB formats a signed byte count, such as a negative headroom.
func SignedGB(b int64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<30)) }
