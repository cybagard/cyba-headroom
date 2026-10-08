package lease_test

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/lease"
	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

const gib = uint64(1 << 30)

func i64(v int64) *int64 { return &v }

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

var cfg = policy.Config{PressureGuard: "critical", DefaultContainerBytes: gib, DefaultTartBytes: 4 * gib}

// snap has 8 GiB headroom, worktree w1 working, one existing container.
func snap() *protocol.Snapshot {
	return &protocol.Snapshot{
		Host:   &protocol.Host{TotalBytes: 64 * gib, Pressure: "normal"},
		Budget: &protocol.Budget{TotalBytes: 64 * gib, HeadroomBytes: i64(int64(8 * gib))},
		Orca: &protocol.Orca{Running: true, Worktrees: []protocol.Worktree{
			{ID: "w1", Name: "A", Agents: []protocol.Agent{{State: "working"}}},
			{ID: "w2", Name: "B", Agents: []protocol.Agent{{State: "working"}}},
		}},
		Docker:      &protocol.Docker{Running: true, Containers: []protocol.Container{{ID: "old", Name: "old"}}},
		Tart:        &protocol.Tart{Installed: true},
		Attribution: &protocol.Attribution{Worktrees: []protocol.WorktreeUsage{{ID: "w1"}, {ID: "w2"}}},
	}
}

func book(t *testing.T) (*lease.Book, *clock, *bytes.Buffer) {
	t.Helper()
	c := &clock{t0}
	var log bytes.Buffer
	return lease.New(2*time.Minute, c.now, slog.New(slog.NewTextHandler(&log, nil))), c, &log
}

func req(wt string, cost uint64) policy.Request {
	return policy.Request{Worktree: wt, Kind: "container", Command: "docker run x", CostBytes: cost}
}

func TestAnAllowLeasesItsCost(t *testing.T) {
	b, _, _ := book(t)
	d := b.Check(req("w1", 5*gib), snap(), cfg)
	if !d.Allow || d.LeaseID == "" || d.LeasedBytes != 0 {
		t.Fatalf("first = %+v", d)
	}
	// 8 − 5 leased = 3 left: 4 does not fit, and says why.
	d = b.Check(req("w2", 4*gib), snap(), cfg)
	if d.Allow || d.LeasedBytes != 5*gib || d.LeaseID != "" || !strings.Contains(d.Message, "promised") {
		t.Fatalf("second = %+v", d)
	}
	if ls := b.List(); len(ls) != 1 || ls[0].Bytes != 5*gib || ls[0].Worktree != "w1" {
		t.Fatalf("leases = %+v", ls)
	}
}

func TestManualCallsAreNotLeased(t *testing.T) {
	// A manual call is outside admission control: it must not hold back
	// memory from gated calls. Its container counts once it appears.
	b, _, _ := book(t)
	if d := b.Check(req("", 6*gib), snap(), cfg); !d.Allow || d.LeaseID != "" {
		t.Fatalf("manual = %+v", d)
	}
	if d := b.Check(req("w1", 4*gib), snap(), cfg); !d.Allow {
		t.Fatalf("a manual call held back memory: %+v", d)
	}
}

