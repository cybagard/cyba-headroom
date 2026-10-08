package lease_test

import (
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

func ungatedKeys(b interface{ Ungated() []protocol.Ungated }) []string {
	var out []string
	for _, u := range b.Ungated() {
		out = append(out, u.Key)
	}
	return out
}

func TestANewContainerWithoutALeaseIsUngated(t *testing.T) {
	b, c, log := book(t)
	b.Observe(snap()) // baseline: "old" was there before
	c.t = c.t.Add(5 * time.Second)
	b.Observe(withContainer(snap(), "c2", "w1"))
	u := b.Ungated()
	if len(u) != 1 || u[0].Key != "container:c2" || u[0].Name != "c2" || u[0].Kind != "container" ||
		u[0].Worktree != "w1" || !u[0].Since.Equal(c.t) {
		t.Fatalf("ungated = %+v", u)
	}
	if !strings.Contains(log.String(), "ungated") || !strings.Contains(log.String(), "c2") {
		t.Fatalf("not logged: %s", log)
	}
	// Logged once, kept while it runs, with its first-seen time.
	n := strings.Count(log.String(), "ungated")
	c.t = c.t.Add(5 * time.Second)
	b.Observe(withContainer(snap(), "c2", "w1"))
	if u := b.Ungated(); len(u) != 1 || !u[0].Since.Equal(c.t.Add(-5*time.Second)) || strings.Count(log.String(), "ungated") != n {
		t.Fatalf("second tick: %+v\n%s", u, log)
	}
	// Gone: dropped.
	b.Observe(snap())
	if u := b.Ungated(); len(u) != 0 {
		t.Fatalf("a stopped container stays ungated: %+v", u)
	}
}

func TestAContainerThatBindsALeaseIsGated(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(req("w1", gib), snap(), cfg)
	b.Observe(withContainer(snap(), "c2", "w1"))
	if k := ungatedKeys(b); len(k) != 0 {
		t.Fatalf("ungated = %v", k)
	}
}

func TestWhatRanBeforeTheDaemonIsNotUngated(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(withContainer(snap(), "c2", "w1")) // the first reading
	if k := ungatedKeys(b); len(k) != 0 {
		t.Fatalf("ungated = %v", k)
	}
}

func TestAFailedReadDoesNotMakeContainersNew(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(withContainer(snap(), "c2", "w1"))
	missing := snap()
	missing.Docker.Containers = nil
	missing.Sources = map[string]protocol.SourceStatus{"docker": {Stale: true}}
	b.Observe(missing)
	b.Observe(withContainer(snap(), "c2", "w1"))
	if k := ungatedKeys(b); len(k) != 0 {
		t.Fatalf("ungated = %v", k)
	}
}

func TestAnUngatedContainerSurvivesAFailedRead(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Observe(withContainer(snap(), "c2", ""))
	missing := snap()
	missing.Docker.Containers = nil
	missing.Sources = map[string]protocol.SourceStatus{"docker": {Stale: true}}
	b.Observe(missing)
	if k := ungatedKeys(b); len(k) != 1 {
		t.Fatalf("ungated = %v after a failed read", k)
	}
}

func TestContainersDockerRestartsItselfAreNotUngated(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	down := snap()
	down.Docker = &protocol.Docker{} // Docker Desktop quit
	b.Observe(down)
	// It is back, and restarts its --restart always containers.
	b.Observe(withContainer(snap(), "c2", "w1"))
	if k := ungatedKeys(b); len(k) != 0 {
		t.Fatalf("ungated = %v", k)
	}
	// Once it has settled, new containers count again.
	c.t = c.t.Add(time.Minute)
	b.Observe(withContainer(withContainer(snap(), "c2", "w1"), "c3", "w1"))
	if k := ungatedKeys(b); len(k) != 1 || k[0] != "container:c3" {
		t.Fatalf("ungated = %v", k)
	}
}

func TestAnUngatedVM(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	s := snap()
	s.Tart.VMs = []protocol.TartVM{{Name: "vm", MemoryBytes: gib}}
	b.Observe(s)
	if u := b.Ungated(); len(u) != 1 || u[0].Key != "vm:vm" || u[0].Kind != "vm" {
		t.Fatalf("ungated = %+v", u)
	}
}

func TestAManualCallThroughTheShimIsNotUngated(t *testing.T) {
	for name, wt := range map[string]string{"unattributed": "", "in a worktree's directory": "w1"} {
		t.Run(name, func(t *testing.T) {
			b, _, _ := book(t)
			b.Observe(snap())
			m := named("", "", "alpine")
			m.CostBytes = 6 * gib
			if d := b.Check(m, snap(), cfg); !d.Allow {
				t.Fatalf("manual = %+v", d)
			}
			// It holds nothing back from gated calls.
			if d := b.Check(req("w2", 7*gib), snap(), cfg); !d.Allow {
				t.Fatalf("a manual call held back memory: %+v", d)
			}
			b.Observe(withNamed(snap(), "c2", "c2", "alpine", wt))
			for _, u := range b.Ungated() {
				if u.Key == "container:c2" {
					t.Fatalf("manual container flagged: %+v", u)
				}
			}
		})
	}
}

func TestAWorktreesOwnLeaseWinsOverAManualOne(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	b.Check(req("", gib), snap(), cfg)
	c.t = c.t.Add(time.Second)
	b.Check(req("w1", gib), snap(), cfg)
	b.Observe(withContainer(snap(), "c2", "w1"))
	for _, l := range b.List() {
		if l.Worktree == "w1" {
			t.Fatalf("w1's lease still open; the manual one took its container: %+v", b.List())
		}
	}
}

func TestAManualLeaseExpiresQuietly(t *testing.T) {
	b, c, log := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Kind: "container", Command: "docker run --rm alpine true"}, snap(), cfg)
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(snap())
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("a manual call's lease warned: %s", log)
	}
}

