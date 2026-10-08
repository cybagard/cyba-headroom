//go:build !darwin

package shim

import "os"

// Ancestors returns the parent PID only: headroom ships for macOS, and
// tests elsewhere fake the walk.
func Ancestors() []int { return []int{os.Getppid()} }
