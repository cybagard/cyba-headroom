// Package policy decides whether a resource-creating request may go ahead
// (R8, #24): a pressure guard, the idle-holder rule, a per-worktree cap and
// minimum headroom, checked against the daemon's current snapshot. It is
// pure: the daemon supplies the snapshot, the caller describes the request.
package policy

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/units"
)

// Reason codes.
const (
	Manual      = "manual"
	Unknown     = "unknown"
	Pressure    = "pressure"
	IdleHolder  = "idle_holder"
	WorktreeCap = "worktree_cap"
	Headroom    = "headroom"
	VMSlots     = "vm_slots"
	// StaleSnapshot: the collector has not refreshed the snapshot in
	// MaxSnapshotAge, so nothing current is known (R7, #30).
	StaleSnapshot = "stale_snapshot"
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
	// DefaultTartBytes is the cost of a Tart VM whose caller gives none.
	DefaultTartBytes uint64
	// IdleGrace is how long an agent must have been out of the working
	// state before its worktree counts as an idle holder.
	IdleGrace time.Duration
	// Now is the clock for IdleGrace.
	Now func() time.Time
	// MaxMacOSVMs is how many macOS VMs may run at once (R6); Apple's
	// licence allows two.
	MaxMacOSVMs int
	// MaxSnapshotAge is how old the snapshot may be before decisions treat
	// it as unknown: the collector has stalled. 0 means no limit.
	MaxSnapshotAge time.Duration
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
	// WorktreeLeasedBytes is the part of LeasedBytes promised to this
	// worktree's own calls: it counts toward its cap.
	WorktreeLeasedBytes uint64
	// MacOS is set for a tart run of a macOS VM, which takes a slot (R6).
	MacOS bool
	// VMUnknown means the VM's config was not found, so MacOS is assumed.
	VMUnknown bool
	// PendingMacOS counts macOS VMs allowed but not yet running (#29).
	PendingMacOS int
	// PID is the calling process for a tart run, which becomes tart: the
	// lease ends if it exits before its VM appears (#29).
	PID int
	// Target and Name identify what the call starts (the image, container,
	// compose project or VM, and a --name), for its lease to bind (#33).
	Target, Name string
	// Labelled means the container will carry the lease's ID (#33).
	Labelled bool
	// Op is the call's subcommand (run, up, start, ...).
	Op string
	// ContainerID is a start's container as Docker resolved it, and
	// TakesOver the lease of the run or create that made it (its label),
	// which this call's lease replaces (#33).
	ContainerID, TakesOver string
	// Running is Docker's word that a start's container already runs, and
	// MultiTarget that the start names others Docker did not resolve.
	Running, MultiTarget bool
	// Others are the other containers a start names, as Docker resolved
	// them (docker start a b c).
	Others []Start
}

// maxBytes bounds request sizes so the headroom arithmetic cannot wrap: far
// above any Mac's memory.
const maxBytes = uint64(1) << 42

// The decision types are the wire format's, so the daemon returns them as is.
type (
	// Reason is one rule's verdict on a request.
	Reason = protocol.Reason
	// Held is one of the worktree's own containers or VMs.
	Held = protocol.Held
	// Decision is the policy's answer.
	Decision = protocol.Decision
)

// Start is one container a start names, as Docker resolved it: its ID,
// the lease of the run or create that made it (its label), and whether it
// already runs.
type Start struct {
	ID, TakesOver string
	Running       bool
}