func named(wt, name, image string) policy.Request {
	return policy.Request{Worktree: wt, Kind: "container", Command: "docker run " + image, CostBytes: gib, Target: image, Name: name}
}

func withNamed(s *protocol.Snapshot, id, name, image, wt string) *protocol.Snapshot {
	return addContainer(s, protocol.Container{ID: id, Name: name, Image: image, MemoryBytes: gib / 2}, wt)
}

// Several containers in one tick: each lease binds its own by name, and
// the one started past the shim is the ungated one (#33).
func TestALeaseBindsItsOwnNamedContainer(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	b.Check(named("w1", "hr-gated", "alpine"), snap(), cfg)
	c.t = c.t.Add(time.Second)
	b.Check(named("", "hr-manual", "alpine"), snap(), cfg)
	s := snap()
	for _, n := range []string{"hr-login", "hr-direct", "hr-manual", "hr-gated"} {
		s = withNamed(s, n, n, "alpine", "")
	}
	b.Observe(s)
	got := ungatedKeys(b)
	if len(got) != 2 || got[0] != "container:hr-direct" || got[1] != "container:hr-login" {
		t.Fatalf("ungated = %v, want hr-direct and hr-login", got)
	}
}

func TestALeaseBindsItsImageFirst(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(named("w1", "", "postgres:17"), snap(), cfg)
	s := withNamed(withNamed(snap(), "a", "eager_turing", "redis", ""), "b", "calm_hopper", "docker.io/library/postgres:17", "")
	b.Observe(s)
	if got := ungatedKeys(b); len(got) != 1 || got[0] != "container:a" {
		t.Fatalf("ungated = %v, want the redis one", got)
	}
}

func TestStartBindsTheContainerItNames(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db", CostBytes: gib, Target: "db"}, snap(), cfg)
	b.Observe(withNamed(withNamed(snap(), "x", "other", "alpine", ""), "y", "db", "postgres", ""))
	if got := ungatedKeys(b); len(got) != 1 || got[0] != "container:x" {
		t.Fatalf("ungated = %v", got)
	}
}

