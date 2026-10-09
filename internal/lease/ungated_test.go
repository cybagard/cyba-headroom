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
	d := b.Check(req("w1", gib), snap(), cfg)
	b.Observe(withRun(snap(), "c2", "w1", 64*gib, d.LeaseID))
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
			m := labelled("", "alpine")
			m.CostBytes = 6 * gib
			d := b.Check(m, snap(), cfg)
			if !d.Allow {
				t.Fatalf("manual = %+v", d)
			}
			// It holds nothing back from gated calls.
			if d := b.Check(req("w2", 7*gib), snap(), cfg); !d.Allow {
				t.Fatalf("a manual call held back memory: %+v", d)
			}
			b.Observe(withLabel(snap(), "c2", "alpine", d.LeaseID, wt))
			for _, u := range b.Ungated() {
				if u.Key == "container:c2" {
					t.Fatalf("manual container flagged: %+v", u)
				}
			}
		})
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

// After a stall past the lease timeout, a reading without x lets its
// verdict go: when x is back it is judged again, and with no lease it is
// ungated.
func TestAContainerBackAfterAStallIsUngated(t *testing.T) {
	b, c, log := book(t)
	x := func() *protocol.Snapshot { return addContainer(snap(), protocol.Container{ID: "X", Name: "x"}, "w1") }
	b.Observe(read(x(), c.t))
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(x(), c.t))
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(read(snap(), c.t)) // x missing
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(x(), c.t)) // x back
	if got := ungatedKeys(b); len(got) != 1 || !strings.Contains(log.String(), "ungated") {
		t.Fatalf("ungated = %v, want x warned: %s", got, log)
	}
}

