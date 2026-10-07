//go:build !darwin

package vmproc

import (
	"context"
	"fmt"
	"runtime"
)

// Host is unsupported off macOS.
type Host struct{}

var errUnsupported = fmt.Errorf("vmproc is macOS-only, not %s", runtime.GOOS)

// VMPIDs implements System.
func (Host) VMPIDs() ([]int, error) { return nil, errUnsupported }

// Run implements System.
func (Host) Run(context.Context, string, ...string) ([]byte, error) { return nil, errUnsupported }
