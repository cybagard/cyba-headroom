package lease_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"slices"
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
		CollectedAt: t0,
	}
}

func book(t *testing.T) (*lease.Book, *clock, *bytes.Buffer) {
	t.Helper()
	c := &clock{t0}
	var log bytes.Buffer
	return lease.New(2*time.Minute, c.now, slog.New(slog.NewTextHandler(&log, nil))), c, &log
}

// req is a docker run through the shim: its container carries its lease.
func req(wt string, cost uint64) policy.Request {
	return policy.Request{Worktree: wt, Kind: "container", Command: "docker run x", CostBytes: cost, Labelled: true}
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

func TestManualCallsReserveNothing(t *testing.T) {
	// A manual call is outside admission control: it must not hold back
	// memory from gated calls. Its container counts once it appears. Its
	// lease only marks the call as checked (#33).
	b, _, _ := book(t)
	if d := b.Check(req("", 6*gib), snap(), cfg); !d.Allow || d.LeasedBytes != 0 {
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

// withRun adds the container of the run whose lease is lease: the shim
// labelled it with the lease's ID.
func withRun(s *protocol.Snapshot, id, wt string, mem uint64, lease string) *protocol.Snapshot {
	return addContainer(s, protocol.Container{ID: id, Name: id, MemoryBytes: mem,
		Labels: map[string]string{protocol.LeaseLabel: lease}}, wt)
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
	d2 := b.Check(req("w2", gib), snap(), cfg)
	b.Observe(snap()) // nothing new yet: only "old"
	if len(b.List()) != 2 {
		t.Fatalf("settled by an old container: %v", worktrees(b))
	}
	b.Observe(withRun(snap(), "c2", "w2", 64*gib, d2.LeaseID))
	if got := worktrees(b); len(got) != 1 || got[0] != "w1" {
		t.Fatalf("open leases = %v, want w1's only", got)
	}
}

func TestTartLeasesWaitForAVM(t *testing.T) {
	b, _, _ := book(t)
	b.Check(policy.Request{Worktree: "w1", Kind: "tart", Command: "tart run vm", CostBytes: gib, Target: "vm"}, snap(), cfg)
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

func TestOneResourceSettlesOneLease(t *testing.T) {
	b, _, _ := book(t)
	d := b.Check(req("w1", gib), snap(), cfg)
	b.Check(req("w1", gib), snap(), cfg)
	s := withRun(snap(), "c2", "w1", 64*gib, d.LeaseID)
	s = withRun(s, "c3", "w1", 64*gib, d.LeaseID) // a copy with the same label
	b.Observe(s)
	b.Observe(s) // the same containers on the next tick
	if len(b.List()) != 1 {
		t.Fatalf("one container settled %d leases", 2-len(b.List()))
	}
}

func TestComposeSettlesOnItsFirstContainer(t *testing.T) {
	b, _, _ := book(t)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 3 * gib, Target: "app"}, snap(), cfg)
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
	d := b.Check(req("w1", gib), snap(), cfg)
	s2 := withRun(snap(), "c2", "w1", 64*gib, d.LeaseID)
	b.Observe(s2)
	b.Check(req("w1", gib), snap() /* stale: no c2 */, cfg)
	b.Observe(s2)
	if len(b.List()) != 1 {
		t.Fatal("c2 settled a lease granted after it appeared")
	}
}

func TestALeaseKeepsWhatItsContainerHasNotUsedYet(t *testing.T) {
	b, _, _ := book(t)
	d := b.Check(req("w1", 6*gib), snap(), cfg)
	b.Observe(withRun(snap(), "jvm", "w1", gib/2, d.LeaseID))
	if r := reserved(b); r != 6*gib-gib/2 {
		t.Fatalf("reserved %d, want 5.5 GiB: the container uses 0.5 so far", r)
	}
	b.Observe(withRun(snap(), "jvm", "w1", 6*gib, d.LeaseID))
	if len(b.List()) != 0 {
		t.Fatalf("lease open after its container used it all: %+v", b.List())
	}
}

func TestComposeBindsAllItsNewContainers(t *testing.T) {
	b, _, _ := book(t)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 3 * gib, Target: "app"}, snap(), cfg)
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
	d := b.Check(req("w1", 6*gib), snap(), cfg)
	b.Observe(withRun(snap(), "small", "w1", gib, d.LeaseID)) // never grows to 6
	c.t = t0.Add(2 * time.Minute)
	b.Observe(withRun(snap(), "small", "w1", gib, d.LeaseID))
	if len(b.List()) != 0 || strings.Contains(log.String(), "never appeared") {
		t.Fatalf("leases %+v, log %s", b.List(), log)
	}
}

func TestAContainerOnTheDeadlineSettlesItsOwnLease(t *testing.T) {
	b, c, log := book(t)
	d := b.Check(req("w1", gib), snap(), cfg)
	c.t = t0.Add(2 * time.Minute)
	b.Observe(withRun(snap(), "c2", "w1", 64*gib, d.LeaseID))
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("logged as never appeared: %s", log)
	}
}

func TestComposeAndPlainLeasesTakeTheirOwnContainers(t *testing.T) {
	b, c, _ := book(t)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 3 * gib, Target: "app"}, snap(), cfg)
	c.t = c.t.Add(time.Second)
	d := b.Check(req("w1", 2*gib), snap(), cfg)
	b.Observe(withRun(snap(), "x", "w1", 2*gib, d.LeaseID)) // the plain docker run's container
	ls := b.List()
	if len(ls) != 1 || ls[0].Kind != "compose" {
		t.Fatalf("open leases = %+v, want only the compose lease", ls)
	}
}

func TestAnExpiredLeaseDoesNotTakeALiveLeasesContainer(t *testing.T) {
	b, c, log := book(t)
	b.Check(req("w1", gib), snap(), cfg) // its call failed: no container
	c.t = t0.Add(time.Minute)
	d := b.Check(req("w1", gib), snap(), cfg)
	c.t = t0.Add(2 * time.Minute) // the first expires as the second's container appears
	b.Observe(withRun(snap(), "c2", "w1", 64*gib, d.LeaseID))
	if len(b.List()) != 0 {
		t.Fatalf("open leases = %+v", b.List())
	}
	if !strings.Contains(log.String(), "never appeared") || !strings.Contains(log.String(), "-1 worktree=w1") { // the first lease
		t.Fatalf("the expired lease was not logged: %s", log)
	}
}

func TestComposeOutlivesAOneShotFirstContainer(t *testing.T) {
	b, _, _ := book(t)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 3 * gib, Target: "app"}, snap(), cfg)
	b.Observe(withComposeContainer(snap(), "migrate", "w1", gib/10))
	// migrate exited, its die says (#87); db and app not up yet.
	b.ContainerEvent("die", "migrate", "migrate", map[string]string{"com.docker.compose.project": "app"})
	b.Observe(snap())
	if r := reserved(b); r != 3*gib {
		t.Fatalf("reserved %d, want the full 3 GiB until the services come up", r)
	}
}

func TestAContainerMissingFromOneSnapshotKeepsItsLease(t *testing.T) {
	b, c, _ := book(t)
	d := b.Check(req("w1", 6*gib), snap(), cfg)
	b.Observe(withRun(snap(), "c2", "w1", gib, d.LeaseID)) // binds, 5 GB still reserved
	c.t = c.t.Add(time.Second)
	b.Check(req("w1", gib), snap(), cfg) // another call from w1, still starting
	missing := snap()
	missing.Sources = map[string]protocol.SourceStatus{"docker": {Stale: true}}
	b.Observe(missing) // the docker read failed: c2 is not in it
	if r := reserved(b); r != 5*gib+gib {
		t.Fatalf("reserved %d after a failed read, want 6 GiB", r)
	}
	b.Observe(withRun(snap(), "c2", "w1", gib, d.LeaseID)) // c2 is back
	ls := b.List()
	if len(ls) != 2 {
		t.Fatalf("c2 came back and took the other lease: %+v", ls)
	}
}

