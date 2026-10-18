package vmproc

import "time"

// Dirs is dirs, for tests.
type Dirs = dirs

// SetDirs replaces f's directory reads, for tests.
func SetDirs(f *Finder, d Dirs) *Finder {
	f.dirs = d
	return f
}

// SetNow replaces f's clock for CPU readings, for tests.
func SetNow(f *Finder, now func() time.Time) *Finder {
	f.now = now
	return f
}
