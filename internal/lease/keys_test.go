package lease_test

import (
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// compose stop, then compose up (no -p) in the same
// worktree. The returning containers must bind the up's lease.
func TestComposeUpAfterStopBindsByItsProjectDirectory(t *testing.T) {
	b, c, _ := book(t)
	app := func(s *protocol.Snapshot) *protocol.Snapshot {
		return addContainer(s, protocol.Container{ID: "A1", Name: "app-db-1", MemoryBytes: gib / 2,
			Labels: map[string]string{"com.docker.compose.project": "app", protocol.ComposeWorkingDirLabel: "/Users/dev/src/a"}}, "w1")
	}
	b.Observe(app(snap()))
	c.t = c.t.Add(5 * time.Second)
	b.Observe(snap()) // docker compose stop
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, ComposeDirs: []string{"/Users/dev/src/a"}}, snap(), cfg)
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
	up := policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, ComposeDirs: []string{"/Users/dev/src/a"}}
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
		"-p a then -p b": {Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, Target: "b", ComposeDirs: []string{"/repo"}},
		"-p a then none": {Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, ComposeDirs: []string{"/repo"}},
	} {
		t.Run(name, func(t *testing.T) {
			b, _, _ := book(t)
			b.Observe(snap())
			b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, Target: "a", ComposeDirs: []string{"/repo"}}, snap(), cfg)
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

// A plain compose up and a -p other up from one directory: both projects'
// containers carry that directory. The named project's lease takes its
// own, and the directory's lease the default project's.
func TestANamedProjectBeatsADirectoryKey(t *testing.T) {
	b, c, log := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, ComposeDirs: []string{"/repo"}}, snap(), cfg)
	c.t = c.t.Add(time.Second)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, Target: "other"}, snap(), cfg)
	ctr := func(s *protocol.Snapshot, id, project string) *protocol.Snapshot {
		return addContainer(s, protocol.Container{ID: id, Name: project + "-web-1", MemoryBytes: gib / 4,
			Labels: map[string]string{"com.docker.compose.project": project, protocol.ComposeWorkingDirLabel: "/repo"}}, "w1")
	}
	b.Observe(ctr(ctr(snap(), "o1", "other"), "r1", "repo")) // other's first
	if l := b.List(); len(l) != 2 || l[0].Bytes != gib-gib/4 || l[1].Bytes != gib-gib/4 {
		t.Fatalf("leases = %+v, want each bound to its own project", l)
	}
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(ctr(ctr(snap(), "o1", "other"), "r1", "repo"))
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("logged: %s", log)
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

// compose -p app up, then a plain compose up of the same stack (project
// app by default, from /repo/app): one lease, not two.
func TestAProjectAndADirectoryOfOneStackShareAKey(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 2 * gib, Target: "app"}, snap(), cfg)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 2 * gib, ComposeDirs: []string{"/repo/app"}}, snap(), cfg)
	if l := b.List(); len(l) != 1 {
		t.Fatalf("leases = %+v, want the second to take the first over", l)
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
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 4 * gib, Target: "px"}, snap(), cfg)
	if r := reserved(b); r != 3*gib+3*gib {
		t.Fatalf("reserved %d GiB, want px's 3 and py's 3", r>>30)
	}
}
