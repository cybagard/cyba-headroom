package vmproc

// Dirs is dirs, for tests.
type Dirs = dirs

// SetDirs replaces f's directory reads, for tests.
func SetDirs(f *Finder, d Dirs) *Finder {
	f.dirs = d
	return f
}
