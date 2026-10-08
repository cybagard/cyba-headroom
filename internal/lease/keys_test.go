package lease_test

import (
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/lease"
	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// compose stop, then compose up in the same worktree. The returning
// containers bind the up's lease, keyed by the project the shim named.
func TestComposeUpAfterStopBindsByItsProject(t *testing.T) {
	b, c, _ := book(t)
	app := func(s *protocol.Snapshot) *protocol.Snapshot {
		return addContainer(s, protocol.Container{ID: "A1", Name: "app-db-1", MemoryBytes: gib / 2,
			Labels: map[string]string{"com.docker.compose.project": "app", protocol.ComposeWorkingDirLabel: "/Users/dev/src/a"}}, "w1")
	}
	b.Observe(app(snap()))
	c.t = c.t.Add(5 * time.Second)
	b.Observe(snap()) // docker compose stop
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, Target: "app"}, snap(), cfg)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(app(snap())) // docker compose up -d: the same containers
	if l := b.List(); len(l) != 1 || l[0].Bytes != gib/2 {
		t.Fatalf("leases = %+v, want the up's lease bound to its container", l)
	}
}

// docker start $(docker create postgres): the start
// names its container by full ID.
func TestAStartByIDTakesOverItsCreate(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	cr := labelled("w1", "postgres")
	cr.Command = "docker create postgres"
	created := b.Check(cr, snap(), cfg)
	c.t = c.t.Add(time.Second)
	id := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	// The gate resolved the target with Docker.
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start " + id[:12], CostBytes: gib, Target: id[:12],
		ContainerID: id, TakesOver: created.LeaseID}, snap(), cfg)
	b.Observe(addContainer(snap(), protocol.Container{ID: id, Name: "eager_turing", Image: "postgres", MemoryBytes: gib / 2,
		Labels: map[string]string{protocol.LeaseLabel: created.LeaseID}}, "w1"))
	ls := b.List()
	if len(ls) != 1 || ls[0].Bytes != gib/2 {
		t.Fatalf("leases = %+v, want one lease, bound", ls)
	}
}

// docker run -m 6g --name db, then docker start db
// before a tick: the reservation must stay the run's 6 GiB (8 GiB headroom).
func TestARunsReservationSurvivesAStart(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	r := labelled("w1", "img")
	r.Name, r.CostBytes = "db", 6*gib
	run := b.Check(r, snap(), cfg)
	if run.LeaseID == "" {
		t.Fatalf("run denied: %+v", run)
	}
	c.t = c.t.Add(time.Second)
	st := startOf("w1", "db")
	st.ContainerID, st.TakesOver = "db1", run.LeaseID
	b.Check(st, snap(), cfg)
	b.Observe(addContainer(snap(), protocol.Container{ID: "db1", Name: "db", Image: "img", MemoryBytes: gib / 2,
		Labels: map[string]string{protocol.LeaseLabel: run.LeaseID}}, "w1"))
	var reserved uint64
	for _, l := range b.List() {
		reserved += l.Bytes
	}
	if reserved < 6*gib-gib/2 {
		t.Fatalf("reserved %.1f GiB, want the run's 6 GiB less what it uses", float64(reserved)/float64(gib))
	}
}

// compose up, stop, up again while the first up's lease is still
// open. The second up's lease takes the first's over: one lease, holding
// the project's container, and nothing logged as never appeared.
func TestComposeUpAgainTakesOverTheOpenLease(t *testing.T) {
	b, c, log := book(t)
	b.Observe(snap())
	up := policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, Target: "a"}
	app := func(s *protocol.Snapshot) *protocol.Snapshot {
		return addContainer(s, protocol.Container{ID: "A1", Name: "a-web-1", MemoryBytes: gib / 4,
			Labels: map[string]string{"com.docker.compose.project": "a", protocol.ComposeWorkingDirLabel: "/Users/dev/src/a"}}, "w1")
	}
	b.Check(up, snap(), cfg)
	b.Observe(app(snap()))
	c.t = c.t.Add(5 * time.Second)
	b.Observe(snap()) // compose stop
	b.Check(up, snap(), cfg)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(app(snap())) // compose up -d again
	// One stack: the larger of the two estimates, less what it uses.
	if l := b.List(); len(l) != 1 || l[0].Bytes != gib-gib/4 {
		t.Fatalf("leases = %+v, want one, holding the container", l)
	}
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(app(snap()))
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("logged: %s", log)
	}
}

// A compose lease has one key: its project if it names one, else its
// directory. Two projects from one directory are two leases.
func TestComposeProjectsFromOneDirectoryStayApart(t *testing.T) {
	for name, second := range map[string]policy.Request{
		"-p a then -p b": {Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, Target: "b"},
	} {
		t.Run(name, func(t *testing.T) {
			b, _, _ := book(t)
			b.Observe(snap())
			b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, Target: "a"}, snap(), cfg)
			b.Check(second, snap(), cfg)
			if r := reserved(b); r != 2*gib {
				t.Fatalf("reserved %d GiB, want both stacks' 2", r>>30)
			}
		})
	}
}

// docker restart (or start) of a running container starts nothing new: no
// lease, so nothing is double-counted.
func TestRestartingARunningContainerTakesNoLease(t *testing.T) {
	b, _, _ := book(t)
	s := addContainer(snap(), protocol.Container{ID: "C", Name: "c", MemoryBytes: 2 * gib}, "w1")
	b.Observe(s)
	d := b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker restart c", CostBytes: gib, Target: "c", ContainerID: "C", Running: true}, s, cfg)
	if !d.Allow || d.LeaseID != "" || len(b.List()) != 0 {
		t.Fatalf("decision %+v, leases %+v", d, b.List())
	}
}

