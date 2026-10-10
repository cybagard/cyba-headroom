package lease

import "slices"

// SetAlive replaces b's process check, for tests.
func SetAlive(b *Book, f func(pid int) bool) { b.alive = f }

// HasVerdict reports whether b keeps a verdict on the resource key.
func HasVerdict(b *Book, key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.verdicts[key] != nil
}

// Bound returns the resource keys the open lease id has bound, sorted.
func Bound(b *Book, id string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var keys []string
	for _, e := range b.open {
		if e.ID == id {
			for k := range e.bound {
				keys = append(keys, k)
			}
		}
	}
	slices.Sort(keys)
	return keys
}