// heldDB is a book whose up holds db, running with 3 GiB, and reserves
// 1 GiB for the services it starts.
func heldDB(t *testing.T) (*lease.Book, *clock, *protocol.Snapshot, policy.Request) {
	t.Helper()
	b, c, _ := book(t)
	s := withComposeContainer(snap(), "db", "w1", 3*gib)
	b.Observe(read(s, c.t))
	up := policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: gib, Target: "app", OnEngine: true}
	b.Check(up, s, cfg)
	return b, c, s, up
}

// observe gives b a fresh reading of s, 5 s after the last.
func observe(b *lease.Book, c *clock, s *protocol.Snapshot) {
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(s, c.t))
}

// A held container missing from one reading (its stats failed, or a
// restart backoff spans it) is missing, not gone (#87): it stays held, so
// when it is back its memory is not counted as what the up waits for.
func TestAHeldContainerMissingFromOneReadingStaysHeld(t *testing.T) {
	b, c, s, _ := heldDB(t)
	observe(b, c, snap()) // db missing
	observe(b, c, s)      // db back
	if r := reserved(b); r != gib {
		t.Fatalf("reserved %d MiB once db is back, want 1024", r>>20)
	}
}

// A held container missing from two readings in a row is gone: let go of,
// it counts as new when it is back.
func TestAHeldContainerMissingFromTwoReadingsIsReleased(t *testing.T) {
	b, c, s, _ := heldDB(t)
	observe(b, c, snap())
	observe(b, c, snap())
	observe(b, c, s)
	if r := reserved(b); r != 0 {
		t.Fatalf("reserved %d MiB once db is back, want 0", r>>20)
	}
}

// A held container Docker says died is gone at once, with no reading to
// wait for.
func TestAHeldContainerThatDiedIsReleasedAtOnce(t *testing.T) {
	b, c, s, _ := heldDB(t)
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("die", "db", "db", map[string]string{protocol.ComposeProjectLabel: "app"})
	observe(b, c, snap())
	observe(b, c, s)
	if r := reserved(b); r != 0 {
		t.Fatalf("reserved %d MiB once db is back, want 0", r>>20)
	}
}

// A takeover keeps a held container that is missing from the check's
// reading: the book, not that reading, says when it is gone.
func TestATakeoverKeepsAHeldContainerMissingFromOneReading(t *testing.T) {
	b, c, s, up := heldDB(t)
	observe(b, c, snap())
	c.t = c.t.Add(time.Second)
	b.Check(up, snap(), cfg)
	observe(b, c, s)
	if r := reserved(b); r != gib {
		t.Fatalf("reserved %d MiB once db is back, want 1024", r>>20)
	}
}

// A run's only container missing from one reading does not end its lease
// while it warms (#110); missing from a second, it is gone, and so is the
// lease.
func TestALeaseOutlivesOneReadingWithoutItsContainer(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(read(snap(), c.t))
	d := b.Check(req("w1", 2*gib), snap(), cfg)
	observe(b, c, withRun(snap(), "c1", "w1", gib, d.LeaseID))
	observe(b, c, snap())
	if len(b.List()) != 1 {
		t.Fatal("one reading without c1 ended its lease")
	}
	observe(b, c, snap())
	if ls := b.List(); len(ls) != 0 {
		t.Fatalf("two readings without c1 left its lease open: %+v", ls)
	}
}

// A container missing from one reading keeps its last known use, as a
// failed read does: what it uses is still in the host's reading.
func TestAContainerMissingFromOneReadingKeepsItsUse(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(read(snap(), c.t))
	d := b.Check(req("w1", 2*gib), snap(), cfg)
	observe(b, c, withRun(snap(), "c1", "w1", 3*gib/2, d.LeaseID))
	observe(b, c, snap())
	if r := reserved(b); r != gib/2 {
		t.Fatalf("reserved %d MiB while c1 is missing, want 512", r>>20)
	}
}

func TestComposeLeasesKeepToTheirProject(t *testing.T) {
	b, c, _ := book(t)
	compose := policy.Request{Worktree: "w1", Kind: "compose", Command: "docker compose up", CostBytes: 3 * gib, Target: "a"}
	b.Check(compose, snap(), cfg)
	b.Observe(withComposeProject(snap(), "a-db", "w1", "a", gib))
	c.t = c.t.Add(time.Second)
	compose.Target = "b"
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
	b.Check(policy.Request{Worktree: "w1", Kind: "tart", Command: "tart run ci-vm", CostBytes: 4 * gib, Target: "ci-vm"}, snap(), cfg)
	b.Observe(running) // started again by that call
	if len(b.List()) != 0 {
		t.Fatalf("the restarted VM did not settle its lease: %+v (%s)", b.List(), log)
	}
}