// The baseline comes from the first reading only: later checks do not
// rebuild it, even when a source (Tart) never reads.
func TestALaterCheckIsNoBaseline(t *testing.T) {
	b, _, _ := book(t)
	noTart := snap()
	noTart.Tart = nil
	b.Observe(noTart)
	// A VM shows up in a check's snapshot: it is not a baseline, so when
	// Tart reads, it is new.
	withVM := snap()
	withVM.Tart.VMs = []protocol.TartVM{{Name: "v", MemoryBytes: gib}}
	b.Check(req("w1", gib), withVM, cfg)
	b.Observe(withVM)
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v: before Tart's first reading, a VM is a baseline", got)
	}
}

// A manual call does not take a worktree's lease over: it reserves
// nothing, and the worktree's reservation must stand.
func TestAManualCallTakesNoWorktreeLeaseOver(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 4 * gib, Target: "p"}, snap(), cfg)
	cr := labelled("w1", "pg")
	cr.Command, cr.CostBytes = "docker create pg", 2*gib
	created := b.Check(cr, snap(), cfg)
	b.Check(policy.Request{Kind: "compose", Command: "docker compose up", Target: "p"}, snap(), cfg)
	b.Check(policy.Request{Kind: "container", Command: "docker start db", Target: "db", ContainerID: "db1", TakesOver: created.LeaseID}, snap(), cfg)
	if r := reserved(b); r != 6*gib {
		t.Fatalf("reserved %d GiB, want the worktree's 6", r>>30)
	}
}

// Whether the container runs is Docker's answer at the check, not a
// snapshot's: docker stop db && docker start db within one tick.
func TestAStartOfAContainerDockerSaysIsStoppedTakesALease(t *testing.T) {
	b, _, _ := book(t)
	s := addContainer(snap(), protocol.Container{ID: "C", Name: "db", MemoryBytes: gib}, "w1")
	b.Observe(s) // still lists db: stopped since
	d := b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db", CostBytes: gib, Target: "db", ContainerID: "C"}, s, cfg)
	if d.LeaseID == "" {
		t.Fatalf("no lease: %+v", d)
	}
	d = b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db", CostBytes: gib, Target: "db", ContainerID: "C", Running: true}, s, cfg)
	if d.LeaseID != "" {
		t.Fatalf("a lease for a running container: %+v", d)
	}
	d = b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db", CostBytes: gib, Target: "db", ContainerID: "C", Running: true, MultiTarget: true}, s, cfg)
	if d.LeaseID == "" {
		t.Fatalf("docker start db other: the others may be stopped, want a lease: %+v", d)
	}
}

// A lease that took another over, released because its call failed, gives
// the other back.
func TestReleaseGivesATakenOverLeaseBack(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 6 * gib, Target: "p"}, snap(), cfg)
	d := b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, Target: "p"}, snap(), cfg)
	if !b.Release(d.LeaseID) {
		t.Fatal("release failed")
	}
	if r := reserved(b); r != 6*gib {
		t.Fatalf("reserved %d GiB, want the first call's 6 back", r>>30)
	}
}

// A later service counts as gated only for the same project from the same
// directory: another stack that happens to share the name does not.
func TestAGatedProjectIsKeyedByItsDirectory(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, Target: "app"}, snap(), cfg)
	ctr := func(s *protocol.Snapshot, id, dir string) *protocol.Snapshot {
		return addContainer(s, protocol.Container{ID: id, Name: id, MemoryBytes: 2 * gib,
			Labels: map[string]string{"com.docker.compose.project": "app", protocol.ComposeWorkingDirLabel: dir}}, "")
	}
	b.Observe(ctr(snap(), "x1", "/repo-x"))
	c.t = c.t.Add(time.Minute)
	b.Observe(ctr(ctr(snap(), "x1", "/repo-x"), "y1", "/repo-y")) // another app, past the shim
	if got := ungatedKeys(b); len(got) != 1 || got[0] != "container:y1" {
		t.Fatalf("ungated = %v", got)
	}
}

// docker stop db && docker start db within one tick: db never leaves the
// snapshot, yet the start's lease binds it by its ID.
func TestALeaseKeyedByIDBindsAContainerThatNeverLeft(t *testing.T) {
	b, _, _ := book(t)
	s := addContainer(snap(), protocol.Container{ID: "C", Name: "db", MemoryBytes: gib / 2}, "w1")
	b.Observe(s)
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db", CostBytes: gib, Target: "db", ContainerID: "C"}, s, cfg)
	b.Observe(s)
	if l := b.List(); len(l) != 1 || l[0].Bytes != gib/2 {
		t.Fatalf("leases = %+v, want the start's bound to db", l)
	}
}

// A container's label names its lease before a start's container ID: a
// manual start of a worktree's created container leaves the create's lease
// to bind it.
func TestALabelBeatsAnIDKey(t *testing.T) {
	b, _, log := book(t)
	b.Observe(snap())
	cr := labelled("w1", "pg")
	cr.Command = "docker create pg"
	created := b.Check(cr, snap(), cfg)
	b.Check(policy.Request{Kind: "container", Command: "docker start x", Target: "x", ContainerID: "X", TakesOver: created.LeaseID}, snap(), cfg)
	b.Observe(withRun(snap(), "X", "w1", gib/2, created.LeaseID))
	for _, l := range b.List() {
		if l.ID == created.LeaseID && l.Bytes == gib/2 {
			return
		}
	}
	t.Fatalf("the create's lease lost its container: %+v\n%s", b.List(), log)
}

