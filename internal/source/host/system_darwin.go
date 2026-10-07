package host

import "golang.org/x/sys/unix"

// System reads the running kernel through sysctl(3). x/sys calls libSystem
// without cgo, so the binary still cross-compiles with CGO_ENABLED=0.
type System struct{}

// Uint32 implements Sysctl.
func (System) Uint32(name string) (uint32, error) { return unix.SysctlUint32(name) }

// Uint64 implements Sysctl.
func (System) Uint64(name string) (uint64, error) { return unix.SysctlUint64(name) }

// Raw implements Sysctl.
func (System) Raw(name string) ([]byte, error) { return unix.SysctlRaw(name) }
