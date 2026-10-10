package lease

// SetAlive replaces b's process check, for tests.
func SetAlive(b *Book, f func(pid int) bool) { b.alive = f }

// HasVerdict reports whether b keeps a verdict on the resource key.
func HasVerdict(b *Book, key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.verdicts[key] != nil
}
