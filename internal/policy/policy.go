// Package policy decides whether a resource-creating request may go ahead
// (R8, #24): a pressure guard, the idle-holder rule, a per-worktree cap and
// minimum headroom, checked against the daemon's current snapshot. It is
// pure: the daemon supplies the snapshot, the caller describes the request.
package policy

import (
	"fmt"
	"strings"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// Reason codes.
const (
	Manual      = "manual"
	Unknown     = "unknown"
	Pressure    = "pressure"
	IdleHolder  = "idle_holder"
	WorktreeCap = "worktree_cap"
	Headroom    = "headroom"
)

// Config is the policy's settings, in bytes.
type Config struct {
	MinHeadroomBytes    uint64
	PerWorktreeCapBytes uint64 // 0 = no cap
	// PressureGuard is the pressure level at which requests are denied:
	// off, warn or critical.
	PressureGuard string
	// GuardRising also denies at warn while the trend is rising.
	GuardRising bool
	// DefaultContainerBytes is the cost of a container or compose request
	// whose caller gives none.
	DefaultContainerBytes uint64
}

// Request describes one resource-creating call.
type Request struct {
	// Worktree is the Orca worktree ID; empty for a manual call.
	Worktree string
	// Kind is container, compose or tart.
	Kind string
	// Command is what was run, for messages.
	Command string
	// CostBytes is the caller's estimate; 0 means the default for Kind.
	CostBytes uint64
	// LeasedBytes is memory promised to earlier allows that has not shown
	// up yet (#25).
	LeasedBytes uint64
}

// The decision types are the wire format's, so the daemon returns them as is.
type (
	// Reason is one rule's verdict on a request.
	Reason = protocol.Reason
	// Held is one of the worktree's own containers or VMs.
	Held = protocol.Held
	// Decision is the policy's answer.
	Decision = protocol.Decision
)

// Decide decides r against s.
func Decide(r Request, s *protocol.Snapshot, c Config) Decision {
	d := Decision{CostBytes: r.CostBytes}
	if d.CostBytes == 0 && r.Kind != "tart" {
		d.CostBytes = c.DefaultContainerBytes
	}
	if s.Budget != nil {
		d.HeadroomBytes = s.Budget.HeadroomBytes
	}
	if r.Worktree == "" {
		d.Allow = true
		d.Reasons = []Reason{{Code: Manual, Text: "no Orca worktree: a manual call, not gated"}}
		d.Message = "headroom: allowed (manual call)"
		return d
	}
	if d.HeadroomBytes == nil {
		d.Allow = true
		d.Reasons = []Reason{{Code: Unknown, Text: "the budget is unknown (no host reading yet)"}}
		d.Message = "headroom: allowed: the budget is unknown, so headroom cannot gate this call"
		return d
	}
	use, held := worktreeUse(s, r.Worktree)
	d.Holding = held
	if reason, ok := pressure(s.Host, c); ok {
		d.Reasons = append(d.Reasons, reason)
	}
	if len(held) > 0 && !working(s, r.Worktree) {
		d.Reasons = append(d.Reasons, Reason{Code: IdleHolder,
			Text: "this worktree holds containers or VMs while none of its agents is working; reuse or stop them first"})
	}
	if c.PerWorktreeCapBytes > 0 && use+d.CostBytes > c.PerWorktreeCapBytes {
		d.Reasons = append(d.Reasons, Reason{Code: WorktreeCap,
			Text: fmt.Sprintf("this worktree would use %s GB, over its cap of %s GB", gb(int64(use+d.CostBytes)), gb(int64(c.PerWorktreeCapBytes)))})
	}
	left := *d.HeadroomBytes - int64(r.LeasedBytes) - int64(d.CostBytes)
	if left < int64(c.MinHeadroomBytes) {
		text := fmt.Sprintf("only %s GB headroom", gb(*d.HeadroomBytes))
		if r.LeasedBytes > 0 {
			text += fmt.Sprintf(" (%s GB of it promised to calls still starting)", gb(int64(r.LeasedBytes)))
		}
		if c.MinHeadroomBytes > 0 {
			text += fmt.Sprintf(", and %s GB must stay free", gb(int64(c.MinHeadroomBytes)))
		}
		d.Reasons = append(d.Reasons, Reason{Code: Headroom, Retry: true, Text: text})
	}
	d.Allow = len(d.Reasons) == 0
	d.Retry = !d.Allow
	for _, reason := range d.Reasons {
		d.Retry = d.Retry && reason.Retry
	}
	d.Message = message(r, s, d)
	return d
}

// guardLevels orders pressure levels for the guard.
var guardLevels = map[string]int{"normal": 1, "warn": 2, "critical": 3}

// pressure is the pressure guard: it trips at the configured level, and at
// warn with a rising trend. The trend alone never trips it: it lags, and
// keeps reading rising after load is released (#18).
func pressure(h *protocol.Host, c Config) (Reason, bool) {
	guard, ok := guardLevels[c.PressureGuard]
	if h == nil || !ok {
		return Reason{}, false
	}
	level := guardLevels[h.Pressure]
	rising := c.GuardRising && h.Pressure == "warn" && h.Trend.Direction == "rising"
	if level < guard && !rising {
		return Reason{}, false
	}
	text := fmt.Sprintf("memory pressure is %s (%d%% free)", h.Pressure, h.FreePercent)
	if rising {
		text += " and rising"
	}
	return Reason{Code: Pressure, Retry: true,
		Text: text + ". This is not about GB: the Mac is short of memory now. Wait and retry, or free memory"}, true
}

// worktreeUse is the worktree's attributed use (container memory and Tart
// VMs' configured memory) and the items behind it.
func worktreeUse(s *protocol.Snapshot, id string) (uint64, []Held) {
	if s.Attribution == nil {
		return 0, nil
	}
	for _, w := range s.Attribution.Worktrees {
		if w.ID != id {
			continue
		}
		var held []Held
		for _, c := range w.Containers {
			held = append(held, Held{Name: c.Name, Bytes: c.MemoryBytes})
		}
		for _, vm := range w.TartVMs {
			held = append(held, Held{Name: vm.Name, Bytes: vm.MemoryBytes})
		}
		return w.ContainerMemoryBytes + w.TartMemoryBytes, held
	}
	return 0, nil
}

// working reports whether any of the worktree's agents is working.
func working(s *protocol.Snapshot, id string) bool {
	if s.Orca == nil {
		return true // agents unknown: do not accuse anyone of idling
	}
	for _, w := range s.Orca.Worktrees {
		if w.ID == id {
			for _, a := range w.Agents {
				if a.State == "working" {
					return true
				}
			}
			return false
		}
	}
	return true
}

// message explains d for an agent.
func message(r Request, s *protocol.Snapshot, d Decision) string {
	var b strings.Builder
	if d.Allow {
		fmt.Fprintf(&b, "headroom: allowed `%s` (≈ %s GB)", r.Command, gb(int64(d.CostBytes)))
	} else {
		fmt.Fprintf(&b, "headroom: not starting `%s` (≈ %s GB)", r.Command, gb(int64(d.CostBytes)))
		if name := worktreeName(s, r.Worktree); name != "" {
			fmt.Fprintf(&b, " for worktree %q", name)
		}
		fmt.Fprintf(&b, ": %s", d.Reasons[0].Text)
	}
	if !d.Allow && len(d.Holding) > 0 {
		var items []string
		for _, h := range d.Holding {
			items = append(items, fmt.Sprintf("%s (%s GB)", h.Name, gb(int64(h.Bytes))))
		}
		fmt.Fprintf(&b, ". Your worktree holds: %s", strings.Join(items, ", "))
	}
	if s.Budget != nil && len(s.Budget.Unknown) > 0 {
		fmt.Fprintf(&b, " (headroom may be lower than shown: no reading yet from %s)", strings.Join(s.Budget.Unknown, ", "))
	}
	return b.String()
}

// worktreeName is the worktree's display name in Orca, if known.
func worktreeName(s *protocol.Snapshot, id string) string {
	if s.Orca != nil {
		for _, w := range s.Orca.Worktrees {
			if w.ID == id {
				return w.Name
			}
		}
	}
	return ""
}

func gb(b int64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<30)) }