// docker start a b, a already running (and ungated): the lease is not
// keyed by a, so a keeps its verdict and the lease holds its cost for b.
func TestAStartOfARunningAndAStoppedContainer(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	s := addContainer(snap(), protocol.Container{ID: "A", Name: "a", MemoryBytes: 4 * gib}, "w1")
	b.Observe(s) // a appeared past the shim: ungated
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start a", CostBytes: gib, Target: "a", ContainerID: "A", Running: true, MultiTarget: true}, s, cfg)
	b.Observe(s)
	if got := ungatedKeys(b); len(got) != 1 || got[0] != "container:A" {
		t.Fatalf("ungated = %v, want a still flagged", got)
	}
	if r := reserved(b); r != gib {
		t.Fatalf("reserved %d, want the start's full cost for the others", r)
	}
}

// A release gives back a lapsed lease the call took over too.
func TestReleaseGivesALapsedTakenOverLeaseBack(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	cr := labelled("w1", "pg")
	cr.Command = "docker create pg"
	created := b.Check(cr, snap(), cfg)
	c.t = c.t.Add(3 * time.Minute) // never started: lapsed
	b.Observe(snap())
	st := b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start x", Target: "x", ContainerID: "X", TakesOver: created.LeaseID}, snap(), cfg)
	b.Release(st.LeaseID) // its exec failed
	b.Observe(withRun(snap(), "X", "w1", gib/2, created.LeaseID))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v: the create was checked", got)
	}
}

// A lease with no key (docker start a b, a running) holds its cost to the
// timeout and then ends quietly: there was nothing it could see appear.
func TestAnUnkeyedLeaseEndsQuietly(t *testing.T) {
	b, c, log := book(t)
	s := addContainer(snap(), protocol.Container{ID: "A", Name: "a", MemoryBytes: gib}, "w1")
	b.Observe(s)
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start a", CostBytes: gib, Target: "a", ContainerID: "A", Running: true, MultiTarget: true}, s, cfg)
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(s)
	if len(b.List()) != 0 || strings.Contains(log.String(), "never appeared") {
		t.Fatalf("leases %+v, log %s", b.List(), log)
	}
}

// docker run db, then docker stop db && docker start db while the run's
// lease is open: that lease still covers db, so the start takes none.
func TestAStartOfAContainerAnOpenLeaseHoldsTakesNoLease(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	run := b.Check(req("w1", 4*gib), snap(), cfg)
	s := withRun(snap(), "db", "w1", gib, run.LeaseID)
	b.Observe(s)
	d := b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db", CostBytes: 4 * gib, Target: "db", ContainerID: "db"}, s, cfg)
	if !d.Allow || d.LeaseID != "" || reserved(b) != 3*gib {
		t.Fatalf("decision %+v, reserved %d GiB, want no new lease and the run's 3", d, reserved(b)>>30)
	}
}

// docker run -d --name a, then docker start a b before a tick: a runs, so
// the start takes nothing over and a binds its run's lease.
func TestARunningTargetIsTakenNothingFrom(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	run := b.Check(req("w1", gib), snap(), cfg)
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start a", CostBytes: gib, Target: "a", ContainerID: "A",
		Running: true, MultiTarget: true, TakesOver: run.LeaseID}, snap(), cfg)
	b.Observe(withRun(snap(), "A", "w1", gib/2, run.LeaseID))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

// Taking over several leases adds up what their containers use.
func TestATakeoverAddsUpUse(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	ctr := func(s *protocol.Snapshot, id string) *protocol.Snapshot {
		return addContainer(s, protocol.Container{ID: id, Name: id, MemoryBytes: gib,
			Labels: map[string]string{"com.docker.compose.project": "p" + id, protocol.ComposeWorkingDirLabel: "/r"}}, "w1")
	}
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 4 * gib, Target: "px"}, snap(), cfg)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 4 * gib, Target: "py"}, snap(), cfg)
	b.Observe(ctr(ctr(snap(), "x"), "y"))
	// px again, estimated below what its lease still reserves: the new
	// lease holds that (3) on top of what x uses (1), less x's use.
	if d := b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 2 * gib, Target: "px"}, snap(), cfg); !d.Allow {
		t.Fatalf("px again: %+v", d)
	}
	if r := reserved(b); r != 3*gib+3*gib {
		t.Fatalf("reserved %d GiB, want px's 3 and py's 3: %+v", r>>30, b.List())
	}
}

// docker start tiny big under pressure, tiny held by an open lease: big is
// not covered by tiny's lease, so the call is decided, not waved through.
func TestAMultiTargetStartIsDecidedEvenWhenItsFirstIsHeld(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	run := b.Check(req("w1", 2*gib), snap(), cfg)
	s := withRun(snap(), "tiny", "w1", gib/8, run.LeaseID)
	b.Observe(s)
	d := b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start tiny", CostBytes: gib, Target: "tiny",
		ContainerID: "tiny", MultiTarget: true}, s, cfg)
	if d.Allow && d.LeaseID == "" {
		t.Fatalf("waved through with no lease: %+v", d)
	}
	// Under pressure, it is denied like any other start.
	b2, _, _ := book(t)
	critical := withRun(snap(), "tiny", "w1", gib/8, "")
	critical.Host.Pressure = "critical"
	b2.Observe(critical)
	if d := b2.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start tiny", CostBytes: gib, Target: "tiny",
		ContainerID: "tiny", MultiTarget: true}, critical, cfg); d.Allow {
		t.Fatalf("allowed under critical pressure: %+v", d)
	}
}

// A lease past its timeout covers nothing, even before Observe expires it
// (settling stalled).
func TestAnExpiredLeaseCoversNoStart(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	run := b.Check(req("w1", 2*gib), snap(), cfg)
	b.Observe(withRun(snap(), "db", "w1", gib/8, run.LeaseID))
	c.t = c.t.Add(3 * time.Minute) // no Observe since
	d := b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db", CostBytes: gib, Target: "db", ContainerID: "db"}, snap(), cfg)
	if d.LeaseID == "" {
		t.Fatalf("covered by an expired lease: %+v", d)
	}
}