// Decide decides r against s.
func Decide(r Request, s *protocol.Snapshot, c Config) Decision {
	if s == nil {
		s = &protocol.Snapshot{} // before the daemon's first tick
	}
	r.CostBytes = min(r.CostBytes, maxBytes)
	r.LeasedBytes = min(r.LeasedBytes, maxBytes)
	r.WorktreeLeasedBytes = min(r.WorktreeLeasedBytes, maxBytes)
	d := Decision{CostBytes: r.CostBytes}
	if d.CostBytes == 0 {
		d.CostBytes = c.DefaultContainerBytes
		if r.Kind == "tart" {
			d.CostBytes = c.DefaultTartBytes
		}
	}
	// A snapshot the collector stopped refreshing says nothing current:
	// decide by the same rules on an empty one, so only what does not come
	// from readings (the config, the leases) can deny (R7, #30).
	age, stale := snapshotAge(s, c)
	if stale {
		s = &protocol.Snapshot{}
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
	use, held := worktreeUse(s, r.Worktree)
	d.Holding = held
	// Count first, memory second (R6).
	if reason, ok := slots(r, s, c); ok {
		d.Reasons = append(d.Reasons, reason)
	}
	// The pressure guard and the idle-holder rule need no budget.
	if reason, ok := pressure(s.Host, c); ok {
		d.Reasons = append(d.Reasons, reason)
	}
	if len(held) > 0 && idle(s, r.Worktree, c) {
		d.Reasons = append(d.Reasons, Reason{Code: IdleHolder,
			Text: "this worktree holds containers or VMs while none of its agents is working; reuse or stop them first"})
	}
	// The cap counts what is known to be the worktree's: its attributed
	// use (0 if unknown) and its leases.
	if would := use + r.WorktreeLeasedBytes + d.CostBytes; c.PerWorktreeCapBytes > 0 && would > c.PerWorktreeCapBytes {
		text := fmt.Sprintf("this worktree would use %s GB, over its cap of %s GB", units.GB(would), units.GB(c.PerWorktreeCapBytes))
		if r.WorktreeLeasedBytes > 0 {
			text += fmt.Sprintf(" (%s GB of it for its calls still starting)", units.GB(r.WorktreeLeasedBytes))
		}
		d.Reasons = append(d.Reasons, Reason{Code: WorktreeCap, Text: text})
	}
	if d.HeadroomBytes != nil {
		left := *d.HeadroomBytes - int64(r.LeasedBytes) - int64(d.CostBytes)
		if left < int64(c.MinHeadroomBytes) {
			text := fmt.Sprintf("only %s GB headroom", units.SignedGB(*d.HeadroomBytes))
			if r.LeasedBytes > 0 {
				text += fmt.Sprintf(" (%s GB of it promised to calls still starting)", units.GB(r.LeasedBytes))
			}
			if c.MinHeadroomBytes > 0 {
				text += fmt.Sprintf(", and %s GB must stay free", units.GB(c.MinHeadroomBytes))
			}
			d.Reasons = append(d.Reasons, Reason{Code: Headroom, Retry: true, Text: text})
		}
	}
	d.Allow = len(d.Reasons) == 0
	switch {
	case d.Allow && stale:
		d.Reasons = []Reason{{Code: StaleSnapshot, Text: staleText(age) + ", so headroom cannot gate this call"}}
	case d.Allow && d.HeadroomBytes == nil:
		// Fail open (R7): with no budget, headroom cannot gate the call.
		d.Reasons = []Reason{{Code: Unknown, Text: "the budget is unknown (no host reading yet), so headroom cannot gate this call"}}
	}
	d.Retry = !d.Allow
	for _, reason := range d.Reasons {
		d.Retry = d.Retry && reason.Retry
	}
	d.Message = message(r, s, d)
	if !d.Allow && stale {
		d.Message += "; " + staleText(age)
	}
	return d
}

// snapshotAge is how old s's readings are, and whether that is too old to
// decide on: the snapshot's own age, or its host reading's when the host
// source keeps failing (each tick reapplies its last reading).
func snapshotAge(s *protocol.Snapshot, c Config) (time.Duration, bool) {
	if c.MaxSnapshotAge <= 0 || c.Now == nil || s.CollectedAt.IsZero() {
		return 0, false
	}
	// Wall clocks: a monotonic reading stops while the Mac sleeps, and a
	// snapshot from before a sleep is old.
	now := c.Now().Round(0)
	age := now.Sub(s.CollectedAt.Round(0))
	if h := s.Sources["host"]; h.Stale && !h.At.IsZero() {
		age = max(age, now.Sub(h.At.Round(0)))
	}
	return age, age > c.MaxSnapshotAge
}

// staleText says how old the readings are.
func staleText(age time.Duration) string {
	return fmt.Sprintf("the daemon's readings are %s old (its collector has stalled)", shortDuration(age))
}

// slots is the macOS VM slot rule: running plus starting macOS VMs must
// stay below the slot count for one more to start. Without a fresh Tart reading the count is unknown, and
// the call is decided on memory alone (R7).
func slots(r Request, s *protocol.Snapshot, c Config) (Reason, bool) {
	if !r.MacOS {
		return Reason{}, false
	}
	if c.MaxMacOSVMs <= 0 {
		return Reason{Code: VMSlots, Text: "this Mac allows no macOS VMs (budget.max_macos_vms = 0)"}, true
	}
	// Running VMs are unknown without a fresh Tart reading: count only the
	// ones still starting, which the leases know.
	running := 0
	if s.Tart != nil && !s.Sources["tart"].Stale {
		running = s.Tart.MacOSRunning
	}
	inUse := running + r.PendingMacOS
	if inUse < c.MaxMacOSVMs {
		return Reason{}, false
	}
	text := fmt.Sprintf("the macOS VM slots are full (%d of %d in use", inUse, c.MaxMacOSVMs)
	if r.PendingMacOS > 0 {
		text += fmt.Sprintf(", %d starting", r.PendingMacOS)
	}
	text += ")"
	if r.VMUnknown {
		text = "its VM's config was not found, so it counts as macOS, and " + text
	}
	var holders []string
	var idle string
	if s.Tart != nil && !s.Sources["tart"].Stale { // a stale reading's VMs may be gone
		holders, idle = slotHolders(s, c)
	}
	if len(holders) > 0 {
		text += ": " + strings.Join(holders, ", ")
	}
	if idle != "" {
		text += fmt.Sprintf(". Nobody is using %s: stop it with `tart stop %s`, or wait for a slot", idle, idle)
	}
	return Reason{Code: VMSlots, Retry: true, Text: text}, true
}

// slotHolders describes the running macOS VMs, those whose worktree's
// agents are idle first (longest idle first), then manual ones, then those
// in use; stop is the first one safe to suggest stopping.
func slotHolders(s *protocol.Snapshot, c Config) (holders []string, stop string) {
	owner := map[string]protocol.WorktreeUsage{}
	if s.Attribution != nil {
		for _, w := range s.Attribution.Worktrees {
			for _, vm := range w.TartVMs {
				owner[vm.Name] = w
			}
		}
	}
	type holder struct {
		text  string
		rank  int           // 0 idle, 1 manual, 2 in use
		idle  time.Duration // for idle ones
		name  string
		offer bool // safe to suggest stopping: idle past the grace, plain name
	}
	var hs []holder
	for _, vm := range s.Tart.VMs {
		if vm.OS != "darwin" && vm.OS != "" { // "": unknown, counted as macOS
			continue
		}
		shown := vmName(vm.Name)
		w, ok := owner[vm.Name]
		if !ok {
			hs = append(hs, holder{text: shown + " (manual)", rank: 1, name: vm.Name})
			continue
		}
		state, since, working := agentState(s, w.ID, c)
		h := holder{rank: 2, name: vm.Name, text: fmt.Sprintf("%s (worktree %q: %s)", shown, w.Name, state)}
		if !working {
			h.rank, h.idle = 0, since
			// Offered for stopping only when the idle-holder rule calls
			// it idle and how long is known, and only under a plain name
			// that cannot read as a flag.
			h.offer = idle(s, w.ID, c) && (since > 0 || state == "no agents") &&
				shown == vm.Name && !strings.HasPrefix(vm.Name, "-")
		}
		hs = append(hs, h)
	}
	slices.SortStableFunc(hs, func(a, b holder) int {
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		return int(b.idle - a.idle)
	})
	for _, h := range hs {
		holders = append(holders, h.text)
		if h.offer && stop == "" {
			stop = h.name
		}
	}
	return holders, stop
}

// vmName shows a VM name: as is if plain (letters, digits, . _ -), else
// quoted, so a name with spaces, control or shell characters is neither
// misread nor offered as a command.
func vmName(n string) string {
	for _, r := range n {
		plain := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if !plain {
			return strconv.Quote(n)
		}
	}
	return n
}

// agentState describes a worktree's agents for a slot holder: whether one
// is working, else the latest one's state and how long it has been in it.
func agentState(s *protocol.Snapshot, id string, c Config) (text string, idle time.Duration, working bool) {
	if s.Orca == nil {
		return "agents unknown", 0, true // unknown is never called idle
	}
	for _, w := range s.Orca.Worktrees {
		if w.ID != id {
			continue
		}
		if len(w.Agents) == 0 {
			return "no agents", 0, false
		}
		latest := w.Agents[0]
		for _, a := range w.Agents {
			if a.State == "working" {
				return "an agent working", 0, true
			}
			if a.StateSince.After(latest.StateSince) {
				latest = a
			}
		}
		if c.Now == nil || latest.StateSince.IsZero() {
			return "agents " + latest.State, 0, false
		}
		idle = c.Now().Sub(latest.StateSince)
		return fmt.Sprintf("agents %s for %s", latest.State, shortDuration(idle)), idle, false
	}
	return "agents unknown", 0, true
}

// shortDuration is d to the minute: "14m", "1h0m".
func shortDuration(d time.Duration) string {
	if d < time.Minute {
		return "under a minute"
	}
	return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
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

// idle reports whether the worktree's agents have all been out of the
// working state for longer than the grace period. Orca's state lags: the
// agent making a call may only just have been marked done or waiting while
// its tool shell still runs. Unknown agents are never called idle.
func idle(s *protocol.Snapshot, id string, c Config) bool {
	if s.Orca == nil {
		return false
	}
	for _, w := range s.Orca.Worktrees {
		if w.ID != id {
			continue
		}
		for _, a := range w.Agents {
			if a.State == "working" || (c.Now != nil && c.Now().Sub(a.StateSince) < c.IdleGrace) {
				return false
			}
		}
		return true
	}
	return false
}

// message explains d for an agent.
func message(r Request, s *protocol.Snapshot, d Decision) string {
	var b strings.Builder
	if d.Allow {
		fmt.Fprintf(&b, "headroom: allowed `%s` (≈ %s GB)", r.Command, units.GB(d.CostBytes))
		if len(d.Reasons) > 0 {
			fmt.Fprintf(&b, ": %s", d.Reasons[0].Text)
		}
	} else {
		fmt.Fprintf(&b, "headroom: not starting `%s` (≈ %s GB)", r.Command, units.GB(d.CostBytes))
		if name := worktreeName(s, r.Worktree); name != "" {
			fmt.Fprintf(&b, " for worktree %q", name)
		}
		fmt.Fprintf(&b, ": %s", d.Reasons[0].Text)
	}
	if !d.Allow && len(d.Holding) > 0 {
		var items []string
		for _, h := range d.Holding {
			items = append(items, fmt.Sprintf("%s (%s GB)", h.Name, units.GB(h.Bytes)))
		}
		fmt.Fprintf(&b, ". Your worktree holds: %s", strings.Join(items, ", "))
	}
	if s.Budget != nil && len(s.Budget.Unknown) > 0 {
		fmt.Fprintf(&b, " (headroom may be lower than shown: no reading yet from %s)", strings.Join(s.Budget.Unknown, ", "))
	}
	if stale := staleSources(s); len(stale) > 0 {
		fmt.Fprintf(&b, " (decided on stale readings from %s: their latest read failed)", strings.Join(stale, ", "))
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

// staleSources lists the policy's inputs whose latest read failed.
func staleSources(s *protocol.Snapshot) []string {
	var out []string
	for _, n := range []string{"host", "docker", "tart", "lmstudio", "ollama", "orca"} {
		if s.Sources[n].Stale {
			out = append(out, n)
		}
	}
	return out
}