// R10: two agents each request 6 GB with 8 GB headroom at the same moment;
// exactly one is allowed.
func TestSimultaneousRequestsCannotOvercommit(t *testing.T) {
	for run := 0; run < 50; run++ {
		b, _, _ := book(t)
		s := snap()
		var wg sync.WaitGroup
		allowed := make(chan bool, 2)
		for _, wt := range []string{"w1", "w2"} {
			wg.Go(func() { allowed <- b.Check(req(wt, 6*gib), s, cfg).Allow })
		}
		wg.Wait()
		close(allowed)
		n := 0
		for a := range allowed {
			if a {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("run %d: %d allowed, want exactly 1", run, n)
		}
	}
}

// withContainer adds a running container, attributed to wt ("" = none),
// already using a lease's full gigabyte.
func withContainer(s *protocol.Snapshot, id, wt string) *protocol.Snapshot {
	return withContainerMem(s, id, wt, 64*gib)
}

func withContainerMem(s *protocol.Snapshot, id, wt string, mem uint64) *protocol.Snapshot {
	return addContainer(s, protocol.Container{ID: id, Name: id, MemoryBytes: mem}, wt)
}

// withComposeContainer adds a container of a compose project.
func withComposeContainer(s *protocol.Snapshot, id, wt string, mem uint64) *protocol.Snapshot {
	return addContainer(s, protocol.Container{ID: id, Name: id, MemoryBytes: mem,
		Labels: map[string]string{"com.docker.compose.project": "app"}}, wt)
}

func addContainer(s *protocol.Snapshot, c protocol.Container, wt string) *protocol.Snapshot {
	id := c.ID
	s.Docker.Containers = append(s.Docker.Containers, c)
	if wt != "" {
		for i := range s.Attribution.Worktrees {
			if s.Attribution.Worktrees[i].ID == wt {
				s.Attribution.Worktrees[i].Containers = append(s.Attribution.Worktrees[i].Containers, protocol.AttributedContainer{ID: id, Name: id})
			}
		}
	}
	return s
}

func worktrees(b *lease.Book) []string {
	var out []string
	for _, l := range b.List() {
		out = append(out, l.Worktree)
	}
	return out
}

func TestANewContainerInTheWorktreeSettlesItsLease(t *testing.T) {
	b, _, _ := book(t)
	b.Check(req("w1", gib), snap(), cfg)
	b.Check(req("w2", gib), snap(), cfg)
	b.Observe(snap()) // nothing new yet: only "old"
	if len(b.List()) != 2 {
		t.Fatalf("settled by an old container: %v", worktrees(b))
	}
	b.Observe(withContainer(snap(), "c2", "w2"))
	if got := worktrees(b); len(got) != 1 || got[0] != "w1" {
		t.Fatalf("open leases = %v, want w1's only", got)
	}
}

func TestTartLeasesWaitForAVM(t *testing.T) {
	b, _, _ := book(t)
	b.Check(policy.Request{Worktree: "w1", Kind: "tart", Command: "tart run vm", CostBytes: gib}, snap(), cfg)
	b.Observe(withContainer(snap(), "c2", "w1"))
	if len(b.List()) != 1 {
		t.Fatal("a container settled a Tart lease")
	}
	s := snap()
	s.Tart.VMs = []protocol.TartVM{{Name: "vm", MemoryBytes: gib}} // a VM counts its configured memory at once
	b.Observe(s)
	if len(b.List()) != 0 {
		t.Fatalf("a new VM did not settle the Tart lease: %+v", b.List())
	}
}

func TestAnUnattributedContainerSettlesTheOldestLease(t *testing.T) {
	b, c, _ := book(t)
	b.Check(req("w1", gib), snap(), cfg)
	c.t = c.t.Add(time.Second)
	b.Check(req("w2", gib), snap(), cfg)
	b.Observe(withContainer(snap(), "c2", ""))
	if got := worktrees(b); len(got) != 1 || got[0] != "w2" {
		t.Fatalf("open leases = %v, want w2's only", got)
	}
	// Another worktree's container does not settle w2's lease.
	b.Observe(withContainer(withContainer(snap(), "c2", ""), "c3", "w1"))
	if len(b.List()) != 1 {
		t.Fatal("w1's container settled w2's lease")
	}
}

func TestOneResourceSettlesOneLease(t *testing.T) {
	b, _, _ := book(t)
	b.Check(req("w1", gib), snap(), cfg)
	b.Check(req("w1", gib), snap(), cfg)
	s := withContainer(snap(), "c2", "w1")
	b.Observe(s)
	b.Observe(s) // the same container on the next tick
	if len(b.List()) != 1 {
		t.Fatalf("one container settled %d leases", 2-len(b.List()))
	}
}

func TestComposeSettlesOnItsFirstContainer(t *testing.T) {
	b, _, _ := book(t)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 3 * gib}, snap(), cfg)
	b.Observe(withComposeContainer(snap(), "db", "w1", 3*gib))
	if len(b.List()) != 0 {
		t.Fatal("compose lease still open after its containers used its cost")
	}
}

