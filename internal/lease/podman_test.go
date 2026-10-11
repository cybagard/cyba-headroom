package lease_test

import (
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// podmanSnap is snap with the Podman source read (no machine running) and
// a running Podman engine with no containers yet.
func podmanSnap() *protocol.Snapshot {
	s := snap()
	s.Sources = map[string]protocol.SourceStatus{"podman": {At: t0}}
	s.Engines = append(s.Engines, protocol.Engine{Kind: protocol.EnginePodman, ID: "podman:podman-machine-default",
		Machine: "podman-machine-default", Running: true, VMRunning: true})
	return s
}

// withMachine adds a running Podman machine's VM.
func withMachine(s *protocol.Snapshot, machine string, footprint uint64) *protocol.Snapshot {
	if s.Podman == nil {
		s.Podman = &protocol.Podman{}
	}
	s.Podman.VMs = append(s.Podman.VMs, protocol.PodmanVM{Machine: machine, PID: 4242, FootprintBytes: footprint})
	return s
}

// withPodContainer adds a container of a pod to the Podman engine.
func withPodContainer(s *protocol.Snapshot, id, pod, podID string, mem uint64) *protocol.Snapshot {
	e := &s.Engines[len(s.Engines)-1]
	e.Containers = append(e.Containers, protocol.Container{ID: id, Name: id, MemoryBytes: mem, Pod: pod, PodID: podID})
	return s
}

func machineStart(wt, machine string, cost uint64) policy.Request {
	return policy.Request{Worktree: wt, Kind: "machine", Op: "start", Command: "podman machine start " + machine, CostBytes: cost, Target: machine}
}

// A podman machine start's lease binds that machine's VM by its name, as a
// tart run's binds its VM: the VM it binds is not ungated, and the lease
// holds what the VM does not use yet. Another machine's VM, or a Tart VM
// of the same name, binds nothing (#230).
func TestAMachineStartLeaseBindsItsPodmanVM(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(podmanSnap())
	if d := b.Check(machineStart("w1", "dev", 2*gib), podmanSnap(), cfg); !d.Allow {
		t.Fatalf("check = %+v", d)
	}
	s := podmanSnap()
	s.Tart.VMs = []protocol.TartVM{{Name: "dev", OS: "linux", MemoryBytes: gib}}
	b.Observe(withMachine(s, "other", gib))
	if reserved(b) != 2*gib {
		t.Fatalf("another machine's VM or a Tart VM bound the lease: reserved %d", reserved(b))
	}
	if k := ungatedKeys(b); len(k) != 2 || k[0] != "machine:other" || k[1] != "vm:dev" {
		t.Fatalf("ungated = %v", k)
	}
	b.Observe(withMachine(podmanSnap(), "dev", gib))
	if ls := b.List(); len(ls) != 1 || ls[0].Bytes != gib {
		t.Fatalf("the machine's VM did not bind its lease: %+v", ls)
	}
	for _, u := range b.Ungated() {
		if u.Key == "machine:dev" {
			t.Fatalf("the lease's VM is ungated: %+v", u)
		}
	}
	b.Observe(withMachine(podmanSnap(), "dev", 2*gib)) // it grew into its cost
	if len(b.List()) != 0 {
		t.Fatalf("lease open after its VM used its cost: %+v", b.List())
	}
}

// The default machine's start names it: the shim asks inspect. A machine
// start's lease ends when its VM stops.
func TestAMachineStartLeaseEndsWithItsVM(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(podmanSnap())
	b.Check(machineStart("w1", "podman-machine-default", 2*gib), podmanSnap(), cfg)
	b.Observe(withMachine(podmanSnap(), "podman-machine-default", gib))
	if reserved(b) != gib {
		t.Fatalf("reserved %d", reserved(b))
	}
	b.Observe(podmanSnap())
	if len(b.List()) != 0 {
		t.Fatalf("lease open after its VM stopped: %+v", b.List())
	}
}

// A machine that ran before the Podman source's first reading is a
// baseline, not ungated.
func TestAMachineRunningAtTheFirstReadingIsABaseline(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(withMachine(podmanSnap(), "dev", gib))
	if k := ungatedKeys(b); len(k) != 0 {
		t.Fatalf("ungated = %v", k)
	}
}

func podStart(wt string, pods ...string) policy.Request {
	return policy.Request{Worktree: wt, Kind: "pod", Op: "start", Command: "podman pod start " + pods[0], Target: pods[0], Pods: pods}
}

// A pod start's or kube play's lease binds each container the Podman
// engine says is of its pods, the infra container too, while it is open:
// by the pod's name, exactly. A container of another pod, even one whose
// ID starts with the name, or one of the same name on another engine,
// binds nothing (#230, plan amendment 3).
func TestAPodLeaseBindsItsPodsContainers(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(podmanSnap())
	if d := b.Check(podStart("w1", "web", "db"), podmanSnap(), cfg); !d.Allow || d.CostBytes != gib {
		t.Fatalf("check = %+v", d) // one container's cost
	}
	s := withPodContainer(podmanSnap(), "web-infra", "web", "a1b2c3", 0)
	s = withPodContainer(s, "web-app", "web", "a1b2c3", gib/4)
	s = withPodContainer(s, "other-app", "other", "d4e5f6", gib/4)
	s = withContainerMem(s, "web", "w1", gib/4) // Docker Desktop's, named as the pod
	b.Observe(s)
	if ls := b.List(); len(ls) != 1 || ls[0].Bytes != gib*3/4 {
		t.Fatalf("leases = %+v", ls)
	}
	if k := ungatedKeys(b); len(k) != 2 || k[0] != "container:other-app" || k[1] != "container:web" {
		t.Fatalf("ungated = %v", k)
	}
	s = withPodContainer(s, "db-app", "db", "0f0f0f", gib/2) // its other pod, a tick later
	b.Observe(s)
	if ls := b.List(); len(ls) != 1 || ls[0].Bytes != gib/4 {
		t.Fatalf("its second pod did not bind: %+v", ls)
	}
	b.Observe(podmanSnap())
	b.Observe(podmanSnap())
	if len(b.List()) != 0 {
		t.Fatalf("lease open after its pods stopped: %+v", b.List())
	}

	// Pod IDs are hex: "db" is a name, never a prefix of another pod's ID.
	b, _, _ = book(t)
	b.Observe(podmanSnap())
	b.Check(podStart("w1", "db"), podmanSnap(), cfg)
	b.Observe(withPodContainer(podmanSnap(), "other-app", "other", "db5e6f7a8b9c", gib/4))
	if ls := b.List(); len(ls) != 1 || ls[0].Bytes != gib {
		t.Fatalf("a lease of pod db bound pod other (ID db5e…): %+v", ls)
	}
}

// withPodContainerIn is withPodContainer attributed to worktree wt.
func withPodContainerIn(s *protocol.Snapshot, id, pod, wt string, mem uint64) *protocol.Snapshot {
	s = withPodContainer(s, id, pod, "", mem)
	for i := range s.Attribution.Worktrees {
		if s.Attribution.Worktrees[i].ID == wt {
			s.Attribution.Worktrees[i].Containers = append(s.Attribution.Worktrees[i].Containers, protocol.AttributedContainer{ID: id, Name: id})
		}
	}
	return s
}

// A pod lease with no pod to name (pod start --all, a kube file on stdin,
// at a URL or of a Deployment) binds, while it is open, the new Podman
// containers in a pod that are attributed to its worktree and that no
// other lease keys: none of them is ungated. Not another worktree's, nor
// one in no pod (#230, plan amendment 3).
func TestAKeylessPodLeaseBindsItsWorktreesNewPodContainers(t *testing.T) {
	for _, r := range []policy.Request{
		{Worktree: "w1", Kind: "pod", Op: "start", Command: "podman pod start"},
		{Worktree: "w1", Kind: "pod", Op: "play", Command: "podman kube play"},
	} {
		b, _, _ := book(t)
		b.Observe(podmanSnap())
		b.Check(r, podmanSnap(), cfg)
		s := withPodContainerIn(podmanSnap(), "web-infra", "web", "w1", 0)
		s = withPodContainerIn(s, "web-app", "web", "w1", gib/4)
		s = withPodContainerIn(s, "w2-app", "other", "w2", gib/4)
		s = addContainer(s, protocol.Container{ID: "plain", Name: "plain", MemoryBytes: gib / 4}, "w1")
		b.Observe(s)
		if ls := b.List(); len(ls) != 1 || ls[0].Bytes != gib*3/4 {
			t.Fatalf("%s: leases = %+v", r.Command, ls)
		}
		if k := ungatedKeys(b); len(k) != 2 || k[0] != "container:plain" || k[1] != "container:w2-app" {
			t.Fatalf("%s: ungated = %v", r.Command, k)
		}
	}

	// A lease keyed by its pod wins the pod's containers over it.
	b, _, _ := book(t)
	b.Observe(podmanSnap())
	b.Check(policy.Request{Worktree: "w1", Kind: "pod", Op: "start", Command: "podman pod start"}, podmanSnap(), cfg)
	b.Check(podStart("w1", "web"), podmanSnap(), cfg)
	b.Observe(withPodContainerIn(podmanSnap(), "web-app", "web", "w1", gib/4))
	if ls := b.List(); len(ls) != 2 || ls[0].Bytes != gib || ls[1].Bytes != gib*3/4 {
		t.Fatalf("leases = %+v", ls)
	}
}

// A start of a machine whose VM runs starts nothing (podman says it
// already runs): allowed whatever the pressure, and no lease.
func TestAStartOfARunningMachineTakesNoLease(t *testing.T) {
	b, _, _ := book(t)
	s := withMachine(podmanSnap(), "dev", gib)
	b.Observe(s)
	if d := b.Check(machineStart("w1", "dev", 64*gib), s, cfg); !d.Allow || d.LeaseID != "" {
		t.Fatalf("check = %+v", d)
	}
}

// A pod start of pods that all run starts nothing (Podman's no-op):
// allowed whatever the pressure, with no lease and no warning, as a docker
// start of a running container is; also when another worktree's lease
// holds them. A start of a pod with none running still takes a lease
// (#230, plan amendment 3).
func TestAPodStartOfRunningPodsTakesNoLease(t *testing.T) {
	b, c, log := book(t)
	b.Observe(podmanSnap())
	b.Check(podStart("w2", "web"), podmanSnap(), cfg)
	s := withPodContainer(podmanSnap(), "web-app", "web", "a1b2c3", gib/4)
	s = withPodContainer(s, "db-app", "db", "0f0f0f", gib/4)
	b.Observe(s)
	for _, r := range []policy.Request{podStart("w1", "web"), podStart("w1", "db"), podStart("w1", "web", "db")} {
		r.CostBytes = 64 * gib
		if d := b.Check(r, s, cfg); !d.Allow || d.LeaseID != "" {
			t.Fatalf("%v: check = %+v", r.Pods, d)
		}
	}
	c.t = t0.Add(3 * time.Minute)
	b.Observe(s)
	if strings.Contains(log.String(), "never appeared") {
		t.Fatalf("log:\n%s", log)
	}
	if d := b.Check(podStart("w1", "web", "cache"), s, cfg); !d.Allow || d.LeaseID == "" {
		t.Fatalf("a start of a stopped pod took no lease: %+v", d)
	}
}