// Another worktree's start of a container an open lease holds takes no
// lease either: the holder's lease covers it and its worktree is charged
// for it, so the starter adds nothing.
func TestAnotherWorktreesStartOfAHeldContainerAddsNothing(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	run := b.Check(req("w1", 4*gib), snap(), cfg)
	s := withRun(snap(), "x", "w1", gib, run.LeaseID)
	b.Observe(s)
	d := b.Check(policy.Request{Worktree: "w2", Kind: "container", Command: "docker start x", CostBytes: gib, Target: "x", ContainerID: "x"}, s, cfg)
	if !d.Allow || d.LeaseID != "" || reserved(b) != 3*gib {
		t.Fatalf("decision %+v, reserved %d GiB, want no second reservation", d, reserved(b)>>30)
	}
}

// docker start held other: the lease is for the others, not keyed by the
// held one, and ends quietly.
func TestAMultiTargetStartOfAHeldContainerEndsQuietly(t *testing.T) {
	b, c, log := book(t)
	b.Observe(snap())
	run := b.Check(req("w1", 2*gib), snap(), cfg)
	s := withRun(snap(), "held", "w1", gib/8, run.LeaseID)
	b.Observe(s)
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start held", CostBytes: gib, Target: "held", ContainerID: "held", MultiTarget: true}, s, cfg)
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(s)
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("logged: %s", log)
	}
}

// A compose takeover never lowers what is reserved, and stays in one
// worktree: another worktree's call keeps its own lease, against its own
// cap.
func TestAComposeTakeoverStaysInItsWorktree(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app"}, snap(), cfg)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: gib, Target: "app"}, snap(), cfg)
	if l := b.List(); len(l) != 1 || l[0].Bytes != 2*gib {
		t.Fatalf("leases = %+v, want one holding the 2 GiB still reserved", l)
	}
	b.Check(policy.Request{Worktree: "w2", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: gib, Target: "app"}, snap(), cfg)
	if l := b.List(); len(l) != 2 || reserved(b) != 3*gib {
		t.Fatalf("leases = %+v, want w2's apart", l)
	}
}

// docker compose up again and again on a running stack: the reservation
// stays bounded by one call's estimate, not one per call.
func TestRepeatedComposeUpStaysBounded(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	up := policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app"}
	web := addContainer(snap(), protocol.Container{ID: "w", Name: "app-web-1", MemoryBytes: gib / 2,
		Labels: map[string]string{"com.docker.compose.project": "app"}}, "w1")
	b.Check(up, snap(), cfg)
	b.Observe(web)
	for range 4 {
		b.Check(up, web, cfg)
		b.Observe(web)
	}
	if r := reserved(b); r > 2*gib {
		t.Fatalf("reserved %d GiB after five ups of one stack", r>>30)
	}
}

// docker compose run starts a one-off container, which Compose labels:
// it is its own lease's, beside the up's, and both reservations stand.
func TestComposeRunHasItsOwnLease(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app"}, snap(), cfg)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "run", Command: "docker compose run", CostBytes: gib, Target: "app"}, snap(), cfg)
	if l := b.List(); len(l) != 2 || reserved(b) != 3*gib {
		t.Fatalf("leases = %+v", l)
	}
	s := addContainer(snap(), protocol.Container{ID: "m", Name: "app-migrate-run-1", MemoryBytes: gib / 4,
		Labels: map[string]string{"com.docker.compose.project": "app", "com.docker.compose.oneoff": "True"}}, "w1")
	b.Observe(s)
	for _, l := range b.List() {
		if l.Command == "docker compose run" && l.Bytes != gib-gib/4 {
			t.Fatalf("the run's lease did not take its one-off container: %+v", b.List())
		}
	}
}

// Two worktrees bring up the same project: each one's containers bind its
// own lease (by the worktree they are attributed to).
func TestTheSameProjectInTwoWorktrees(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "myapp"}, snap(), cfg)
	c.t = c.t.Add(time.Second)
	b.Check(policy.Request{Worktree: "w2", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "myapp"}, snap(), cfg)
	b.Observe(addContainer(snap(), protocol.Container{ID: "b1", Name: "myapp-web-1", MemoryBytes: gib / 2,
		Labels: map[string]string{"com.docker.compose.project": "myapp"}}, "w2"))
	for _, l := range b.List() {
		if l.Worktree == "w2" && l.Bytes != 2*gib-gib/2 {
			t.Fatalf("w2's container bound another lease: %+v", b.List())
		}
	}
}

func oneoff(id, project, wt string) func(*protocol.Snapshot) *protocol.Snapshot {
	return func(s *protocol.Snapshot) *protocol.Snapshot {
		return addContainer(s, protocol.Container{ID: id, Name: id, MemoryBytes: gib / 4,
			Labels: map[string]string{"com.docker.compose.project": project, "com.docker.compose.oneoff": "True"}}, wt)
	}
}

func service(id, project, wt string) func(*protocol.Snapshot) *protocol.Snapshot {
	return func(s *protocol.Snapshot) *protocol.Snapshot {
		return addContainer(s, protocol.Container{ID: id, Name: id, MemoryBytes: gib / 4,
			Labels: map[string]string{"com.docker.compose.project": project}}, wt)
	}
}

func run(wt, project string) policy.Request {
	return policy.Request{Worktree: wt, Kind: "compose", Op: "run", Command: "docker compose run", CostBytes: gib, Target: project}
}