func TestACheckBeforeTheFirstTickBindsItsContainer(t *testing.T) {
	b, _, _ := book(t)
	d := b.Check(req("w1", gib), &protocol.Snapshot{}, cfg) // the daemon has no reading yet
	s := withRun(snap(), "c2", "w1", 64*gib, d.LeaseID)     // "old" existed before; c2 is this call's
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

// A call whose exec failed hands its lease back (#28).
func TestReleaseEndsALease(t *testing.T) {
	b, _, _ := book(t)
	d := b.Check(req("w1", 5*gib), snap(), cfg)
	if !b.Release(d.LeaseID) || len(b.List()) != 0 {
		t.Fatalf("release %s: leases %+v", d.LeaseID, b.List())
	}
	if b.Release(d.LeaseID) || b.Release("lease-99") {
		t.Fatal("released a lease that is not open")
	}
	if d = b.Check(req("w2", 8*gib), snap(), cfg); !d.Allow {
		t.Fatalf("the released cost still counts: %+v", d)
	}
}

// Lease IDs differ between daemon runs, so a release meant for a lease of
// an earlier run cannot end one of this run.
func TestLeaseIDsDifferBetweenBooks(t *testing.T) {
	a, _, _ := book(t)
	b, _, _ := book(t)
	da, db := a.Check(req("w1", gib), snap(), cfg), b.Check(req("w1", gib), snap(), cfg)
	if da.LeaseID == db.LeaseID || b.Release(da.LeaseID) {
		t.Fatalf("ids %q and %q", da.LeaseID, db.LeaseID)
	}
}

// A macOS VM allowed but not yet running holds a slot (R6, #29): of two
// simultaneous requests for the last slot, one gets it.
func TestTheLastMacOSSlotGoesToOneRequest(t *testing.T) {
	b, _, _ := book(t)
	s := snap()
	s.Tart = &protocol.Tart{Installed: true, MacOSRunning: 1, VMs: []protocol.TartVM{{Name: "held", OS: "darwin"}}}
	c := cfg
	c.MaxMacOSVMs = 2
	mac := policy.Request{Worktree: "w1", Kind: "tart", Command: "tart run m", CostBytes: gib, MacOS: true, Target: "m"}
	var wg sync.WaitGroup
	allowed := make(chan bool, 8)
	for range 8 {
		wg.Go(func() { allowed <- b.Check(mac, s, c).Allow })
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
		t.Fatalf("%d of 8 got the last slot", n)
	}
	// Once its VM runs, the lease no longer counts as starting: the
	// snapshot counts it as running instead.
	s.Tart.VMs = append(s.Tart.VMs, protocol.TartVM{Name: "m", OS: "darwin"})
	s.Tart.MacOSRunning = 2
	b.Observe(s)
	c.MaxMacOSVMs = 3
	if d := b.Check(mac, s, c); !d.Allow {
		t.Fatalf("the bound lease still counts as starting: %+v", d)
	}
}

// A Linux VM appearing does not settle a macOS lease, whose slot would be
// freed for a VM that never took one.
func TestAMacOSLeaseWaitsForAMacOSVM(t *testing.T) {
	b, _, _ := book(t)
	s := snap()
	s.Tart = &protocol.Tart{Installed: true}
	c := cfg
	c.MaxMacOSVMs = 1
	mac := policy.Request{Worktree: "w1", Kind: "tart", Command: "tart run m", CostBytes: gib, MacOS: true, Target: "m"}
	if d := b.Check(mac, s, c); !d.Allow {
		t.Fatalf("first: %+v", d)
	}
	s.Tart.VMs = []protocol.TartVM{{Name: "lx", OS: "linux"}}
	b.Observe(s)
	if d := b.Check(mac, s, c); d.Allow {
		t.Fatalf("the linux VM freed the macOS slot: %+v", d)
	}
}

// A tart run whose process is gone without its VM showing up (it failed
// after the exec) frees its slot at once, not at the lease timeout.
func TestATartLeaseEndsWithItsProcess(t *testing.T) {
	b, _, log := book(t)
	alive := map[int]bool{4242: true}
	lease.SetAlive(b, func(pid int) bool { return alive[pid] })
	s := snap()
	s.Tart = &protocol.Tart{Installed: true}
	run := policy.Request{Worktree: "w1", Kind: "tart", Command: "tart run lx", CostBytes: gib, PID: 4242}
	if d := b.Check(run, s, cfg); !d.Allow {
		t.Fatalf("check: %+v", d)
	}
	b.Observe(s)
	if len(b.List()) != 1 {
		t.Fatal("the lease ended while its process runs")
	}
	alive[4242] = false
	b.Observe(s)
	if len(b.List()) != 0 || !strings.Contains(log.String(), "exited before") {
		t.Fatalf("leases %+v, log %s", b.List(), log)
	}
}

// A tart lease binds the VM its own process runs, whatever starts first.
func TestATartLeaseBindsTheVMItsProcessRuns(t *testing.T) {
	b, _, _ := book(t)
	lease.SetAlive(b, func(int) bool { return true })
	s := snap()
	s.Tart = &protocol.Tart{Installed: true}
	c := cfg
	c.MaxMacOSVMs = 2
	b.Check(policy.Request{Worktree: "w1", Kind: "tart", Command: "tart run lx", CostBytes: gib, PID: 10}, s, c)
	b.Check(policy.Request{Worktree: "w1", Kind: "tart", Command: "tart run mac", CostBytes: gib, MacOS: true, PID: 20}, s, c)
	// mac comes up first; the older linux lease must not take it.
	s.Tart.VMs = []protocol.TartVM{{Name: "mac", OS: "darwin", RunPID: 20}}
	s.Tart.MacOSRunning = 1
	b.Observe(s)
	// One macOS VM running and none starting: a second fits.
	if d := b.Check(policy.Request{Worktree: "w2", Kind: "tart", Command: "tart run m2", CostBytes: gib, MacOS: true}, s, c); !d.Allow {
		t.Fatalf("the slot is counted twice: %+v", d)
	}
}

// A macOS lease is settled by a VM of unknown OS (counted as macOS), and a
// lease binds the VM its process runs whatever its OS (#29).
func TestTartLeasesBindUnknownAndOwnVMs(t *testing.T) {
	b, _, _ := book(t)
	lease.SetAlive(b, func(int) bool { return true })
	s := snap()
	s.Tart = &protocol.Tart{Installed: true}
	c := cfg
	c.MaxMacOSVMs = 2
	b.Check(policy.Request{Worktree: "w1", Kind: "tart", Command: "tart run m", CostBytes: gib, MacOS: true, Target: "m"}, s, c)
	b.Check(policy.Request{Worktree: "w1", Kind: "tart", Command: "tart run x", CostBytes: gib, MacOS: true, VMUnknown: true, PID: 30, Target: "x"}, s, c)
	s.Tart.VMs = []protocol.TartVM{{Name: "m"}, {Name: "x", OS: "linux", RunPID: 30}}
	s.Tart.MacOSRunning = 1
	b.Observe(s)
	// Both leases are bound: nothing is starting, one macOS VM runs.
	if d := b.Check(policy.Request{Worktree: "w2", Kind: "tart", Command: "tart run n", CostBytes: gib, MacOS: true}, s, c); !d.Allow {
		t.Fatalf("a bound lease still counts as starting: %+v", d)
	}
}

// A tart behind a wrapper that forks has another PID than the lease; its
// VM still settles the lease.
func TestATartLeaseBindsAVMRunByAWrapper(t *testing.T) {
	b, _, _ := book(t)
	lease.SetAlive(b, func(int) bool { return true })
	s := snap()
	s.Tart = &protocol.Tart{Installed: true}
	c := cfg
	c.MaxMacOSVMs = 2
	b.Check(policy.Request{Worktree: "w1", Kind: "tart", Command: "tart run m", CostBytes: gib, MacOS: true, PID: 40, Target: "m"}, s, c)
	s.Tart.VMs = []protocol.TartVM{{Name: "m", OS: "darwin", RunPID: 41}}
	s.Tart.MacOSRunning = 1
	b.Observe(s)
	if d := b.Check(policy.Request{Worktree: "w2", Kind: "tart", Command: "tart run n", CostBytes: gib, MacOS: true}, s, c); !d.Allow {
		t.Fatalf("the wrapped VM did not settle its lease: %+v", d)
	}
}

// coveredDB is a book whose w1 run lease (2 GiB, timeout at t0+2m) holds db,
// stopped since: a start of db is covered by it.
func coveredDB(t *testing.T) (*lease.Book, *clock, *protocol.Snapshot, string) {
	t.Helper()
	b, c, _ := book(t)
	b.Observe(snap())
	run := b.Check(req("w1", 2*gib), snap(), cfg)
	s := withRun(snap(), "db", "w1", gib/8, run.LeaseID)
	b.Observe(s)
	return b, c, s, run.LeaseID
}

// expiry is when lease id times out, or the zero time when it is not open.
func expiry(b *lease.Book, id string) time.Time {
	for _, l := range b.List() {
		if l.ID == id {
			return l.Expires
		}
	}
	return time.Time{}
}

// docker start db big: db is covered, big does not fit.
func startDBAndBig() policy.Request {
	return policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db big", CostBytes: 7 * gib,
		Target: "db", ContainerID: "db", Others: []policy.Start{{ID: "big"}}}
}

// A denied start renews no lease, even one that covers part of it (#88).
func TestADeniedStartDoesNotRenewTheLeaseThatCoversIt(t *testing.T) {
	b, c, s, id := coveredDB(t)
	c.t = t0.Add(time.Minute)
	if d := b.Check(startDBAndBig(), s, cfg); d.Allow {
		t.Fatalf("start of db big: %+v, want a denial", d)
	}
	if got, want := expiry(b, id), t0.Add(2*time.Minute); !got.Equal(want) {
		t.Fatalf("the covering lease expires at %v, want %v", got, want)
	}
}

// A BUDGET_WAIT polls the same denied start: the lease that covers part of
// it still ends at its own timeout (#88).
func TestPollingADeniedStartLetsTheCoveringLeaseExpire(t *testing.T) {
	b, c, s, id := coveredDB(t)
	for c.t = t0.Add(30 * time.Second); c.t.Before(t0.Add(2 * time.Minute)); c.t = c.t.Add(30 * time.Second) {
		if d := b.Check(startDBAndBig(), s, cfg); d.Allow {
			t.Fatalf("poll at %v: %+v, want a denial", c.t, d)
		}
		if got, want := expiry(b, id), t0.Add(2*time.Minute); !got.Equal(want) {
			t.Fatalf("poll at %v: the covering lease expires at %v, want %v", c.t, got, want)
		}
	}
	c.t = t0.Add(2*time.Minute + time.Second)
	b.Observe(s)
	if got := expiry(b, id); !got.IsZero() {
		t.Fatalf("the covering lease is open past its timeout, until %v", got)
	}
}

