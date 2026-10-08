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
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, ComposeDir: "/Users/dev/src/a"}, snap(), cfg)
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
	up := policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: gib, ComposeDir: "/Users/dev/src/a"}
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