// Two compose runs in a row: each lease binds its own one-off container.
func TestEachComposeRunBindsItsOwnContainer(t *testing.T) {
	b, c, log := book(t)
	b.Observe(snap())
	b.Check(run("w1", "p"), snap(), cfg)
	b.Observe(oneoff("r1", "p", "w1")(snap()))
	c.t = c.t.Add(time.Second)
	b.Check(run("w1", "p"), snap(), cfg)
	b.Observe(oneoff("r2", "p", "w1")(oneoff("r1", "p", "w1")(snap())))
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(oneoff("r2", "p", "w1")(oneoff("r1", "p", "w1")(snap())))
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("a run's lease never bound: %s", log)
	}
}

// compose run starts its depends_on services first: the run's lease takes
// them, so they are not ungated; an open up lease of the project still wins
// them.
func TestComposeRunBindsItsDependencies(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(run("w1", "p"), snap(), cfg)
	b.Observe(service("db", "p", "w1")(snap()))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
	b2, _, _ := book(t)
	b2.Observe(snap())
	b2.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: gib, Target: "p"}, snap(), cfg)
	b2.Check(run("w1", "p"), snap(), cfg)
	b2.Observe(service("web", "p", "w1")(snap()))
	for _, l := range b2.List() {
		if l.Command == "docker compose up" && l.Bytes != gib-gib/4 {
			t.Fatalf("the up's lease lost its service to the run's: %+v", b2.List())
		}
	}
}

// compose run --rm: the run's lease ends when its container is gone.
func TestAComposeRunLeaseEndsWithItsContainer(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(run("w1", "p"), snap(), cfg)
	b.Observe(oneoff("r1", "p", "w1")(snap()))
	b.Observe(snap()) // exited, removed
	if l := b.List(); len(l) != 0 {
		t.Fatalf("leases = %+v", l)
	}
}

// An unattributed container binds a worktree's lease before a manual one.
func TestAManualLeaseDoesNotWinAnUnattributedContainer(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	up := policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: gib, Target: "shop"}
	b.Check(up, snap(), cfg)
	c.t = c.t.Add(time.Second)
	up.Worktree = ""
	b.Check(up, snap(), cfg)
	b.Observe(service("s1", "shop", "")(snap()))
	if l := b.List(); len(l) != 1 || l[0].Bytes != gib-gib/4 {
		t.Fatalf("leases = %+v, want w1's bound", l)
	}
}

// compose run app, app depends on a one-shot migrate: migrate binds the
// run's lease and exits before app's one-off appears. The lease lives on,
// and app's container binds it.
func TestAOneShotDependencyDoesNotEndARunsLease(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(run("w1", "p"), snap(), cfg)
	b.Observe(service("migrate", "p", "w1")(snap()))
	b.Observe(snap()) // migrate done
	b.Observe(oneoff("app", "p", "w1")(snap()))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

// Once its one-off is there, a run's lease takes no more services: one
// started past the shim is ungated.
func TestARunsLeaseTakesNoServiceAfterItsOneOff(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(run("w1", "p"), snap(), cfg)
	s := oneoff("r1", "p", "w1")(snap())
	b.Observe(s)
	b.Observe(service("worker", "p", "w1")(oneoff("r1", "p", "w1")(snap())))
	if got := ungatedKeys(b); len(got) != 1 || got[0] != "container:worker" {
		t.Fatalf("ungated = %v", got)
	}
}

// A takeover that asks for more than the old lease still reserves gets a
// fresh timeout: compose --profile heavy up just before the old lease
// expires keeps its 6 GiB.
func TestATakeoverThatNeedsMoreGetsAFreshTimeout(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	s := service("web", "app", "w1")(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: gib, Target: "app"}, snap(), cfg)
	b.Observe(s)
	c.t = c.t.Add(115 * time.Second)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 6 * gib, Target: "app"}, s, cfg)
	c.t = c.t.Add(10 * time.Second)
	b.Observe(s)
	if r := reserved(b); r < 6*gib-gib/4 {
		t.Fatalf("reserved %d GiB after the old lease's timeout, want the new 6", r>>30)
	}
}

// Through the shim a compose call carries no estimate (the default cost):
// a takeover just before the old lease's timeout still holds its admitted
// reservation for a full timeout.
func TestATakeoverAtTheDefaultCostGetsAFreshTimeout(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	up := policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", Target: "app"}
	s := service("web", "app", "w1")(snap())
	b.Check(up, snap(), cfg)
	b.Observe(s)
	c.t = c.t.Add(115 * time.Second)
	b.Check(up, s, cfg) // compose --profile heavy up
	c.t = c.t.Add(10 * time.Second)
	b.Observe(s)
	if r := reserved(b); r == 0 {
		t.Fatal("the admitted call's reservation ended with the old lease")
	}
}

