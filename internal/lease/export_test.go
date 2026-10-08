package lease

// SetAlive replaces b's process check, for tests.
func SetAlive(b *Book, f func(pid int) bool) { b.alive = f }
