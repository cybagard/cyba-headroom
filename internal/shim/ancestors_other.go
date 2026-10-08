//go:build !darwin

package shim

import (
	"os"
	"strconv"
)

// Ancestors returns the parent PID only: headroom ships for macOS, and
// tests elsewhere fake the walk.
func Ancestors() []int { return []int{os.Getppid()} }

// Self names this process for HEADROOM_SHIM_CHECKED: its PID.
func Self() string { return strconv.Itoa(os.Getpid()) }