// An allowed start renews the lease that covers it, on either way it is
// allowed: the lease waits for that container again.
func TestAnAllowedStartRenewsTheLeaseThatCoversIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    policy.Request
	}{
		// Its lease holds it: allowed with no new lease.
		{"covered", policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db", CostBytes: gib,
			Target: "db", ContainerID: "db"}},
		// db is covered, small fits: allowed with a lease for small.
		{"decided", policy.Request{Worktree: "w1", Kind: "container", Command: "docker start db small", CostBytes: gib,
			Target: "db", ContainerID: "db", Others: []policy.Start{{ID: "small"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, c, s, id := coveredDB(t)
			c.t = t0.Add(time.Minute)
			d := b.Check(tc.r, s, cfg)
			if !d.Allow || (d.LeaseID == "") != (tc.name == "covered") {
				t.Fatalf("%s: %+v", tc.r.Command, d)
			}
			if got, want := expiry(b, id), t0.Add(3*time.Minute); !got.Equal(want) {
				t.Fatalf("the covering lease expires at %v, want %v", got, want)
			}
		})
	}
}

// guessed is a compose up whose project the shim guessed: Compose's config
// failed, so the name is Compose's default, which may not be the one it
// uses (#84).
func guessed(wt, project string) policy.Request {
	return policy.Request{Worktree: wt, Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: gib, Target: project, Guessed: true, OnEngine: true}
}

// A guess that misses: the up's stack comes up under another name (name:
// in its file). Its container is ungated, as with no key, and the lease
// ends quietly at its timeout: it never says its stack never appeared.
func TestAGuessThatMissesEndsQuietly(t *testing.T) {
	b, c, log := book(t)
	b.Observe(snap())
	b.Check(guessed("w1", "proj"), snap(), cfg)
	s := withComposeProject(snap(), "x-web-1", "w1", "x", gib/4)
	b.Observe(s)
	if !strings.Contains(log.String(), "ungated") {
		t.Fatalf("x-web-1 bound the guess: %s", log)
	}
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(s)
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("log: %s", log)
	}
}

// A guess naming another worktree's same-named stack binds none of it:
// neither one that worktree checked (its own lease binds it) nor one started
// unchecked, there or unattributed (still ungated), nor by Docker's event,
// which says nothing of whose it is. The guess still holds its cost.
func TestAGuessBindsNoStackOfAnotherWorktree(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checked bool   // w2 checked its up
		wt      string // the container's worktree
	}{
		{"w2's checked stack", true, "w2"},
		{"w2's unchecked stack", false, "w2"},
		{"an unattributed stack", false, ""},
	} {
		b, _, log := book(t)
		b.Observe(snap())
		if tc.checked {
			b.Check(policy.Request{Worktree: "w2", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: gib, Target: "api", OnEngine: true}, snap(), cfg)
		}
		b.Check(guessed("w1", "api"), snap(), cfg)
		// Docker's event first: it says nothing of whose the container is.
		b.ContainerEvent("start", "api-web-1", "api-web-1", map[string]string{"com.docker.compose.project": "api"})
		b.Observe(withComposeProject(snap(), "api-web-1", tc.wt, "api", gib/4))
		for _, l := range b.List() {
			want := gib
			if l.Worktree == "w2" {
				want = gib - gib/4
			}
			if l.Bytes != want {
				t.Errorf("%s: %s reserves %d MiB, want %d", tc.name, l.Worktree, l.Bytes>>20, want>>20)
			}
		}
		if ungated := strings.Contains(log.String(), "ungated"); ungated == tc.checked {
			t.Errorf("%s: ungated logged %v; log: %s", tc.name, ungated, log)
		}
	}
}

// A guess takes no lease over, not even its own worktree's of the same
// project (compose -f typo.yml up in app/): that lease keeps its
// containers, its reservation and its timeout.
func TestAGuessTakesNoLeaseOver(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	d := b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", OnEngine: true}, snap(), cfg)
	s := withComposeProject(snap(), "app-web-1", "w1", "app", gib/2)
	b.Observe(s)
	before := expiry(b, d.LeaseID)
	c.t = c.t.Add(time.Minute)
	b.Check(guessed("w1", "app"), s, cfg)
	if got := expiry(b, d.LeaseID); !got.Equal(before) {
		t.Fatalf("the up's lease expires at %v, want %v: the guess took it over", got, before)
	}
	if r := reserved(b); r != 2*gib-gib/2+gib {
		t.Fatalf("reserved %d MiB, want the up's remainder and the guess's estimate", r>>20)
	}
}

// Nothing takes a guess over, not even a later up of the same project in its
// own worktree (compose -p api up while the guess's stack, named billing in
// its file, still pulls): both reserve, and the guess keeps its timeout.
func TestNoUpTakesAGuessOver(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(snap())
	d := b.Check(guessed("w1", "api"), snap(), cfg)
	before := expiry(b, d.LeaseID)
	c.t = c.t.Add(time.Minute)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose -p api up -d", CostBytes: gib, Target: "api", OnEngine: true}, snap(), cfg)
	if got := expiry(b, d.LeaseID); !got.Equal(before) {
		t.Fatalf("the guess's lease expires at %v, want %v: the up took it over", got, before)
	}
	if r := reserved(b); r != 2*gib {
		t.Fatalf("reserved %d MiB, want both estimates", r>>20)
	}
}

// A guess that hits: the up's containers, in its own worktree, bind its
// lease. None is ungated, and the lease ends quietly.
func TestAGuessThatHitsBindsItsStack(t *testing.T) {
	b, c, log := book(t)
	b.Observe(snap())
	b.Check(guessed("w1", "app"), snap(), cfg)
	s := withComposeProject(withComposeProject(snap(), "app-web-1", "w1", "app", gib/4), "app-db-1", "w1", "app", gib/4)
	b.Observe(s)
	if r := reserved(b); r != gib/2 {
		t.Fatalf("reserved %d MiB, want the estimate less what its containers use", r>>20)
	}
	c.t = c.t.Add(3 * time.Minute)
	b.Observe(s)
	if strings.Contains(log.String(), "ungated") || strings.Contains(log.String(), "never appeared") {
		t.Fatalf("log: %s", log)
	}
}

// A guess that hits while another worktree's checked up names the same
// project: each binds the container its own worktree runs, the guess too.
func TestAGuessThatHitsKeepsItsOwnContainerFromAnotherWorktree(t *testing.T) {
	b, _, log := book(t)
	b.Observe(snap())
	b.Check(policy.Request{Worktree: "w2", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: gib, Target: "api", OnEngine: true}, snap(), cfg)
	b.Check(guessed("w1", "api"), snap(), cfg)
	b.Observe(withComposeProject(snap(), "api-web-1", "w1", "api", gib/4))
	for _, l := range b.List() {
		want := gib
		if l.Worktree == "w1" {
			want = gib - gib/4
		}
		if l.Bytes != want {
			t.Errorf("%s reserves %d MiB, want %d", l.Worktree, l.Bytes>>20, want>>20)
		}
	}
	if strings.Contains(log.String(), "ungated") {
		t.Errorf("log: %s", log)
	}
}