func TestAComposeLeaseKnowsItsProjectFromTheCall(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, Target: "app"}, snap(), cfg)
	s := addContainer(snap(), protocol.Container{ID: "o1", Name: "other-db-1", Labels: map[string]string{"com.docker.compose.project": "other"}}, "")
	s = addContainer(s, protocol.Container{ID: "a1", Name: "app-db-1", Labels: map[string]string{"com.docker.compose.project": "app"}}, "")
	b.Observe(s)
	if got := ungatedKeys(b); len(got) != 1 || got[0] != "container:o1" {
		t.Fatalf("ungated = %v, want the other project's", got)
	}
}

func TestAnImageMismatchStillBinds(t *testing.T) {
	// Image names have many spellings (a mirror, a digest): only a name
	// says for sure that a container is not the call's.
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(named("w1", "", "my-registry.local/alpine"), snap(), cfg)
	b.Observe(withNamed(snap(), "a", "eager_turing", "alpine", ""))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

func TestAnImageNamesOnlyInItsOwnWorktree(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	b.Check(named("w1", "", "postgres"), snap(), cfg) // w1's, still pulling
	c.t = c.t.Add(time.Second)
	// An SDK in w2 starts postgres through the socket.
	b.Observe(withNamed(snap(), "sdk", "sdk-pg", "postgres", "w2"))
	if got := ungatedKeys(b); len(got) != 1 || got[0] != "container:sdk" {
		t.Fatalf("ungated = %v, want w2's SDK container", got)
	}
	// w1's own container then binds w1's lease.
	b.Observe(withNamed(withNamed(snap(), "sdk", "sdk-pg", "postgres", "w2"), "own", "eager_turing", "postgres", "w1"))
	if got := ungatedKeys(b); len(got) != 1 {
		t.Fatalf("ungated = %v", got)
	}
}

func TestAManualLeaseTakesOnlyWhatItNames(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	// docker run --rm hello-world by hand: gone before any tick sees it.
	b.Check(named("", "", "hello-world"), snap(), cfg)
	// Testcontainers in w1, through the socket, within the lease timeout.
	b.Observe(withNamed(snap(), "tc", "tc-redis", "redis", "w1"))
	if got := ungatedKeys(b); len(got) != 1 || got[0] != "container:tc" {
		t.Fatalf("ungated = %v", got)
	}
}

func TestAContainerMissingForATickIsNotUngated(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(withNamed(snap(), "db", "db", "postgres", "w1"))
	c.t = c.t.Add(5 * time.Second)
	b.Observe(snap()) // its stats failed, or it crashed and restarts
	c.t = c.t.Add(5 * time.Second)
	b.Observe(withNamed(snap(), "db", "db", "postgres", "w1"))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

func TestComposeServicesOnLaterTicksAreGated(t *testing.T) {
	for name, wt := range map[string]string{"manual": "", "worktree": "w1"} {
		t.Run(name, func(t *testing.T) {
			b, c, _ := book(t)
			b.Observe(snap())
			b.Check(policy.Request{Worktree: wt, Kind: "compose", Command: "docker compose up", CostBytes: gib}, snap(), cfg)
			app := func(s *protocol.Snapshot, id string) *protocol.Snapshot {
				return addContainer(s, protocol.Container{ID: id, Name: "app-" + id, MemoryBytes: 2 * gib,
					Labels: map[string]string{"com.docker.compose.project": "app"}}, wt)
			}
			b.Observe(app(snap(), "db")) // uses the whole cost at once
			c.t = c.t.Add(30 * time.Second)
			b.Observe(app(app(snap(), "db"), "web")) // depends_on db: later
			c.t = c.t.Add(3 * time.Minute)           // past the lease timeout
			b.Observe(app(app(app(snap(), "db"), "web"), "worker"))
			if got := ungatedKeys(b); len(got) != 0 {
				t.Fatalf("ungated = %v", got)
			}
		})
	}
}

func TestAnImageIsNotAContainerID(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(named("w1", "", "cafe"), snap(), cfg) // docker run cafe
	b.Observe(withNamed(snap(), "cafe1234", "x", "redis", "w2"))
	if got := ungatedKeys(b); len(got) != 1 {
		t.Fatalf("ungated = %v: an image matched a container ID", got)
	}
}

func TestManualLeasesAreNotListed(t *testing.T) {
	b, _, _ := book(t)
	b.Check(named("", "", "alpine"), snap(), cfg)
	if l := b.List(); len(l) != 0 {
		t.Fatalf("leases = %+v", l)
	}
}

func TestAnUngatedContainerFollowsItsAttribution(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Observe(withNamed(snap(), "x", "x", "alpine", "w1"))
	b.Observe(withNamed(snap(), "x", "x", "alpine", ""))
	if u := b.Ungated(); len(u) != 1 || u[0].Worktree != "" {
		t.Fatalf("ungated = %+v", u)
	}
}

func TestAnUngatedContainerStaysFlaggedAfterACrash(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	b.Observe(withNamed(snap(), "x", "x", "alpine", ""))
	since := b.Ungated()[0].Since
	c.t = c.t.Add(5 * time.Second)
	b.Observe(snap()) // crashed
	c.t = c.t.Add(5 * time.Second)
	b.Observe(withNamed(snap(), "x", "x", "alpine", "")) // its restart policy
	if u := b.Ungated(); len(u) != 1 || !u[0].Since.Equal(since) {
		t.Fatalf("ungated = %+v", u)
	}
}

func TestAnUngatedContainerStaysFlaggedAfterDockerRestarts(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Observe(withNamed(snap(), "x", "x", "alpine", ""))
	down := snap()
	down.Docker = &protocol.Docker{}
	b.Observe(down)
	b.Observe(withNamed(snap(), "x", "x", "alpine", ""))
	if got := ungatedKeys(b); len(got) != 1 {
		t.Fatalf("ungated = %v", got)
	}
}

func TestAVMRunAgainPastTheShimIsUngated(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "tart", Command: "tart run ci", CostBytes: gib, Target: "ci"}, snap(), cfg)
	vm := snap()
	vm.Tart.VMs = []protocol.TartVM{{Name: "ci", MemoryBytes: gib}}
	b.Observe(vm)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(snap()) // stopped
	c.t = c.t.Add(5 * time.Second)
	b.Observe(vm) // run again with the real tart
	if got := ungatedKeys(b); len(got) != 1 || got[0] != "vm:ci" {
		t.Fatalf("ungated = %v", got)
	}
}