func TestLeasesExpireAndAreLogged(t *testing.T) {
	b, c, log := book(t)
	b.Check(req("w1", 5*gib), snap(), cfg)
	c.t = t0.Add(2*time.Minute - time.Second)
	b.Observe(snap())
	if len(b.List()) != 1 {
		t.Fatal("expired early")
	}
	c.t = t0.Add(2 * time.Minute)
	b.Observe(snap())
	if len(b.List()) != 0 {
		t.Fatal("not expired at the timeout")
	}
	for _, want := range []string{"lease expired", "worktree=w1", "docker run x", "age=2m0s"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	// The headroom is back.
	if d := b.Check(req("w2", 6*gib), snap(), cfg); !d.Allow {
		t.Fatalf("after expiry: %+v", d)
	}
}

func reserved(b *lease.Book) uint64 {
	var n uint64
	for _, l := range b.List() {
		n += l.Bytes
	}
	return n
}

func TestChecksUseTheSnapshotLeasesWereSettledOn(t *testing.T) {
	// A lease settled by c2 and the snapshot showing c2 must be seen
	// together: a check holding the older snapshot (no c2) would otherwise
	// count neither.
	b, _, _ := book(t)
	b.Check(req("w1", 6*gib), snap(), cfg)
	settled := withContainerMem(snap(), "c2", "w1", 6*gib)
	settled.Budget.HeadroomBytes = i64(int64(2 * gib)) // c2's 6 GB is in the budget now
	b.Observe(settled)
	if d := b.Check(req("w2", 6*gib), snap() /* stale: no c2, 8 GB */, cfg); d.Allow {
		t.Fatalf("decided on a stale snapshot: %+v", d)
	}
}

func TestASettlingContainerCannotSettleALaterLease(t *testing.T) {
	b, _, _ := book(t)
	b.Check(req("w1", gib), snap(), cfg)
	s2 := withContainer(snap(), "c2", "w1")
	b.Observe(s2)
	b.Check(req("w1", gib), snap() /* stale: no c2 */, cfg)
	b.Observe(s2)
	if len(b.List()) != 1 {
		t.Fatal("c2 settled a lease granted after it appeared")
	}
}

func TestALeaseKeepsWhatItsContainerHasNotUsedYet(t *testing.T) {
	b, _, _ := book(t)
	b.Check(req("w1", 6*gib), snap(), cfg)
	b.Observe(withContainerMem(snap(), "jvm", "w1", gib/2))
	if r := reserved(b); r != 6*gib-gib/2 {
		t.Fatalf("reserved %d, want 5.5 GiB: the container uses 0.5 so far", r)
	}
	b.Observe(withContainerMem(snap(), "jvm", "w1", 6*gib))
	if len(b.List()) != 0 {
		t.Fatalf("lease open after its container used it all: %+v", b.List())
	}
}

func TestComposeBindsAllItsNewContainers(t *testing.T) {
	b, _, _ := book(t)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 3 * gib}, snap(), cfg)
	s := withComposeContainer(snap(), "db", "w1", gib)
	b.Observe(s)
	if r := reserved(b); r != 2*gib {
		t.Fatalf("reserved %d after db, want 2 GiB", r)
	}
	b.Observe(withComposeContainer(withComposeContainer(snap(), "db", "w1", gib), "web", "w1", 2*gib))
	if len(b.List()) != 0 {
		t.Fatal("compose lease open after its containers used it")
	}
}

func TestABoundLeaseEndsQuietlyAtTheTimeout(t *testing.T) {
	b, c, log := book(t)
	b.Check(req("w1", 6*gib), snap(), cfg)
	b.Observe(withContainerMem(snap(), "small", "w1", gib)) // never grows to 6
	c.t = t0.Add(2 * time.Minute)
	b.Observe(withContainerMem(snap(), "small", "w1", gib))
	if len(b.List()) != 0 || strings.Contains(log.String(), "never appeared") {
		t.Fatalf("leases %+v, log %s", b.List(), log)
	}
}

func TestAContainerOnTheDeadlineSettlesItsOwnLease(t *testing.T) {
	b, c, log := book(t)
	b.Check(req("w1", gib), snap(), cfg)
	c.t = t0.Add(2 * time.Minute)
	b.Observe(withContainer(snap(), "c2", "w1"))
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("logged as never appeared: %s", log)
	}
}