// A guess that hits is confirmed: a later up of its project in its own
// worktree (compose -p api up) takes it over as any repeat up does, so that
// up's lease holds the stack. It never says the stack never appeared, nor
// lapses to hide another worktree's unchecked stack of that name.
func TestAnUpTakesAGuessThatHitOver(t *testing.T) {
	up := policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose -p api up", CostBytes: gib, Target: "api", OnEngine: true}
	for _, tc := range []struct {
		name     string
		recreate bool // the up recreates api-web-1, read before any event
		w2       bool // w2 then starts an unchecked api stack
	}{
		{"an up that starts nothing new", false, false},
		{"an up whose recreated container is read first", true, false},
		{"another worktree's unchecked stack after", false, true},
	} {
		b, c, log := book(t)
		b.Observe(snap())
		b.Check(guessed("w1", "api"), snap(), cfg)
		s := withComposeProject(snap(), "api-web-1", "w1", "api", gib/4)
		b.Observe(s)
		c.t = c.t.Add(time.Minute)
		b.Check(up, s, cfg)
		if tc.recreate {
			s = withComposeProject(snap(), "api-web-2", "w1", "api", gib/4)
			b.Observe(s)
		}
		c.t = c.t.Add(3 * time.Minute)
		b.Observe(s)
		if strings.Contains(log.String(), "never appeared") {
			t.Errorf("%s: log: %s", tc.name, log)
		}
		if tc.w2 {
			b.Observe(addContainer(s, protocol.Container{ID: "api-x-1", Name: "api-x-1", MemoryBytes: gib / 4,
				Labels: map[string]string{"com.docker.compose.project": "api", protocol.ComposeWorkingDirLabel: "/w2/api"}}, "w2"))
			if !strings.Contains(log.String(), "ungated") {
				t.Errorf("%s: api-x-1 is not ungated; log: %s", tc.name, log)
			}
		}
	}
}

// A guess and a real key of one project, in one worktree or two, in either
// order, by Docker's event or the reading (#84). Whatever the order: a guess
// binds only what the reading attributes to its own worktree, takes no
// lease's reservation, never makes a real lease warn "never appeared", and
// hides no unchecked container. Steps run in order; reserves checks what
// each worktree's leases still hold.
func TestAGuessAndARealKeyOfOneProject(t *testing.T) {
	type ctr struct{ id, wt string }
	web1 := ctr{"api-web-1", "w1"} // the up's stack, in w1
	web2 := ctr{"api-web-2", "w1"} // web1 recreated
	x := ctr{"api-x-1", "w2"}      // w2's stack of the same name
	type harness struct {
		b       *lease.Book
		c       *clock
		running []ctr
	}
	labels := func(k ctr) map[string]string {
		return map[string]string{"com.docker.compose.project": "api", protocol.ComposeWorkingDirLabel: "/" + k.wt + "/api"}
	}
	s := func(h *harness) *protocol.Snapshot {
		out := snap()
		for _, k := range h.running {
			addContainer(out, protocol.Container{ID: k.id, Name: k.id, MemoryBytes: gib / 4, Labels: labels(k)}, k.wt)
		}
		return out
	}
	type step func(t *testing.T, h *harness)
	up := func(wt string) policy.Request {
		return policy.Request{Worktree: wt, Kind: "compose", Op: "up", Command: "docker compose -p api up", CostBytes: gib, Target: "api", OnEngine: true}
	}
	check := func(r policy.Request) step {
		return func(_ *testing.T, h *harness) { h.b.Check(r, s(h), cfg) }
	}
	guess := check(guessed("w1", "api"))
	upIn := func(wt string) step { return check(up(wt)) }
	start := func(k ctr) step { // starts k; neither event nor reading yet
		return func(_ *testing.T, h *harness) { h.running = append(h.running, k) }
	}
	stop := func(k ctr) step {
		return func(_ *testing.T, h *harness) {
			h.running = slices.DeleteFunc(h.running, func(o ctr) bool { return o == k })
		}
	}
	event := func(k ctr) step {
		return func(_ *testing.T, h *harness) { h.b.ContainerEvent("start", k.id, k.id, labels(k)) }
	}
	reading := func(_ *testing.T, h *harness) { h.b.Observe(s(h)) }
	wait := func(_ *testing.T, h *harness) { h.c.t = h.c.t.Add(3 * time.Minute); h.b.Observe(s(h)) }
	reserves := func(w1, w2 uint64) step {
		return func(t *testing.T, h *harness) {
			got := map[string]uint64{}
			for _, l := range h.b.List() {
				got[l.Worktree] += l.Bytes
			}
			if got["w1"] != w1 || got["w2"] != w2 {
				t.Errorf("reserved w1 %d MiB, w2 %d MiB; want %d, %d", got["w1"]>>20, got["w2"]>>20, w1>>20, w2>>20)
			}
		}
	}
	const q = gib / 4 // what each container uses
	for _, tc := range []struct {
		name      string
		before    []ctr  // running before the guess, held by no lease
		steps     []step // after a first reading
		unchecked []ctr  // started past the gate: ungated
	}{
		// The guess misses: its stack comes up under another name.
		{name: "S3 miss, then a real up", steps: []step{guess, upIn("w1"), reserves(2*gib, 0)}},
		{name: "F1a miss over a stack already running, then a real up",
			before: []ctr{web1}, steps: []step{guess, upIn("w1"), reserves(2*gib, 0), wait}},
		{name: "F1b miss over a stack already running, w2's unchecked by reading",
			before: []ctr{web1}, steps: []step{guess, start(x), reading, reserves(gib, 0)}, unchecked: []ctr{x}},
		{name: "F1c miss over a stack already running, w2's unchecked by event",
			before: []ctr{web1}, steps: []step{guess, start(x), event(x), reading, reserves(gib, 0)}, unchecked: []ctr{x}},
		// The guess hits, then a real up of the project in w1.
		{name: "N1a hit, then a real up that starts nothing new",
			steps: []step{guess, start(web1), reading, upIn("w1"), reserves(gib, 0), wait, start(x), reading}, unchecked: []ctr{x}},
		{name: "N1b hit, then a real up whose recreated container is read first",
			steps: []step{guess, start(web1), reading, upIn("w1"), stop(web1), start(web2), reading, wait}},
		{name: "N1c hit by event, then a real up",
			steps: []step{guess, start(web1), event(web1), reading, upIn("w1"), reserves(gib, 0), wait}},
		// A real up of the project in w1 before the guess binds.
		{name: "F2a real up first, stack read first",
			steps: []step{guess, upIn("w1"), start(web1), reading, reserves(gib+gib-q, 0), wait, start(x), reading}, unchecked: []ctr{x}},
		{name: "F2b real up first, stack by event first",
			steps: []step{guess, upIn("w1"), start(web1), event(web1), reading, reserves(gib+gib-q, 0), wait, start(x), reading}, unchecked: []ctr{x}},
		{name: "F2c real up checked first, then the guess, stack read first",
			steps: []step{upIn("w1"), guess, start(web1), reading, reserves(gib+gib-q, 0), wait}},
		// The guess hits, then w2 starts a stack of the same name unchecked.
		{name: "F3a hit, w2's unchecked by reading",
			steps: []step{guess, start(web1), reading, start(x), reading, reserves(gib-q, 0)}, unchecked: []ctr{x}},
		{name: "F3b hit, w2's unchecked by event",
			steps: []step{guess, start(web1), reading, start(x), event(x), reading, reserves(gib-q, 0)}, unchecked: []ctr{x}},
		{name: "F3c hit and w2's unchecked in one reading",
			steps: []step{guess, start(web1), start(x), reading, reserves(gib-q, 0)}, unchecked: []ctr{x}},
		// w2's real up of the same project: each binds its own worktree's.
		{name: "S1a w2's real up, the guess hits, read first",
			steps: []step{upIn("w2"), guess, start(web1), reading, reserves(gib-q, gib)}},
		{name: "S1b w2's real up, the guess hits, by event first",
			steps: []step{upIn("w2"), guess, start(web1), event(web1), reading, reserves(gib-q, gib)}},
		{name: "S1c w2's real up, w2's stack by event, the guess misses",
			steps: []step{upIn("w2"), guess, start(x), event(x), reading, reserves(gib, gib-q)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, c, log := book(t)
			h := &harness{b: b, c: c, running: slices.Clone(tc.before)}
			b.Observe(s(h))
			for _, st := range tc.steps {
				st(t, h)
			}
			if strings.Contains(log.String(), "never appeared") {
				t.Errorf("a lease warned its stack never appeared: %s", log)
			}
			var want []string
			for _, k := range tc.unchecked {
				want = append(want, "container:"+k.id)
			}
			if got := ungatedKeys(b); !slices.Equal(got, want) {
				t.Errorf("ungated %v, want %v", got, want)
			}
		})
	}
}