// compose run with a dependency that starts within the same tick as the
// one-off: Docker lists the newest (the one-off) first, yet the dependency
// is the run's too.
func TestADependencyInTheOneOffsTick(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(run("w1", "p"), snap(), cfg)
	b.Observe(service("db", "p", "w1")(oneoff("r1", "p", "w1")(snap())))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

// A service that crash-loops (out of a reading, then back) while a compose
// run waits for its one-off is not the run's dependency: it keeps its
// verdict, and the run's lease keeps its reservation.
func TestARestartingServiceDoesNotBindARunsLease(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(service("api", "p", "w1")(snap())) // running before
	c.t = c.t.Add(5 * time.Second)
	b.Check(run("w1", "p"), snap(), cfg)
	b.Observe(snap()) // api crashed
	c.t = c.t.Add(5 * time.Second)
	b.Observe(service("api", "p", "w1")(snap())) // its restart policy
	if r := reserved(b); r != gib {
		t.Fatalf("reserved %d, want the run's whole 1 GiB", r)
	}
}

// docker start a b c: every stopped target is the lease's, each at the
// default cost, so none of them is ungated and all are reserved.
func TestAStartOfSeveralContainersKeysEach(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	r := policy.Request{Worktree: "w1", Kind: "container", Command: "docker start a", Target: "a", ContainerID: "A",
		Others: []policy.Start{{ID: "B"}, {ID: "C"}}}
	d := b.Check(r, snap(), cfg)
	if d.LeaseID == "" || reserved(b) != 3*gib {
		t.Fatalf("decision %+v, reserved %d GiB, want 3", d, reserved(b)>>30)
	}
	s := snap()
	for _, id := range []string{"A", "B", "C"} {
		s = addContainer(s, protocol.Container{ID: id, Name: id, MemoryBytes: gib / 4}, "w1")
	}
	b.Observe(s)
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

// docker start a b with a running: only b is reserved; with both running
// or held, nothing starts and no lease is taken.
func TestAStartOfSeveralSkipsThoseThatRun(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	r := policy.Request{Worktree: "w1", Kind: "container", Command: "docker start a", Target: "a", ContainerID: "A", Running: true,
		Others: []policy.Start{{ID: "B"}}}
	b.Check(r, snap(), cfg)
	if reserved(b) != gib {
		t.Fatalf("reserved %d, want b's 1 GiB", reserved(b))
	}
	b2, _, _ := book(t)
	b2.Observe(snap())
	r.Others = []policy.Start{{ID: "B", Running: true}}
	if d := b2.Check(r, snap(), cfg); !d.Allow || d.LeaseID != "" {
		t.Fatalf("all running: %+v", d)
	}
}

// A target created through the shim: the start takes its create's lease
// over, as for one target.
func TestAStartOfSeveralTakesTheirCreatesOver(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	cr := labelled("w1", "pg")
	cr.Command, cr.CostBytes = "docker create pg", 4*gib
	created := b.Check(cr, snap(), cfg)
	r := policy.Request{Worktree: "w1", Kind: "container", Command: "docker start a", Target: "a", ContainerID: "A",
		Others: []policy.Start{{ID: "B", TakesOver: created.LeaseID}}}
	b.Check(r, snap(), cfg)
	if l := b.List(); len(l) != 1 || l[0].Bytes != 5*gib {
		t.Fatalf("leases = %+v, want one: a's 1 GiB and the create's 4", l)
	}
}

// docker start a b: a exits before b is seen; the lease waits for b.
func TestAStartOfSeveralWaitsForEach(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start a", Target: "a", ContainerID: "A",
		Others: []policy.Start{{ID: "B"}}}, snap(), cfg)
	b.Observe(addContainer(snap(), protocol.Container{ID: "A", Name: "a", MemoryBytes: gib / 4}, "w1"))
	b.Observe(snap())
	b.Observe(addContainer(snap(), protocol.Container{ID: "B", Name: "b", MemoryBytes: gib / 4}, "w1"))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

// compose up -d again once the first up's lease has ended, its services
// unknown: its running containers are the lease's, so it logs no "never
// appeared"; the estimate stays reserved for what the up may add.
func TestComposeUpOfARunningStackBindsItsContainers(t *testing.T) {
	b, c, log := book(t)
	up := policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", OnEngine: true}
	b.Observe(snap())
	b.Check(up, snap(), cfg)
	s := withComposeContainer(snap(), "web", "w1", gib/2)
	b.Observe(s)
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(s)
	if l := b.List(); len(l) != 0 {
		t.Fatalf("first lease still open: %+v", l)
	}
	b.Check(up, s, cfg)
	if r := reserved(b); r != 2*gib {
		t.Fatalf("reserved %d MiB, want the estimate: web counts already", r>>20)
	}
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(s)
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("log: %s", log)
	}
}

// Another worktree's containers of a same-named project are not its.
func TestComposeUpBindsNoOtherWorktreesStack(t *testing.T) {
	b, c, log := book(t)
	s := withComposeContainer(snap(), "web", "w2", gib/2)
	b.Observe(s)
	b.Observe(s)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 2 * gib, Target: "app"}, s, cfg)
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(s)
	if !strings.Contains(log.String(), "never appeared") {
		t.Fatal("w1's up bound w2's container")
	}
}

// docker start typo b c: the first is not found, the others still start.
// Each named target costs, minus only those known to run or be held.
func TestAStartChargesTargetsDockerCouldNotResolve(t *testing.T) {
	for name, r := range map[string]policy.Request{
		"first unresolved": {Worktree: "w1", Kind: "container", Command: "docker start x", Target: "x",
			Others: []policy.Start{{ID: "B"}, {ID: "C"}}},
		"one other unresolved": {Worktree: "w1", Kind: "container", Command: "docker start a", Target: "a", ContainerID: "A",
			Others: []policy.Start{{ID: "B"}}, Unresolved: 1},
		"running first, others unresolved": {Worktree: "w1", Kind: "container", Command: "docker start a", Target: "a", ContainerID: "A", Running: true,
			Unresolved: 3},
	} {
		b, _, _ := book(t)
		b.Observe(snap())
		if d := b.Check(r, snap(), cfg); d.LeaseID == "" || reserved(b) != 3*gib {
			t.Errorf("%s: decision %+v, reserved %d GiB, want 3", name, d, reserved(b)>>30)
		}
	}
}

// docker start db <db's ID>: one container, at one container's cost.
func TestAStartNamingOneContainerTwiceCostsOne(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db", Target: "db", ContainerID: "A",
		Others: []policy.Start{{ID: "A"}}}, snap(), cfg)
	if reserved(b) != gib {
		t.Fatalf("reserved %d GiB, want 1", reserved(b)>>30)
	}
}

