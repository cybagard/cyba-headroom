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
	b, _, _ := book(t)
	b.Observe(snap())
	down := snap()
	down.Docker = &protocol.Docker{} // Docker Desktop quit
	b.Observe(down)
	// It is back, and restarts its --restart always containers.
	b.Observe(withContainer(snap(), "c2", "w1"))
	if k := ungatedKeys(b); len(k) != 0 {
		t.Fatalf("ungated = %v", k)
	}
	// After that first reading, new containers count again.
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
			d := b.Check(req("", 6*gib), snap(), cfg)
			if !d.Allow {
				t.Fatalf("manual = %+v", d)
			}
			// It holds nothing back from gated calls.
			if d := b.Check(req("w2", 7*gib), snap(), cfg); !d.Allow {
				t.Fatalf("a manual call held back memory: %+v", d)
			}
			b.Observe(withContainer(snap(), "c2", wt))
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