func TestABaselineProjectDoesNotExemptLaterServices(t *testing.T) {
	b, _, _ := book(t)
	foo := func(s *protocol.Snapshot, id string) *protocol.Snapshot {
		return addContainer(s, protocol.Container{ID: id, Name: "foo-" + id, Labels: map[string]string{"com.docker.compose.project": "foo"}}, "")
	}
	b.Observe(foo(snap(), "a"))           // running before the daemon
	b.Observe(foo(foo(snap(), "a"), "b")) // a login shell's compose up
	if got := ungatedKeys(b); len(got) != 1 || got[0] != "container:b" {
		t.Fatalf("ungated = %v", got)
	}
}

func TestAManualComposeWithAProjectIsGated(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Kind: "compose", Command: "docker compose up", Target: "foo"}, snap(), cfg)
	b.Observe(addContainer(snap(), protocol.Container{ID: "a", Name: "foo-a", Labels: map[string]string{"com.docker.compose.project": "foo"}}, ""))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

func TestPodmansLocalImagesMatch(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(named("", "", "myimg"), snap(), cfg)
	b.Observe(withNamed(snap(), "a", "eager_turing", "localhost/myimg:latest", ""))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

func TestStartByAShortIDPrefix(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Kind: "container", Command: "docker start 3f", Target: "3f"}, snap(), cfg)
	b.Observe(withNamed(snap(), "3fab12", "db", "postgres", ""))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

