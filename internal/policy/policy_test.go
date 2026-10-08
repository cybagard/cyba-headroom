package policy_test

import (
	"strings"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

const gib = uint64(1 << 30)

func i64(v int64) *int64 { return &v }

// snap is a 64 GiB Mac at normal pressure with 20 GiB headroom. Worktree
// "busy" has a working agent and a 2 GiB container; "idle" has a done agent
// and holds a 3 GiB container and an 8 GiB Tart VM.
func snap() *protocol.Snapshot {
	return &protocol.Snapshot{
		Host:   &protocol.Host{TotalBytes: 64 * gib, Pressure: "normal", FreePercent: 60, Trend: protocol.Trend{Direction: "steady", Worst: "normal"}},
		Budget: &protocol.Budget{TotalBytes: 64 * gib, ReservedBytes: 44 * gib, HeadroomBytes: i64(int64(20 * gib))},
		Orca: &protocol.Orca{Installed: true, Running: true, Worktrees: []protocol.Worktree{
			{ID: "busy", Name: "Fix login", Agents: []protocol.Agent{{Type: "claude", State: "working"}}},
			{ID: "idle", Name: "Release prep", Agents: []protocol.Agent{{Type: "claude", State: "done"}}},
		}},
		Attribution: &protocol.Attribution{Worktrees: []protocol.WorktreeUsage{
			{ID: "busy", Name: "Fix login", Usage: protocol.Usage{
				Containers:           []protocol.AttributedContainer{{Name: "web", MemoryBytes: 2 * gib}},
				ContainerMemoryBytes: 2 * gib}},
			{ID: "idle", Name: "Release prep", Usage: protocol.Usage{
				Containers:           []protocol.AttributedContainer{{Name: "db", MemoryBytes: 3 * gib}},
				ContainerMemoryBytes: 3 * gib,
				TartVMs:              []protocol.AttributedVM{{Name: "ci-mac", MemoryBytes: 8 * gib}},
				TartMemoryBytes:      8 * gib}},
		}},
	}
}

var cfg = policy.Config{PressureGuard: "critical", GuardRising: true, DefaultContainerBytes: gib}

func req(wt string, cost uint64) policy.Request {
	return policy.Request{Worktree: wt, Kind: "container", Command: "docker run postgres:17", CostBytes: cost}
}

func TestManualRequestsAreAllowed(t *testing.T) {
	s := snap()
	s.Host.Pressure = "critical" // even now: a human decides for themselves
	d := policy.Decide(req("", 50*gib), s, cfg)
	if !d.Allow || len(d.Reasons) != 1 || d.Reasons[0].Code != policy.Manual {
		t.Fatalf("decision = %+v", d)
	}
}

func TestAllowWhenItFits(t *testing.T) {
	d := policy.Decide(req("busy", 2*gib), snap(), cfg)
	if !d.Allow || len(d.Reasons) != 0 || d.CostBytes != 2*gib || d.HeadroomBytes == nil || *d.HeadroomBytes != int64(20*gib) {
		t.Fatalf("decision = %+v", d)
	}
}

func TestUnknownBudgetFailsOpen(t *testing.T) {
	s := snap()
	s.Budget.HeadroomBytes = nil
	d := policy.Decide(req("busy", 2*gib), s, cfg)
	if !d.Allow || len(d.Reasons) != 1 || d.Reasons[0].Code != policy.Unknown {
		t.Fatalf("decision = %+v", d)
	}
	s.Budget = nil
	if d := policy.Decide(req("busy", 2*gib), s, cfg); !d.Allow {
		t.Fatalf("no budget: %+v", d)
	}
}

func TestPartlyKnownBudgetIsNoted(t *testing.T) {
	s := snap()
	s.Budget.Unknown = []string{"lmstudio"}
	d := policy.Decide(req("busy", 2*gib), s, cfg)
	if !d.Allow || !strings.Contains(d.Message, "lmstudio") {
		t.Fatalf("decision = %+v", d)
	}
}

func code(d policy.Decision) []string {
	var out []string
	for _, r := range d.Reasons {
		out = append(out, r.Code)
	}
	return out
}

func TestPressureGuard(t *testing.T) {
	cases := []struct {
		name, level, direction, guard string
		rising                        bool
		deny                          bool
	}{
		{"critical", "critical", "steady", "critical", true, true},
		{"warn and rising", "warn", "rising", "critical", true, true},
		{"warn and rising, rising guard off", "warn", "rising", "critical", false, false},
		{"warn and steady", "warn", "steady", "critical", true, false},
		{"normal and rising (the trend lags)", "normal", "rising", "critical", true, false},
		{"guard at warn", "warn", "steady", "warn", true, true},
		{"guard off", "critical", "rising", "off", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := snap()
			s.Host.Pressure, s.Host.FreePercent, s.Host.Trend.Direction = tc.level, 12, tc.direction
			c := cfg
			c.PressureGuard, c.GuardRising = tc.guard, tc.rising
			d := policy.Decide(req("busy", gib), s, c)
			if denied := !d.Allow; denied != tc.deny {
				t.Fatalf("denied = %v, want %v: %+v", denied, tc.deny, d)
			}
			if tc.deny && (d.Reasons[0].Code != policy.Pressure || !d.Retry ||
				!strings.Contains(d.Message, "memory pressure is "+tc.level+" (12% free)") || !strings.Contains(d.Message, "not about GB")) {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
}

func TestPressureGuardsWhateverHeadroomSays(t *testing.T) {
	s := snap()
	s.Host.Pressure = "critical"
	s.Budget.HeadroomBytes = i64(int64(60 * gib))
	if d := policy.Decide(req("busy", gib), s, cfg); d.Allow {
		t.Fatalf("allowed at critical pressure: %+v", d)
	}
}

func TestIdleHolderMustActFirst(t *testing.T) {
	d := policy.Decide(req("idle", gib), snap(), cfg)
	if d.Allow || d.Retry || d.Reasons[0].Code != policy.IdleHolder {
		t.Fatalf("decision = %+v", d)
	}
	if len(d.Holding) != 2 || !strings.Contains(d.Message, "db (3.0 GB)") || !strings.Contains(d.Message, "ci-mac (8.0 GB)") ||
		!strings.Contains(d.Message, `for worktree "Release prep"`) {
		t.Fatalf("holding = %+v, message %q", d.Holding, d.Message)
	}
	// Holding nothing, a done agent may start something.
	s := snap()
	s.Attribution.Worktrees[1].Usage = protocol.Usage{}
	if d := policy.Decide(req("idle", gib), s, cfg); !d.Allow {
		t.Fatalf("idle worktree holding nothing: %+v", d)
	}
	// No agent at all counts as idle.
	s = snap()
	s.Orca.Worktrees[1].Agents = nil
	if d := policy.Decide(req("idle", gib), s, cfg); d.Allow {
		t.Fatalf("no agent, holding resources: %+v", d)
	}
}

func TestPerWorktreeCap(t *testing.T) {
	c := cfg
	c.PerWorktreeCapBytes = 4 * gib
	// busy holds 2 GiB: 1 more fits, 3 more do not.
	if d := policy.Decide(req("busy", gib), snap(), c); !d.Allow {
		t.Fatalf("under the cap: %+v", d)
	}
	d := policy.Decide(req("busy", 3*gib), snap(), c)
	if d.Allow || d.Retry || d.Reasons[0].Code != policy.WorktreeCap || !strings.Contains(d.Message, "web (2.0 GB)") {
		t.Fatalf("over the cap: %+v", d)
	}
	c.PerWorktreeCapBytes = 0
	if d := policy.Decide(req("busy", 30*gib), snap(), c); code(d)[0] == policy.WorktreeCap {
		t.Fatalf("cap 0 enforced: %+v", d)
	}
}

func TestHeadroom(t *testing.T) {
	c := cfg
	c.MinHeadroomBytes = 4 * gib
	if d := policy.Decide(req("busy", 16*gib), snap(), c); !d.Allow {
		t.Fatalf("20 − 16 = 4, the floor: %+v", d)
	}
	d := policy.Decide(req("busy", 17*gib), snap(), c)
	if d.Allow || !d.Retry || d.Reasons[0].Code != policy.Headroom {
		t.Fatalf("below the floor: %+v", d)
	}
	// In-flight allows count.
	r := req("busy", 10*gib)
	r.LeasedBytes = 8 * gib
	if d := policy.Decide(r, snap(), c); d.Allow {
		t.Fatalf("leases ignored: %+v", d)
	}
	// With no floor, headroom still cannot go below zero.
	c.MinHeadroomBytes = 0
	if d := policy.Decide(req("busy", 21*gib), snap(), c); d.Allow {
		t.Fatalf("went below zero: %+v", d)
	}
	// No cost given: the default for containers.
	r = req("busy", 0)
	if d := policy.Decide(r, snap(), c); d.CostBytes != gib {
		t.Fatalf("default cost = %d", d.CostBytes)
	}
}

func TestEveryFailingRuleIsReported(t *testing.T) {
	s := snap()
	s.Host.Pressure = "critical"
	c := cfg
	c.PerWorktreeCapBytes = gib
	d := policy.Decide(req("idle", 30*gib), s, c)
	want := []string{policy.Pressure, policy.IdleHolder, policy.WorktreeCap, policy.Headroom}
	if strings.Join(code(d), ",") != strings.Join(want, ",") {
		t.Fatalf("reasons = %v, want %v", code(d), want)
	}
	// Waiting alone cannot fix the idle holder or the cap.
	if d.Retry {
		t.Fatal("retry with reasons the agent must act on")
	}
	if !strings.Contains(d.Message, "memory pressure is critical") {
		t.Fatalf("the first reason must lead: %q", d.Message)
	}
}