// w1 and w2 use one project name (#89). w1's container crashes, w2
// checks an up of the name, and w1's restart policy brings the container
// back: it is w1's, so it binds nothing of w2's lease. w2's reservation
// stands, and its own containers bind its lease.
func TestARestartAfterACrashBindsNoLeaseOfAnotherWorktree(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(read(snap(), c.t))
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 4 * gib, Target: "app", OnEngine: true}, snap(), cfg)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(withComposeContainer(snap(), "a1", "w1", 4*gib), c.t)) // it uses w1's lease, which ends
	lab := map[string]string{protocol.ComposeProjectLabel: "app"}
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("die", "a1", "a1", lab) // a crash: no stop
	c.t = c.t.Add(time.Second)
	b.Check(policy.Request{Worktree: "w2", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", OnEngine: true}, snap(), cfg)
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("start", "a1", "a1", lab) // the restart policy
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(withComposeContainer(snap(), "a1", "w1", 4*gib), c.t))
	if got := reserved(b); got != 2*gib {
		t.Fatalf("reserved = %d MiB, want w2's 2048 MiB: %+v", got>>20, b.List())
	}
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("start", "b1", "b1", lab)
	c.t = c.t.Add(5 * time.Second)
	s := withComposeContainer(snap(), "a1", "w1", 4*gib)
	b.Observe(read(withComposeContainer(s, "b1", "w2", gib), c.t))
	if got := reserved(b); got != gib {
		t.Fatalf("reserved = %d MiB, want w2's 1024 MiB left: %+v", got>>20, b.List())
	}
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

// A restart after a crash, after its own worktree checked an up of its
// project, binds that up's lease, though the reading lost its attribution.
func TestARestartAfterACrashBindsALeaseOfItsOwnWorktree(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(read(snap(), c.t))
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 4 * gib, Target: "app", OnEngine: true}, snap(), cfg)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(withComposeContainer(snap(), "a1", "w1", 4*gib), c.t))
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(withComposeContainer(snap(), "a1", "", 4*gib), c.t)) // attribution lost for a reading
	lab := map[string]string{protocol.ComposeProjectLabel: "app"}
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("die", "a1", "a1", lab)
	c.t = c.t.Add(time.Second)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", OnEngine: true}, snap(), cfg)
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("start", "a1", "a1", lab)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(withComposeContainer(snap(), "a1", "w1", 4*gib), c.t))
	if l := b.List(); len(l) != 0 {
		t.Fatalf("leases = %+v, want w1's up bound and ended", l)
	}
}

// A container no reading attributed (no Orca reading, or its compose
// directory and mounts in no worktree) is the worktree's whose lease it
// bound: after a crash, its restart binds that worktree's up.
func TestARestartAfterACrashOfAnUnattributedContainerBindsTheWorktreeItBound(t *testing.T) {
	b, c, log := book(t)
	b.Observe(read(snap(), c.t))
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 4 * gib, Target: "app", OnEngine: true}, snap(), cfg)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(withComposeContainer(snap(), "a1", "", 4*gib), c.t))
	lab := map[string]string{protocol.ComposeProjectLabel: "app"}
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("die", "a1", "a1", lab)
	c.t = c.t.Add(time.Second)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", OnEngine: true}, snap(), cfg)
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("start", "a1", "a1", lab)
	for range 30 {
		c.t = c.t.Add(5 * time.Second)
		b.Observe(read(withComposeContainer(snap(), "a1", "", 4*gib), c.t))
	}
	if l := b.List(); len(l) != 0 || strings.Contains(log.String(), "never appeared") {
		t.Fatalf("leases = %+v, want w1's up bound and ended\n%s", l, log)
	}
}

// As above, with readings between the crash and the restart: the reading
// that shows the container back binds nothing of w2's either.
func TestAContainerBackAfterACrashBindsNoLeaseOfAnotherWorktree(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(read(snap(), c.t))
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 4 * gib, Target: "app", OnEngine: true}, snap(), cfg)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(withComposeContainer(snap(), "a1", "w1", 4*gib), c.t))
	lab := map[string]string{protocol.ComposeProjectLabel: "app"}
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("die", "a1", "a1", lab) // a crash: no stop
	for range 3 {
		c.t = c.t.Add(5 * time.Second)
		b.Observe(read(snap(), c.t)) // the restart policy's backoff
	}
	b.Check(policy.Request{Worktree: "w2", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", OnEngine: true}, snap(), cfg)
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("start", "a1", "a1", lab)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(withComposeContainer(snap(), "a1", "w1", 4*gib), c.t))
	if got := reserved(b); got != 2*gib {
		t.Fatalf("reserved = %d MiB, want w2's 2048 MiB: %+v", got>>20, b.List())
	}
	c.t = c.t.Add(5 * time.Second)
	s := withComposeContainer(snap(), "a1", "w1", 4*gib)
	b.Observe(read(withComposeContainer(s, "b1", "w2", gib), c.t))
	if got := reserved(b); got != gib {
		t.Fatalf("reserved = %d MiB, want w2's 1024 MiB left: %+v", got>>20, b.List())
	}
	if got := ungatedKeys(b); len(got) != 0 {
		t.Fatalf("ungated = %v", got)
	}
}

// A container back after a crash and readings, after its own worktree
// checked an up of its project, binds that up's lease.
func TestAContainerBackAfterACrashBindsALeaseOfItsOwnWorktree(t *testing.T) {
	b, c, log := book(t)
	b.Observe(read(snap(), c.t))
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 4 * gib, Target: "app", OnEngine: true}, snap(), cfg)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(withComposeContainer(snap(), "a1", "w1", 4*gib), c.t))
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("die", "a1", "a1", map[string]string{protocol.ComposeProjectLabel: "app"})
	for range 3 {
		c.t = c.t.Add(5 * time.Second)
		b.Observe(read(snap(), c.t))
	}
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", OnEngine: true}, snap(), cfg)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(withComposeContainer(snap(), "a1", "w1", 4*gib), c.t)) // no event: the reading shows it
	if l := b.List(); len(l) != 0 || strings.Contains(log.String(), "never appeared") {
		t.Fatalf("leases = %+v, want w1's up bound and ended\n%s", l, log)
	}
}

// The reading that shows a container back after a crash names its owner,
// over the readings before: a container w2's before its crash, w1's now,
// binds w1's up.
func TestAContainerBackAfterACrashIsWhoseItsReadingSays(t *testing.T) {
	b, c, log := book(t)
	b.Observe(read(withComposeContainer(snap(), "a1", "w2", 4*gib), c.t))
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("die", "a1", "a1", map[string]string{protocol.ComposeProjectLabel: "app"})
	for range 3 {
		c.t = c.t.Add(5 * time.Second)
		b.Observe(read(snap(), c.t))
	}
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", OnEngine: true}, snap(), cfg)
	for range 30 {
		c.t = c.t.Add(5 * time.Second)
		b.Observe(read(withComposeContainer(snap(), "a1", "w1", 4*gib), c.t))
	}
	if l := b.List(); len(l) != 0 || strings.Contains(log.String(), "never appeared") {
		t.Fatalf("leases = %+v, want w1's up bound and ended\n%s", l, log)
	}
}