// A huge estimate times its targets does not wrap to a small cost.
func TestAStartsCostDoesNotWrap(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	if d := b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start a", Target: "a", ContainerID: "A",
		CostBytes: (1<<64 + 2) / 3, Others: []policy.Start{{ID: "B"}, {ID: "C"}}}, snap(), cfg); d.Allow {
		t.Fatalf("allowed %+v", d)
	}
}

// docker start a b with a not resolved in time: a still binds by its name,
// and the lease waits for it too.
func TestAnUnresolvedFirstTargetKeepsItsName(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start a", Target: "a",
		Others: []policy.Start{{ID: "B"}}}, snap(), cfg)
	b.ContainerEvent("start", "B", "b", nil)
	b.ContainerEvent("die", "B", "b", nil)
	if len(b.List()) != 1 {
		t.Fatal("the lease ended before a came")
	}
	b.Observe(addContainer(snap(), protocol.Container{ID: "A", Name: "a"}, "w1"))
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

// A manual call's lease reserves nothing: it ties with no worktree's.
func TestAnEventPrefersAWorktreesLeaseToAManualOne(t *testing.T) {
	b, c, log := book(t)
	b.Observe(snap())
	up := policy.Request{Kind: "compose", Command: "docker compose up", CostBytes: 2 * gib, Target: "app"}
	b.Check(up, snap(), cfg)
	up.Worktree = "w1"
	b.Check(up, snap(), cfg)
	b.ContainerEvent("start", "job", "job", map[string]string{protocol.ComposeProjectLabel: "app"})
	b.ContainerEvent("die", "job", "job", map[string]string{protocol.ComposeProjectLabel: "app"})
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(snap())
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("w1's lease bound nothing: %s", log)
	}
}

// compose up -d of a stack whose services all run: it starts nothing, so
// it reserves nothing; one service down, and it holds its estimate.
func TestComposeUpReservesOnlyForServicesNotRunning(t *testing.T) {
	stack := func(services ...string) *protocol.Snapshot {
		s := snap()
		for _, sv := range services {
			s = addContainer(s, protocol.Container{ID: sv, Name: sv, MemoryBytes: gib / 4,
				Labels: map[string]string{protocol.ComposeProjectLabel: "app", "com.docker.compose.service": sv}}, "w1")
		}
		return s
	}
	for _, tc := range []struct {
		running []string
		want    uint64
	}{{[]string{"web", "db"}, 0}, {[]string{"web"}, 2 * gib}, {nil, 2 * gib}} {
		b, _, _ := book(t)
		s := stack(tc.running...)
		b.Observe(s)
		d := b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", OnEngine: true,
			Services: []string{"web", "db"}}, s, cfg)
		if !d.Allow || reserved(b) > tc.want || reserved(b)+1 < tc.want {
			t.Errorf("%v running: decision %+v, reserved %d MiB, want %d", tc.running, d, reserved(b)>>20, tc.want>>20)
		}
	}
}

// A service with three replicas of which one runs still starts two.
func TestComposeUpCountsReplicas(t *testing.T) {
	b, _, _ := book(t)
	s := addContainer(snap(), protocol.Container{ID: "w-1", Name: "w-1",
		Labels: map[string]string{protocol.ComposeProjectLabel: "app", "com.docker.compose.service": "web"}}, "w1")
	b.Observe(s)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 3 * gib, Target: "app", OnEngine: true,
		Services: []string{"web", "web", "web"}}, s, cfg)
	if reserved(b) != 3*gib {
		t.Fatalf("reserved %d MiB, want the estimate: two replicas start", reserved(b)>>20)
	}
}

// An up of a running stack that another lease holds (a compose run's
// dependencies) binds nothing, and that is no "never appeared".
func TestAnIdleUpEndsQuietly(t *testing.T) {
	b, c, log := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "run", Command: "docker compose run", Target: "app"}, snap(), cfg)
	s := addContainer(snap(), protocol.Container{ID: "db", Name: "db",
		Labels: map[string]string{protocol.ComposeProjectLabel: "app", "com.docker.compose.service": "db"}}, "w1")
	b.Observe(s) // db: the run's dependency
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", Target: "app", Services: []string{"db"}, OnEngine: true}, s, cfg)
	if reserved(b) > gib {
		t.Fatalf("reserved %d MiB: the up starts nothing", reserved(b)>>20)
	}
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(s)
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("log: %s", log)
	}
}

// docker start nope b, b running: it starts nothing, in either order.
func TestAStartOfAMissingAndARunningTakesNoLease(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	if d := b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start nope", FirstMissing: true,
		Others: []policy.Start{{ID: "B", Running: true}}}, snap(), cfg); !d.Allow || d.LeaseID != "" {
		t.Fatalf("decision %+v", d)
	}
}

// An up that starts nothing still meets the pressure guard: its reading may
// be seconds old (compose stop, then up).
func TestAnIdleUpMeetsThePressureGuard(t *testing.T) {
	b, _, _ := book(t)
	s := addContainer(snap(), protocol.Container{ID: "db", Name: "db",
		Labels: map[string]string{protocol.ComposeProjectLabel: "app", "com.docker.compose.service": "db"}}, "w1")
	s.Host.Pressure = "critical"
	b.Observe(s)
	if d := b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", Target: "app", Services: []string{"db"}, OnEngine: true}, s, cfg); d.Allow {
		t.Fatalf("decision %+v", d)
	}
}