func TestWithoutABaselineAnImageNamesOnlyTheWorktreesOwn(t *testing.T) {
	b, _, _ := book(t)
	none := snap()
	none.Docker = nil
	b.Check(named("w1", "", "postgres"), none, cfg) // no baseline yet
	s := addContainer(snap(), protocol.Container{ID: "earlier", Name: "earlier", Image: "postgres", MemoryBytes: 3 * gib}, "")
	s = withNamed(s, "new", "new", "postgres", "w1")
	b.Observe(s)
	if l := b.List(); len(l) != 1 || l[0].Bytes != gib/2 {
		t.Fatalf("leases = %+v, want w1's bound to its own container", l)
	}
}

func TestAManualLeaseWithoutATargetLeavesWorktreesContainers(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Kind: "container", Command: "docker run"}, snap(), cfg) // target unparsed
	b.Observe(withNamed(snap(), "tc", "tc-redis", "redis", "w1"))
	if got := ungatedKeys(b); len(got) != 1 {
		t.Fatalf("ungated = %v", got)
	}
}

// A check whose snapshot had a Tart reading but no Docker one is no
// baseline for containers: those running are not ungated.
func TestABaselineIsPerSource(t *testing.T) {
	b, _, _ := book(t)
	none := snap()
	none.Docker = nil
	b.Check(named("w1", "", "postgres"), none, cfg)
	b.Observe(withNamed(snap(), "earlier", "earlier", "redis", ""))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

func TestRestartPolicyContainersAfterDockerStartsAreNotUngated(t *testing.T) {
	b, c, _ := book(t)
	down := snap()
	down.Docker = &protocol.Docker{}
	b.Observe(down) // the daemon starts before Docker Desktop
	c.t = c.t.Add(5 * time.Second)
	b.Observe(snap()) // up, its containers not yet
	c.t = c.t.Add(10 * time.Second)
	b.Observe(withNamed(snap(), "r", "always", "redis", "")) // --restart always
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
	// Well after it came up, a new one counts again.
	c.t = c.t.Add(time.Minute)
	b.Observe(withNamed(withNamed(snap(), "r", "always", "redis", ""), "n", "sdk", "redis", ""))
	if got := ungatedKeys(b); len(got) != 1 || got[0] != "container:n" {
		t.Fatalf("ungated = %v", got)
	}
}

func TestAContainerAfterASlowPullIsGated(t *testing.T) {
	b, c, log := book(t)
	b.Observe(snap())
	b.Check(named("w1", "", "big-image"), snap(), cfg)
	c.t = c.t.Add(3 * time.Minute) // the pull outlasts the lease
	b.Observe(snap())
	c.t = c.t.Add(5 * time.Second)
	b.Observe(withNamed(snap(), "big", "eager_turing", "big-image", "w1"))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v\n%s", got, log)
	}
	if l := b.List(); len(l) != 0 {
		t.Fatalf("a lapsed lease reserves again: %+v", l)
	}
}

func TestImagesUnderAnotherRegistryMatch(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(named("", "", "myimg"), snap(), cfg)
	b.Observe(withNamed(snap(), "a", "eager_turing", "quay.io/org/myimg:latest", ""))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

func TestAWorktreesOwnLeaseNamesItsContainerFirst(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	b.Check(named("", "", "postgres"), snap(), cfg) // manual, older
	c.t = c.t.Add(time.Second)
	b.Check(named("w1", "", "postgres"), snap(), cfg)
	s := withNamed(snap(), "own", "own", "postgres", "w1")
	s = addContainer(s, protocol.Container{ID: "man", Name: "man", Image: "postgres", MemoryBytes: 3 * gib}, "")
	b.Observe(s)
	if l := b.List(); len(l) != 1 || l[0].Bytes != gib/2 {
		t.Fatalf("leases = %+v, want w1's bound to its own container", l)
	}
}

func TestACatchAllManualLeaseMatchesOnlyBriefly(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Kind: "container", Command: "docker run"}, snap(), cfg) // target unparsed
	c.t = c.t.Add(30 * time.Second)
	b.Observe(withNamed(snap(), "tc", "tc-redis", "redis", ""))
	if got := ungatedKeys(b); len(got) != 1 {
		t.Fatalf("ungated = %v", got)
	}
}