// A start by ID names the container, not whose it is (#89): w2's run
// dressed as a container of w1's project, started by ID from w1, is still
// w2's run, and after a crash its restart binds nothing of w1's up.
func TestARestartAfterACrashOfARunStartedByIDFromAnotherWorktreeBindsNoLeaseOfThatWorktree(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(read(snap(), c.t))
	run := b.Check(policy.Request{Worktree: "w2", Kind: "container", Command: "docker run -l com.docker.compose.project=app img", CostBytes: gib, Labelled: true}, snap(), cfg)
	lab := map[string]string{protocol.ComposeProjectLabel: "app", protocol.ComposeConfigHashLabel: "h", protocol.LeaseLabel: run.LeaseID}
	dressed := func() *protocol.Snapshot {
		return addContainer(snap(), protocol.Container{ID: "c1", Name: "c1", MemoryBytes: gib, Labels: lab}, "")
	}
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(dressed(), c.t))
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("die", "c1", "c1", lab) // a crash: no stop
	c.t = c.t.Add(time.Second)
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start c1", CostBytes: gib, Target: "c1", ContainerID: "c1", OnEngine: true, TakesOver: protocol.LeaseOf(lab)}, snap(), cfg)
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("start", "c1", "c1", lab)
	for range 60 { // the run's lease lapses: its label marks no live lease
		c.t = c.t.Add(5 * time.Second)
		b.Observe(read(dressed(), c.t))
	}
	if l := b.List(); len(l) != 0 {
		t.Fatalf("leases = %+v, want the start's bound and ended", l)
	}
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("die", "c1", "c1", lab)
	c.t = c.t.Add(time.Second)
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", OnEngine: true}, snap(), cfg)
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("start", "c1", "c1", lab) // the restart policy
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(dressed(), c.t))
	if got := reserved(b); got != 2*gib {
		t.Fatalf("reserved = %d MiB, want w1's 2048 MiB: %+v", got>>20, b.List())
	}
}

// A crash is remembered from the die until a reading begun after it shows
// the container again, whatever readings come between (#89): one begun
// before the die still lists the container, and the readings of a restart
// backoff do not. Then the restart,
// by its start event or by the reading that shows it back, binds a
// project's up only in its owner's worktree: w2's up keeps its
// reservation, and w1's binds.
func TestACrashLastsUntilAReadingShowsTheRestart(t *testing.T) {
	type row struct {
		span    bool   // a reading begun before the die ends after it, listing the container
		backoff int    // readings without the container, 5 s apart, before the restart
		event   bool   // the restart's start event arrives; else only a reading shows it
		up      string // the worktree that checks an up of the project before the restart
	}
	var rows []row
	for _, span := range []bool{false, true} {
		for _, backoff := range []int{0, 1, 3} {
			for _, event := range []bool{true, false} {
				for _, up := range []string{"w1", "w2"} {
					if backoff == 0 && !event {
						continue // back within a tick and no event: nothing shows it went
					}
					rows = append(rows, row{span, backoff, event, up})
				}
			}
		}
	}
	for _, x := range rows {
		t.Run(fmt.Sprintf("span=%v/backoff=%d/event=%v/up=%s", x.span, x.backoff, x.event, x.up), func(t *testing.T) {
			b, c, log := book(t)
			a1 := func() *protocol.Snapshot { return withComposeContainer(snap(), "a1", "w1", 4*gib) }
			b.Observe(read(snap(), c.t))
			b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 4 * gib, Target: "app", OnEngine: true}, snap(), cfg)
			c.t = c.t.Add(5 * time.Second)
			b.Observe(read(a1(), c.t)) // it uses w1's lease, which ends
			lab := map[string]string{protocol.ComposeProjectLabel: "app"}
			c.t = c.t.Add(5 * time.Second)
			died := c.t
			b.ContainerEvent("die", "a1", "a1", lab) // a crash: no stop
			if x.span {
				c.t = c.t.Add(time.Second)
				b.Observe(read(a1(), died.Add(-time.Second)))
			}
			for range x.backoff {
				c.t = c.t.Add(5 * time.Second)
				b.Observe(read(snap(), c.t))
			}
			c.t = c.t.Add(time.Second)
			b.Check(policy.Request{Worktree: x.up, Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", OnEngine: true}, snap(), cfg)
			if x.event {
				c.t = c.t.Add(time.Second)
				b.ContainerEvent("start", "a1", "a1", lab) // the restart policy
			}
			c.t = c.t.Add(5 * time.Second)
			b.Observe(read(a1(), c.t))
			if x.up == "w2" {
				if got := reserved(b); got != 2*gib {
					t.Fatalf("reserved = %d MiB, want w2's 2048 MiB: %+v", got>>20, b.List())
				}
				return
			}
			if l := b.List(); len(l) != 0 || strings.Contains(log.String(), "never appeared") {
				t.Fatalf("leases = %+v, want w1's up bound and ended\n%s", l, log)
			}
		})
	}
}

// A reading begun after the restart ends the crash: the container, of no
// known worktree, missing later from readings with no event and back,
// binds its worktree's up as any container back does.
func TestAReadingThatShowsTheRestartEndsTheCrash(t *testing.T) {
	b, c, log := book(t)
	a1 := func() *protocol.Snapshot { return withComposeContainer(snap(), "a1", "", 4*gib) }
	b.Observe(read(snap(), c.t))
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(a1(), c.t))
	lab := map[string]string{protocol.ComposeProjectLabel: "app"}
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("die", "a1", "a1", lab) // a crash: no stop
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("start", "a1", "a1", lab)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(a1(), c.t))
	for range 2 { // missing, with no event: its stats failed
		c.t = c.t.Add(5 * time.Second)
		b.Observe(read(snap(), c.t))
	}
	b.Check(policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose up", CostBytes: 2 * gib, Target: "app", OnEngine: true}, snap(), cfg)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(a1(), c.t))
	if l := b.List(); len(l) != 0 || strings.Contains(log.String(), "never appeared") {
		t.Fatalf("leases = %+v, want w1's up bound and ended\n%s", l, log)
	}
}

