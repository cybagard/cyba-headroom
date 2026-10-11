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
// by the pod's name, or by its ID as given. A container of another pod,
// or one of the same name on another engine, binds nothing (#230).
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

	b, _, _ = book(t)
	b.Observe(podmanSnap())
	b.Check(podStart("w1", "a1b2"), podmanSnap(), cfg) // the pod's ID, as podman takes a prefix
	b.Observe(withPodContainer(podmanSnap(), "web-app", "web", "a1b2c3", gib/4))
	if ls := b.List(); len(ls) != 1 || ls[0].Bytes != gib*3/4 || len(ungatedKeys(b)) != 0 {
		t.Fatalf("by ID: leases %+v, ungated %v", ls, ungatedKeys(b))
	}
}

// A pod lease with no pod to name (pod start --all, a kube file on stdin)
// has no key: it binds nothing, and holds its cost to its timeout.
func TestAPodLeaseWithNoPodBindsNothing(t *testing.T) {
	b, _, _ := book(t)
	b.Observe(podmanSnap())
	b.Check(policy.Request{Worktree: "w1", Kind: "pod", Op: "play", Command: "podman kube play"}, podmanSnap(), cfg)
	b.Observe(withPodContainer(podmanSnap(), "web-app", "web", "a1b2c3", gib/4))
	if ls := b.List(); len(ls) != 1 || ls[0].Bytes != gib {
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

// A pod start of a pod that runs holds its containers: they are not what
// the lease waits for, and it ends at its timeout without warning that
// nothing appeared.
func TestAPodStartOfARunningPodHoldsItsContainers(t *testing.T) {
	b, c, log := book(t)
	s := withPodContainer(podmanSnap(), "web-app", "web", "a1b2c3", 3*gib)
	b.Observe(s)
	b.Check(podStart("w1", "web"), s, cfg)
	b.Observe(s)
	if ls := b.List(); len(ls) != 1 || ls[0].Bytes != gib {
		t.Fatalf("leases = %+v", ls)
	}
	c.t = t0.Add(2 * time.Minute)
	b.Observe(s)
	if len(b.List()) != 0 || strings.Contains(log.String(), "never appeared") {
		t.Fatalf("leases %+v, log:\n%s", b.List(), log)
	}
}