func TestComposeAndPlainLeasesTakeTheirOwnContainers(t *testing.T) {
	b, c, _ := book(t)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 3 * gib}, snap(), cfg)
	c.t = c.t.Add(time.Second)
	b.Check(req("w1", 2*gib), snap(), cfg)
	b.Observe(withContainerMem(snap(), "x", "w1", 2*gib)) // the plain docker run's container
	ls := b.List()
	if len(ls) != 1 || ls[0].Kind != "compose" {
		t.Fatalf("open leases = %+v, want only the compose lease", ls)
	}
}

func TestAnExpiredLeaseDoesNotTakeALiveLeasesContainer(t *testing.T) {
	b, c, log := book(t)
	b.Check(req("w1", gib), snap(), cfg) // its call failed: no container
	c.t = t0.Add(time.Minute)
	b.Check(req("w1", gib), snap(), cfg)
	c.t = t0.Add(2 * time.Minute) // the first expires as the second's container appears
	b.Observe(withContainer(snap(), "c2", "w1"))
	if len(b.List()) != 0 {
		t.Fatalf("open leases = %+v", b.List())
	}
	if !strings.Contains(log.String(), "never appeared") || !strings.Contains(log.String(), "lease-1") {
		t.Fatalf("the expired lease was not logged: %s", log)
	}
}

func TestComposeOutlivesAOneShotFirstContainer(t *testing.T) {
	b, _, _ := book(t)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 3 * gib}, snap(), cfg)
	b.Observe(withComposeContainer(snap(), "migrate", "w1", gib/10))
	b.Observe(snap()) // migrate exited; db and app not up yet
	if r := reserved(b); r != 3*gib {
		t.Fatalf("reserved %d, want the full 3 GiB until the services come up", r)
	}
}

func TestAContainerMissingFromOneSnapshotKeepsItsLease(t *testing.T) {
	b, c, _ := book(t)
	b.Check(req("w1", 6*gib), snap(), cfg)
	b.Observe(withContainerMem(snap(), "c2", "w1", gib)) // binds, 5 GB still reserved
	c.t = c.t.Add(time.Second)
	b.Check(req("w1", gib), snap(), cfg) // another call from w1, still starting
	missing := snap()
	missing.Sources = map[string]protocol.SourceStatus{"docker": {Stale: true}}
	b.Observe(missing) // the docker read failed: c2 is not in it
	if r := reserved(b); r != 5*gib+gib {
		t.Fatalf("reserved %d after a failed read, want 6 GiB", r)
	}
	b.Observe(withContainerMem(snap(), "c2", "w1", gib)) // c2 is back
	ls := b.List()
	if len(ls) != 2 {
		t.Fatalf("c2 came back and took the other lease: %+v", ls)
	}
}

func TestComposeLeasesKeepToTheirProject(t *testing.T) {
	b, c, _ := book(t)
	compose := policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 3 * gib}
	b.Check(compose, snap(), cfg)
	b.Observe(withComposeProject(snap(), "a-db", "w1", "a", gib))
	c.t = c.t.Add(time.Second)
	b.Check(compose, snap(), cfg) // a second project
	s := withComposeProject(withComposeProject(snap(), "a-db", "w1", "a", gib), "b-db", "w1", "b", gib)
	b.Observe(s)
	for _, l := range b.List() {
		if l.Bytes != 2*gib {
			t.Fatalf("lease %s reserves %d, want 2 GiB: each project bound its own container", l.ID, l.Bytes)
		}
	}
}

func withComposeProject(s *protocol.Snapshot, id, wt, project string, mem uint64) *protocol.Snapshot {
	return addContainer(s, protocol.Container{ID: id, Name: id, MemoryBytes: mem,
		Labels: map[string]string{"com.docker.compose.project": project}}, wt)
}