// docker start c1 c2 covers starting each once: c2 died while its lease
// waits on c1, so a compose up checked since that starts it again takes it,
// and its use counts against the up's lease, not the start's (#111).
func TestAContainerThatDiedLeavesItsStartOfSeveral(t *testing.T) {
	b, c, log := book(t)
	p1 := func(mem1, mem2 uint64) *protocol.Snapshot {
		s := snap()
		if mem1 > 0 {
			withComposeProject(s, "c1", "w1", "p1", mem1)
		}
		if mem2 > 0 {
			withComposeProject(s, "c2", "w1", "p1", mem2)
		}
		return read(s, c.t)
	}
	lab := map[string]string{protocol.ComposeProjectLabel: "p1"}
	up := policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose -p p1 up -d", CostBytes: 2 * gib, Target: "p1", OnEngine: true}
	b.Observe(read(snap(), c.t))
	b.Check(up, snap(), cfg)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(p1(gib, gib)) // the up's lease ends
	for _, id := range []string{"c1", "c2"} {
		b.ContainerEvent("stop", id, id, lab)
		b.ContainerEvent("die", id, id, lab)
	}
	c.t = c.t.Add(5 * time.Second)
	b.Observe(p1(0, 0))
	start := b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start c1 c2", Target: "c1", ContainerID: "c1",
		Others: []policy.Start{{ID: "c2"}}, CostBytes: 2 * gib, OnEngine: true}, p1(0, 0), cfg)
	b.ContainerEvent("start", "c1", "c1", lab)
	b.ContainerEvent("start", "c2", "c2", lab)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(p1(gib/8, gib/8))
	held := func() uint64 {
		i := slices.IndexFunc(b.List(), func(l protocol.Lease) bool { return l.ID == start.LeaseID })
		if i < 0 {
			t.Fatalf("leases = %+v, want %s open", b.List(), start.LeaseID)
		}
		return b.List()[i].Bytes
	}
	cost := held() + gib/4
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("die", "c2", "c2", lab) // a crash
	c.t = c.t.Add(time.Second)
	up.CostBytes = gib
	d := b.Check(up, p1(gib/8, 0), cfg)
	b.ContainerEvent("start", "c2", "c2", lab)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(p1(gib/8, gib))
	if slices.ContainsFunc(b.List(), func(l protocol.Lease) bool { return l.ID == d.LeaseID }) {
		t.Fatalf("leases = %+v, want %s bound to c2 and ended", b.List(), d.LeaseID)
	}
	if got, want := held(), cost-gib/8; got != want {
		t.Fatalf("%s holds %d MiB, want %d MiB: its cost less c1's use", start.LeaseID, got>>20, want>>20)
	}
	for range 30 {
		c.t = c.t.Add(5 * time.Second)
		b.Observe(p1(gib/8, gib))
	}
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("log: %s", log)
	}
}

// p1Started is the book of TestAContainerThatDiedLeavesItsStartOfSeveral
// as c2 runs again under docker start c1 c2: p1 reads c1 and c2 in w1's
// compose project p1, and up is w1's compose up of p1.
func p1Started(t *testing.T) (b *lease.Book, c *clock, log *bytes.Buffer, p1 func(mem1, mem2 uint64) *protocol.Snapshot, up policy.Request) {
	b, c, log = book(t)
	p1 = func(mem1, mem2 uint64) *protocol.Snapshot {
		s := snap()
		if mem1 > 0 {
			withComposeProject(s, "c1", "w1", "p1", mem1)
		}
		if mem2 > 0 {
			withComposeProject(s, "c2", "w1", "p1", mem2)
		}
		return read(s, c.t)
	}
	lab := map[string]string{protocol.ComposeProjectLabel: "p1"}
	up = policy.Request{Worktree: "w1", Kind: "compose", Op: "up", Command: "docker compose -p p1 up -d", CostBytes: 2 * gib, Target: "p1", OnEngine: true}
	b.Observe(read(snap(), c.t))
	b.Check(up, snap(), cfg)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(p1(gib, gib))
	for _, id := range []string{"c1", "c2"} {
		b.ContainerEvent("stop", id, id, lab)
		b.ContainerEvent("die", id, id, lab)
	}
	c.t = c.t.Add(5 * time.Second)
	b.Observe(p1(0, 0))
	b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start c1 c2", Target: "c1", ContainerID: "c1",
		Others: []policy.Start{{ID: "c2"}}, CostBytes: 2 * gib, OnEngine: true}, p1(0, 0), cfg)
	b.ContainerEvent("start", "c1", "c1", lab)
	b.ContainerEvent("start", "c2", "c2", lab)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(p1(gib/8, gib/8))
	up.CostBytes = gib
	return b, c, log, p1, up
}

// With no events, c2 died when the first reading missed it, not when the
// second made it gone: a compose up checked between the two that starts it
// takes it (#111).
func TestAStartOfSeveralDatesADeathAtTheFirstReadingThatMissedIt(t *testing.T) {
	b, c, log, p1, up := p1Started(t)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(p1(gib/8, 0)) // c2 crashed: missing once
	c.t = c.t.Add(time.Second)
	d := b.Check(up, p1(gib/8, 0), cfg)
	c.t = c.t.Add(4 * time.Second)
	b.Observe(p1(gib/8, 0)) // gone
	c.t = c.t.Add(5 * time.Second)
	b.Observe(p1(gib/8, gib))
	if slices.ContainsFunc(b.List(), func(l protocol.Lease) bool { return l.ID == d.LeaseID }) {
		t.Fatalf("leases = %+v, want %s bound to c2 and ended", b.List(), d.LeaseID)
	}
	for range 30 {
		c.t = c.t.Add(5 * time.Second)
		b.Observe(p1(gib/8, gib))
	}
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("log: %s", log)
	}
}

// startTwo is docker start c1 c2, both resolved.
var startTwo = policy.Request{Worktree: "w1", Kind: "container", Command: "docker start c1 c2", Target: "c1", ContainerID: "c1",
	Others: []policy.Start{{ID: "c2"}}, CostBytes: 2 * gib, OnEngine: true}

// withC1C2 is a reading of c1 and c2 in w1, each at its use in cs.
func withC1C2(head uint64, cs map[string]uint64) *protocol.Snapshot {
	s := snap()
	s.Budget.HeadroomBytes = i64(int64(head))
	for _, id := range []string{"c1", "c2"} {
		if m, ok := cs[id]; ok {
			withContainerMem(s, id, "w1", m)
		}
	}
	return s
}

// docker stop c2 && docker start c2 after docker start c1 c2: the start's
// lease still covers c2, so the second start is allowed, with no lease,
// however low headroom is (#111).
func TestAStartOfSeveralCoversItsStopAndStart(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(read(withC1C2(8*gib, nil), c.t))
	b.Check(startTwo, withC1C2(8*gib, nil), cfg)
	b.ContainerEvent("start", "c1", "c1", nil)
	b.ContainerEvent("start", "c2", "c2", nil)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(withC1C2(8*gib, map[string]uint64{"c1": gib / 8, "c2": gib / 8}), c.t))
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("stop", "c2", "c2", nil)
	b.ContainerEvent("die", "c2", "c2", nil)
	c.t = c.t.Add(4 * time.Second)
	s := read(withC1C2(2*gib+gib/2, map[string]uint64{"c1": gib / 8}), c.t)
	b.Observe(s)
	d := b.Check(policy.Request{Worktree: "w1", Kind: "container", Command: "docker start c2", Target: "c2", ContainerID: "c2", CostBytes: gib, OnEngine: true}, s, cfg)
	if !d.Allow || d.LeaseID != "" {
		t.Fatalf("docker start c2 = %+v, want allowed by the start's lease", d)
	}
}

// A restart policy restarting c2 after a crash: c2 stays the start's, and
// its use counts against the start's reservation (#111).
func TestAStartOfSeveralCountsARestartByPolicy(t *testing.T) {
	b, c, _ := book(t)
	b.Observe(read(withC1C2(8*gib, nil), c.t))
	b.Check(startTwo, withC1C2(8*gib, nil), cfg)
	b.ContainerEvent("start", "c1", "c1", nil)
	b.ContainerEvent("start", "c2", "c2", nil)
	c.t = c.t.Add(5 * time.Second)
	b.Observe(read(withC1C2(8*gib, map[string]uint64{"c1": gib / 8, "c2": gib / 8}), c.t))
	cost := reserved(b) + gib/4
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("die", "c2", "c2", nil) // a crash
	c.t = c.t.Add(time.Second)
	b.ContainerEvent("start", "c2", "c2", nil) // its restart policy
	for range 2 {
		c.t = c.t.Add(5 * time.Second)
		b.Observe(read(withC1C2(4*gib+gib/2, map[string]uint64{"c1": gib / 2, "c2": gib}), c.t))
	}
	if got, want := reserved(b), cost-gib/2-gib; got != want {
		t.Fatalf("reserved = %d MiB, want %d MiB: the cost less c1's and c2's use", got>>20, want>>20)
	}
}
