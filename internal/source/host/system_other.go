//go:build !darwin

package host

import (
	"fmt"
	"runtime"
)

// System is unsupported off macOS; the host source reports stale.
type System struct{}

var errUnsupported = fmt.Errorf("host memory source is macOS-only, not %s", runtime.GOOS)

// Uint32 implements Sysctl.
func (System) Uint32(string) (uint32, error) { return 0, errUnsupported }

// Uint64 implements Sysctl.
func (System) Uint64(string) (uint64, error) { return 0, errUnsupported }

// Raw implements Sysctl.
func (System) Raw(string) ([]byte, error) { return nil, errUnsupported }
