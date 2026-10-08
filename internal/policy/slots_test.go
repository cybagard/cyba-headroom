package policy_test

import (
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// slotSnap has two macOS VMs running: ci-mac for worktree "busy" (agent
// working) and rel-mac for "idle" (agent done an hour ago), plus a linux VM.
func slotSnap() *protocol.Snapshot {
	s := snap()
	s.Tart = &protocol.Tart{Installed: true, MacOSRunning: 2, VMs: []protocol.TartVM{
		{Name: "ci-mac", OS: "darwin", MemoryBytes: 8 * gib},
		{Name: "rel-mac", OS: "darwin", MemoryBytes: 8 * gib},
		{Name: "lx", OS: "linux", MemoryBytes: 2 * gib},
	}}
	s.Attribution.Worktrees[0].TartVMs = []protocol.AttributedVM{{Name: "ci-mac", MemoryBytes: 8 * gib}}
	s.Attribution.Worktrees[1].TartVMs = []protocol.AttributedVM{{Name: "rel-mac", MemoryBytes: 8 * gib}}
	return s
}

var slotCfg = func() policy.Config { c := cfg; c.MaxMacOSVMs = 2; return c }()

func tartReq(wt string, macOS bool) policy.Request {
	return policy.Request{Worktree: wt, Kind: "tart", Command: "tart run new-mac", CostBytes: 4 * gib, MacOS: macOS}
}

func TestMacOSSlotsAreCountedFirst(t *testing.T) {
	d := policy.Decide(tartReq("busy", true), slotSnap(), slotCfg)
	if d.Allow || !d.Retry || d.Reasons[0].Code != policy.VMSlots {
		t.Fatalf("third macOS VM: %+v", d)
	}
	// The idle worktree's holder is named first, with how long it has idled.
	msg := d.Reasons[0].Text
	if !strings.Contains(msg, "2 of 2") || strings.Index(msg, "rel-mac") > strings.Index(msg, "ci-mac") ||
		!strings.Contains(msg, `rel-mac (worktree "Release prep": agents done for 1h0m)`) ||
		!strings.Contains(msg, `ci-mac (worktree "Fix login": an agent working)`) || !strings.Contains(msg, "tart stop rel-mac") {
		t.Fatalf("message %q", msg)
	}
	// A linux VM takes no slot.
	if d := policy.Decide(tartReq("busy", false), slotSnap(), slotCfg); !d.Allow {
		t.Fatalf("linux VM: %+v", d)
	}
}

func TestMacOSSlotsCountVMsStillStarting(t *testing.T) {
	s := slotSnap()
	s.Tart.MacOSRunning, s.Tart.VMs = 1, s.Tart.VMs[1:]
	r := tartReq("busy", true)
	if d := policy.Decide(r, s, slotCfg); !d.Allow {
		t.Fatalf("one slot free: %+v", d)
	}
	r.PendingMacOS = 1
	if d := policy.Decide(r, s, slotCfg); d.Allow || d.Reasons[0].Code != policy.VMSlots || !strings.Contains(d.Reasons[0].Text, "1 starting") {
		t.Fatalf("the free slot is promised: %+v", d)
	}
}

func TestManualMacOSVMsAreNamedBetweenIdleAndBusy(t *testing.T) {
	s := slotSnap()
	s.Tart.VMs = append(s.Tart.VMs, protocol.TartVM{Name: "hand-mac", OS: "darwin"})
	s.Tart.MacOSRunning = 3
	c := slotCfg
	c.MaxMacOSVMs = 3
	msg := policy.Decide(tartReq("busy", true), s, c).Reasons[0].Text
	if i, j, k := strings.Index(msg, "rel-mac"), strings.Index(msg, "hand-mac (manual)"), strings.Index(msg, "ci-mac"); i >= j || j >= k {
		t.Fatalf("order in %q", msg)
	}
}

func TestNoMacOSSlots(t *testing.T) {
	s := slotSnap()
	s.Tart.MacOSRunning, s.Tart.VMs = 0, nil
	c := slotCfg
	c.MaxMacOSVMs = 0
	if d := policy.Decide(tartReq("busy", true), s, c); d.Allow || d.Reasons[0].Code != policy.VMSlots {
		t.Fatalf("max 0: %+v", d)
	}
}

// Without a fresh Tart reading the count is unknown: decide on memory (R7).
func TestUnknownSlotCountDoesNotBlock(t *testing.T) {
	s := slotSnap()
	s.Sources = map[string]protocol.SourceStatus{"tart": {Stale: true}}
	if d := policy.Decide(tartReq("busy", true), s, slotCfg); !d.Allow || !strings.Contains(d.Message, "tart") {
		t.Fatalf("stale tart: %+v", d)
	}
	s = slotSnap()
	s.Tart = nil
	if d := policy.Decide(tartReq("busy", true), s, slotCfg); !d.Allow {
		t.Fatalf("no tart reading: %+v", d)
	}
}

func TestIdleTimeIsRounded(t *testing.T) {
	s := slotSnap()
	s.Orca.Worktrees[1].Agents[0].StateSince = now.Add(-14*time.Minute - 25*time.Second)
	if msg := policy.Decide(tartReq("busy", true), s, slotCfg).Reasons[0].Text; !strings.Contains(msg, "done for 14m") {
		t.Fatalf("message %q", msg)
	}
}
