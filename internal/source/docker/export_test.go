package docker

// RememberedDies is how many dies a Podman stream's podmanStops held at
// most while it saw a die of each of ids, a second apart from at, and how
// many it holds after a start of another container at the last die's time
// plus after (#335).
func RememberedDies(ids []string, at, after int64) (peak, last int) {
	p := newPodmanStops()
	for _, id := range ids {
		p.see("die", id, at)
		peak = max(peak, len(p.dies))
		at += 1e9
	}
	p.see("start", "other", at-1e9+after)
	return peak, len(p.dies)
}