func labelled(wt, image string) policy.Request {
	r := named(wt, "", image)
	r.Labelled = true
	return r
}

func withLabel(s *protocol.Snapshot, id, image, lease, wt string) *protocol.Snapshot {
	c := protocol.Container{ID: id, Name: id, Image: image, MemoryBytes: gib / 2}
	if lease != "" {
		c.Labels = map[string]string{protocol.LeaseLabel: lease}
	}
	return addContainer(s, c, wt)
}

// Two unnamed containers of one image in one tick, one through the shim:
// its label says which.
func TestTheLabelSaysWhichContainerIsGated(t *testing.T) {
	for _, order := range [][2]string{{"gated", "direct"}, {"direct", "gated"}} {
		b, _, _ := book(t)
		b.Observe(snap())
		d := b.Check(labelled("w1", "alpine"), snap(), cfg)
		s := snap()
		for _, id := range order {
			lease := ""
			if id == "gated" {
				lease = d.LeaseID
			}
			s = withLabel(s, id, "alpine", lease, "")
		}
		b.Observe(s)
		if got := ungatedKeys(b); len(got) != 1 || got[0] != "container:direct" {
			t.Fatalf("order %v: ungated = %v", order, got)
		}
	}
}

func TestALabelledLeaseBindsOnlyItsLabel(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	b.Check(labelled("w1", "alpine"), snap(), cfg)
	b.Observe(withLabel(snap(), "direct", "alpine", "", "w1"))
	if got := ungatedKeys(b); len(got) != 1 {
		t.Fatalf("ungated = %v", got)
	}
	// Nor through lapsing.
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(withLabel(snap(), "direct", "alpine", "", "w1"))
	b.Observe(withLabel(withLabel(snap(), "direct", "alpine", "", "w1"), "late", "alpine", "", "w1"))
	if got := ungatedKeys(b); len(got) != 2 {
		t.Fatalf("ungated = %v", got)
	}
}

func TestALabelWhoseLeaseIsNotOpenCountsForNothing(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	// Forged, or inherited from an image committed from a gated container.
	b.Observe(withLabel(snap(), "x", "alpine", "lease-0ld-1", ""))
	if got := ungatedKeys(b); len(got) != 1 {
		t.Fatalf("ungated = %v", got)
	}
}

func TestAStartLeaseTakesALabelledContainer(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	// docker create, then docker start: the container runs at the start.
	created := b.Check(labelled("w1", "postgres"), snap(), cfg)
	c.t = c.t.Add(time.Second)
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db", CostBytes: gib, Target: "db"}, snap(), cfg)
	s := addContainer(snap(), protocol.Container{ID: "db1", Name: "db", Image: "postgres", MemoryBytes: gib / 2,
		Labels: map[string]string{protocol.LeaseLabel: created.LeaseID}}, "w1")
	b.Observe(s)
	ls := b.List()
	if len(ls) != 1 || ls[0].Command != "docker start db" || ls[0].Bytes != gib/2 {
		t.Fatalf("leases = %+v, want only the start's, bound", ls)
	}
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

func TestALabelledContainerStartedAgainLaterUsesItsStartLease(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	// Created through the shim long ago: its lease is gone.
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db", CostBytes: gib, Target: "db"}, snap(), cfg)
	b.Observe(withLabel(snap(), "db", "postgres", "lease-0ld-1", "w1"))
	if l := b.List(); len(l) != 1 || l[0].Bytes != gib/2 {
		t.Fatalf("leases = %+v, want the start's bound", l)
	}
}

func TestALabelBindsOnlyAnUnboundContainerLease(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	d := b.Check(labelled("w1", "alpine"), snap(), cfg)
	b.Observe(withLabel(snap(), "one", "alpine", d.LeaseID, ""))
	// A copy with the same labels: the lease already has its container.
	b.Observe(withLabel(withLabel(snap(), "one", "alpine", d.LeaseID, ""), "copy", "alpine", d.LeaseID, ""))
	if got := ungatedKeys(b); len(got) != 1 || got[0] != "container:copy" {
		t.Fatalf("ungated = %v", got)
	}
}