func TestComposeServicesOnLaterTicksAreGated(t *testing.T) {
	for name, wt := range map[string]string{"manual": "", "worktree": "w1"} {
		t.Run(name, func(t *testing.T) {
			b, c, _ := book(t)
			b.Observe(snap())
			b.Check(policy.Request{Worktree: wt, Kind: "compose", Command: "docker compose up", CostBytes: gib, Target: "app"}, snap(), cfg)
			app := func(s *protocol.Snapshot, id string) *protocol.Snapshot {
				return addContainer(s, protocol.Container{ID: id, Name: "app-" + id, MemoryBytes: 2 * gib,
					Labels: map[string]string{"com.docker.compose.project": "app", protocol.ComposeWorkingDirLabel: "/Users/dev/src/app"}}, wt)
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
	cr := labelled("w1", "postgres")
	cr.Command = "docker create postgres"
	created := b.Check(cr, snap(), cfg)
	c.t = c.t.Add(time.Second)
	// The gate asked Docker: db1 is the container, and the create's label
	// is on it.
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db", CostBytes: gib, Target: "db",
		ContainerID: "db1", TakesOver: created.LeaseID}, snap(), cfg)
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

// The shim labels every run: a slow pull's container still finds its
// lapsed lease by its label.
func TestALabelledContainerAfterASlowPullIsGated(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	d := b.Check(labelled("w1", "big-image"), snap(), cfg)
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(snap())
	c.t = c.t.Add(5 * time.Second)
	b.Observe(withLabel(snap(), "big", "big-image", d.LeaseID, "w1"))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
	if l := b.List(); len(l) != 0 {
		t.Fatalf("a lapsed lease reserves again: %+v", l)
	}
}

// Only a create's lease hands its container to a start: a run's is its own.
func TestAStartLeaseDoesNotTakeARunsContainer(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	// docker start db || docker run --name db postgres: the start failed.
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db", CostBytes: gib, Target: "db"}, snap(), cfg)
	c.t = c.t.Add(time.Second)
	r := labelled("w1", "postgres")
	r.Name, r.CostBytes = "db", 4*gib
	run := b.Check(r, snap(), cfg)
	b.Observe(addContainer(snap(), protocol.Container{ID: "db1", Name: "db", Image: "postgres", MemoryBytes: gib / 2,
		Labels: map[string]string{protocol.LeaseLabel: run.LeaseID}}, "w1"))
	for _, l := range b.List() {
		if l.ID == run.LeaseID && l.Bytes == 4*gib-gib/2 {
			return
		}
	}
	t.Fatalf("the run's lease lost its container: %+v", b.List())
}

func TestAStartLeaseTakesOnlyItsOwnWorktreesCreated(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w2", Kind: "container", Command: "docker start db", CostBytes: gib, Target: "db"}, snap(), cfg)
	c.t = c.t.Add(time.Second)
	r := labelled("w1", "postgres")
	r.Command = "docker create postgres"
	created := b.Check(r, snap(), cfg)
	b.Observe(addContainer(snap(), protocol.Container{ID: "db1", Name: "db", Image: "postgres", MemoryBytes: gib / 2,
		Labels: map[string]string{protocol.LeaseLabel: created.LeaseID}}, "w1"))
	for _, l := range b.List() {
		if l.ID == created.LeaseID {
			return // kept its container
		}
	}
	t.Fatalf("w2's start took w1's created container: %+v", b.List())
}

func TestAShimmedRunWithAComposeLabelIsGated(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	d := b.Check(labelled("w1", "alpine"), snap(), cfg)
	b.Observe(addContainer(snap(), protocol.Container{ID: "x", Name: "x", Image: "alpine",
		Labels: map[string]string{protocol.LeaseLabel: d.LeaseID, "com.docker.compose.project": "dev"}}, "w1"))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

func startOf(wt, name string) policy.Request {
	return policy.Request{Worktree: wt, Kind: "container", Command: "docker start " + name, CostBytes: gib, Target: name}
}

func TestAStartAfterTheCreateLapsedTakesTheContainer(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	cr := labelled("w1", "postgres")
	cr.Command = "docker create postgres"
	created := b.Check(cr, snap(), cfg)
	c.t = c.t.Add(3 * time.Minute) // the create's lease lapsed
	b.Observe(snap())
	b.Check(startOf("w1", "db"), snap(), cfg)
	b.Observe(addContainer(snap(), protocol.Container{ID: "db1", Name: "db", Image: "postgres", MemoryBytes: gib / 2,
		Labels: map[string]string{protocol.LeaseLabel: created.LeaseID}}, "w1"))
	if l := b.List(); len(l) != 1 || l[0].Bytes != gib/2 {
		t.Fatalf("leases = %+v, want the start's bound", l)
	}
}

func TestAStartAfterAnUnboundRunTakesTheContainer(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	r := labelled("w1", "img")
	r.Name = "x"
	run := b.Check(r, snap(), cfg) // its container exited before a tick
	c.t = c.t.Add(30 * time.Second)
	st := startOf("w1", "x")
	st.ContainerID, st.TakesOver = "x1", run.LeaseID
	b.Check(st, snap(), cfg)
	b.Observe(addContainer(snap(), protocol.Container{ID: "x1", Name: "x", Image: "img", MemoryBytes: gib / 2,
		Labels: map[string]string{protocol.LeaseLabel: run.LeaseID}}, "w1"))
	ls := b.List()
	if len(ls) != 1 || ls[0].Command != "docker start x" || ls[0].Bytes != gib/2 {
		t.Fatalf("leases = %+v, want only the start's, bound", ls)
	}
}

func TestAReturningContainerIsNotTakenByAGuess(t *testing.T) {
	b, c, _ := book(t)
	old := func(s *protocol.Snapshot) *protocol.Snapshot {
		return addContainer(s, protocol.Container{ID: "X", Name: "old-x", Labels: map[string]string{"com.docker.compose.project": "old"}}, "w1")
	}
	b.Observe(old(snap()))
	c.t = c.t.Add(5 * time.Second)
	b.Observe(snap()) // crash-looping
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, Target: "new"}, snap(), cfg)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(old(snap())) // back: must not take the new compose lease
	c.t = c.t.Add(5 * time.Second)
	b.Observe(addContainer(old(snap()), protocol.Container{ID: "N", Name: "new-web", Labels: map[string]string{"com.docker.compose.project": "new"}}, "w1"))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}