func TestTheCapCountsTheWorktreesOwnLeases(t *testing.T) {
	b, _, _ := book(t)
	c := cfg
	c.PerWorktreeCapBytes = 5 * gib
	if d := b.Check(req("w1", 3*gib), snap(), c); !d.Allow {
		t.Fatalf("first = %+v", d)
	}
	if d := b.Check(req("w1", 3*gib), snap(), c); d.Allow || d.Reasons[0].Code != policy.WorktreeCap {
		t.Fatalf("second = %+v: its first 3 GB are leased", d)
	}
}

func TestChecksUseTheDaemonsSnapshotWhenDeriveStopped(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap()) // normal pressure
	c.t = c.t.Add(time.Minute)
	fresh := snap()
	fresh.CollectedAt = c.t
	fresh.Host.Pressure = "critical" // derive failed since: Observe never saw this
	if d := b.Check(req("w1", gib), fresh, cfg); d.Allow {
		t.Fatalf("decided on a frozen snapshot: %+v", d)
	}
}

func TestNoSnapshotYet(t *testing.T) {
	b, _, _ := book(t)
	if d := b.Check(req("w1", gib), nil, cfg); !d.Allow {
		t.Fatalf("before the first tick: %+v", d)
	}
}

func TestLeasesKeepOnlyWhatTheCallWas(t *testing.T) {
	for cmd, want := range map[string]string{
		"docker run postgres:17":                  "docker run postgres:17",
		"docker run -e TOKEN=x -v /a:/b img":      "docker run",
		"docker compose -f a.yml up -d":           "docker compose",
		"docker compose up -d":                    "docker compose up",
		"tart run ci-vm --dir src:/Users/dev/w/a": "tart run ci-vm",
		"docker run img sh -c 'echo KEY=x'":       "docker run img",
		"docker run user@host":                    "docker run",
	} {
		if got := lease.Summary(cmd); got != want {
			t.Errorf("Summary(%q) = %q, want %q", cmd, got, want)
		}
	}
	b, _, _ := book(t)
	r := req("w1", gib)
	r.Command = "docker run -e TOKEN=x img"
	b.Check(r, snap(), cfg)
	if got := b.List()[0].Command; got != "docker run" {
		t.Fatalf("lease command = %q", got)
	}
}

func TestARestartedVMBindsItsNewLease(t *testing.T) {
	b, c, log := book(t)
	running := snap()
	running.Tart.VMs = []protocol.TartVM{{Name: "ci-vm", MemoryBytes: 4 * gib}}
	b.Observe(running) // ci-vm runs
	b.Observe(snap())  // stopped
	c.t = c.t.Add(5 * time.Minute)
	b.Check(policy.Request{Worktree: "w1", Kind: "tart", Command: "tart run ci-vm", CostBytes: 4 * gib}, snap(), cfg)
	b.Observe(running) // started again by that call
	if len(b.List()) != 0 {
		t.Fatalf("the restarted VM did not settle its lease: %+v (%s)", b.List(), log)
	}
}

func TestACheckBeforeTheFirstTickBindsItsContainer(t *testing.T) {
	b, _, _ := book(t)
	b.Check(req("w1", gib), &protocol.Snapshot{}, cfg) // the daemon has no reading yet
	s := withContainer(snap(), "c2", "w1")             // "old" existed before; c2 is this call's
	b.Observe(s)
	if len(b.List()) != 0 {
		t.Fatalf("its container did not settle the lease: %+v", b.List())
	}
}

func TestWithoutABaselineOnlyTheWorktreesOwnContainerBinds(t *testing.T) {
	b, _, _ := book(t)
	b.Check(req("w1", gib), &protocol.Snapshot{}, cfg)
	b.Observe(snap()) // only "old", unattributed: not this call's
	if len(b.List()) != 1 {
		t.Fatal("an unattributed container that may have been there before bound the lease")
	}
}

func TestExpiredLeasesStopCountingWithoutObserve(t *testing.T) {
	b, c, log := book(t)
	b.Check(req("w1", 6*gib), snap(), cfg)
	c.t = c.t.Add(10 * time.Minute) // derive stalled: no Observe since
	if d := b.Check(req("w2", 6*gib), snap(), cfg); !d.Allow {
		t.Fatalf("an expired lease still counted: %+v", d)
	}
	if !strings.Contains(log.String(), "never appeared") {
		t.Fatalf("expiry not logged: %s", log)
	}
}