// An up to another engine, or after Docker said its stack exited, or on an
// old reading, starts what it may: it keeps its estimate.
func TestAnUpIsIdleOnlyOnTheDaemonsEngineAndAFreshReading(t *testing.T) {
	stack := func() *protocol.Snapshot {
		return addContainer(snap(), protocol.Container{ID: "db", Name: "db",
			Labels: map[string]string{protocol.ComposeProjectLabel: "app", "com.docker.compose.service": "db"}}, "w1")
	}
	up := policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", Services: []string{"db"}}
	for name, prep := range map[string]func(*lease.Book, *clock) (policy.Request, *protocol.Snapshot){
		"another engine": func(*lease.Book, *clock) (policy.Request, *protocol.Snapshot) { return up, stack() },
		"stopped since": func(b *lease.Book, _ *clock) (policy.Request, *protocol.Snapshot) {
			b.ContainerEvent("die", "db", "db", map[string]string{protocol.ComposeProjectLabel: "app"})
			r := up
			r.OnEngine = true
			return r, stack()
		},
		"stale docker reading": func(b *lease.Book, c *clock) (policy.Request, *protocol.Snapshot) {
			s := stack()
			s.CollectedAt = c.t // stamped this tick, but Docker's reading is the last good one
			s.Sources = map[string]protocol.SourceStatus{"docker": {At: c.t.Add(-time.Minute), Began: c.t.Add(-time.Minute), Stale: true}}
			b.Observe(s)
			r := up
			r.OnEngine = true
			return r, s
		},
		"old reading": func(b *lease.Book, c *clock) (policy.Request, *protocol.Snapshot) {
			s := stack()
			s.CollectedAt = c.t
			b.Observe(s)
			c.t = c.t.Add(time.Minute)
			r := up
			r.OnEngine = true
			return r, s
		},
	} {
		b, c, _ := book(t)
		b.Observe(stack())
		r, s := prep(b, c)
		b.Check(r, s, cfg)
		if reserved(b) != 2*gib {
			t.Errorf("%s: reserved %d MiB, want the estimate", name, reserved(b)>>20)
		}
	}
}

// compose up -d again while the first up's lease is open: the new lease
// takes it over, so it is not counted twice against the cap.
func TestARepeatUpIsNotChargedTwice(t *testing.T) {
	b, _, _ := book(t)
	c := cfg
	c.PerWorktreeCapBytes = 2 * gib
	b.Observe(snap())
	up := policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", OnEngine: true}
	if d := b.Check(up, snap(), c); !d.Allow {
		t.Fatalf("first: %+v", d)
	}
	if d := b.Check(up, snap(), c); !d.Allow {
		t.Fatalf("again: %+v", d)
	}
}

// docker create x, then docker start x: the start takes the create's lease
// over, so the create's reservation is not counted against it too.
func TestAStartIsNotChargedForTheCreateItTakesOver(t *testing.T) {
	b, _, _ := book(t)
	c := cfg
	c.PerWorktreeCapBytes = 2 * gib
	b.Observe(snap())
	cr := labelled("w1", "x")
	cr.Command, cr.CostBytes = "docker create x", 2*gib
	created := b.Check(cr, snap(), c)
	if d := b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start x", CostBytes: 2 * gib, Target: "x", ContainerID: "X",
		TakesOver: created.LeaseID}, snap(), c); !d.Allow {
		t.Fatalf("start: %+v", d)
	}
}

// docker create -m 8g x, then docker start x: the start's lease holds the
// create's 8 GiB, so that is what its check weighs.
func TestAStartIsCheckedAtTheCostItTakesOver(t *testing.T) {
	b, _, _ := book(t)
	c := cfg
	c.PerWorktreeCapBytes = 9 * gib
	b.Observe(snap())
	cr := labelled("w1", "x")
	cr.Command, cr.CostBytes = "docker create x", 8*gib
	created := b.Check(cr, snap(), c)
	d := b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start x", Target: "x", ContainerID: "X",
		TakesOver: created.LeaseID}, snap(), c)
	if !d.Allow || d.CostBytes != 8*gib {
		t.Fatalf("decision %+v, want allowed at the create's 8 GiB", d)
	}
	if reserved(b) != 8*gib {
		t.Fatalf("reserved %d GiB, want the create's 8, once", reserved(b)>>30)
	}
}

// docker create -m 8g x, its lease lapsed (a slow pull), then docker start
// x: the start is weighed at the create's 8 GiB, released when it lapsed.
func TestAStartTakingALapsedCreateIsCheckedAtItsCost(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	cr := labelled("w1", "x")
	cr.Command, cr.CostBytes = "docker create x", 6*gib
	created := b.Check(cr, snap(), cfg)
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(snap()) // lapses
	if d := b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start x", Target: "x", ContainerID: "X",
		TakesOver: created.LeaseID}, snap(), cfg); d.CostBytes != 6*gib {
		t.Fatalf("decision %+v, want weighed at 6 GiB", d)
	}
}

// compose up -d again while the first up's lease still reserves, the stack
// running: it starts nothing, so it is not weighed at that reservation.
func TestAnIdleRepeatUpIsWeighedAtAByte(t *testing.T) {
	b, _, _ := book(t)
	c := cfg
	c.PerWorktreeCapBytes = 3 * gib
	b.Observe(snap())
	up := policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", OnEngine: true, Services: []string{"db"}}
	b.Check(up, snap(), c)
	s := addContainer(snap(), protocol.Container{ID: "db", Name: "db", MemoryBytes: gib / 2,
		Labels: map[string]string{protocol.ComposeProjectLabel: "app", "com.docker.compose.service": "db"}}, "w1")
	b.Observe(s)
	if d := b.Check(up, s, c); !d.Allow || d.CostBytes != 1 {
		t.Fatalf("decision %+v, want allowed at a byte", d)
	}
}
