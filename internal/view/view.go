// Package view renders the observe view (R4): the host's budget on top, one
// row per worktree below, then what could not be attributed. It is a pure
// function of a snapshot, so `headroom` and `headroom --watch` share it.
package view

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// Options shape one rendering.
type Options struct {
	// Width cuts every line to this many columns.
	Width int
	// Color adds ANSI colour (a TTY without NO_COLOR).
	Color bool
	// All shows worktrees that have no agent and run nothing.
	All bool
	// Now is the time the snapshot's age is measured against.
	Now time.Time
	// TrendWindow labels the pressure trend (daemon.trend_window).
	TrendWindow time.Duration
}

// Render writes the view of s to w.
func Render(w io.Writer, s *protocol.Snapshot, o Options) {
	r := &renderer{o: o}
	r.top(s)
	r.reserved(s.Budget)
	r.add("")
	hidden := r.worktrees(s)
	r.footer(s, hidden)
	for _, l := range r.lines {
		fmt.Fprintln(w, cut(l, o.Width))
	}
}

type renderer struct {
	o     Options
	lines []string
}

func (r *renderer) add(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

// top is the host line: headroom, pressure, total, reserved, used, swap.
func (r *renderer) top(s *protocol.Snapshot) {
	h, b := s.Host, s.Budget
	if h == nil {
		r.add("headroom  host unknown")
		return
	}
	used := "?"
	if h.UsedBytes != nil {
		used = num(*h.UsedBytes)
	}
	reserved, headroom := "?", "?"
	if b != nil {
		reserved = num(b.ReservedBytes)
		if b.HeadroomBytes != nil {
			headroom = signed(*b.HeadroomBytes) + " GB"
			if len(b.Unknown) > 0 {
				headroom = "≤" + headroom
			}
		}
	}
	// Most important first: a narrow terminal cuts the end of the line.
	r.add("headroom %s · pressure %s %s · total %s · reserved %s · used %s · swap %s/%s GB",
		headroom, r.pressure(h.Pressure), r.trend(h.Trend),
		num(h.TotalBytes), reserved, used, num(h.SwapUsedBytes), num(h.SwapTotalBytes))
}

// reserved breaks the reservation down by component. The baseline also shows
// what is in use outside every component, so a baseline set too small shows.
func (r *renderer) reserved(b *protocol.Budget) {
	if b == nil {
		return
	}
	var parts []string
	for _, c := range b.Components {
		if c.Name == "host_baseline" {
			inUse := "?"
			if c.UsedBytes != nil {
				inUse = num(*c.UsedBytes)
			}
			parts = append(parts, fmt.Sprintf("baseline %s (in use %s)", num(c.ReservedBytes), inUse))
			continue
		}
		if c.ReservedBytes > 0 {
			parts = append(parts, c.Name+" "+num(c.ReservedBytes))
		}
	}
	r.add("  reserved: %s", strings.Join(parts, " · "))
}

const rowFormat = "%-18s %-20s %-2s %-13s %-13s %-6s %s"

// worktrees adds a row per worktree and the unattributed row; it returns
// how many idle worktrees it left out.
func (r *renderer) worktrees(s *protocol.Snapshot) (hidden int) {
	a := s.Attribution
	if a == nil {
		r.add("worktrees unknown")
		return 0
	}
	agents := map[string][]protocol.Agent{}
	if s.Orca != nil {
		for _, w := range s.Orca.Worktrees {
			agents[w.ID] = w.Agents
		}
	}
	r.add(rowFormat, "WORKTREE", "AGENTS", "", "CONTAINERS", "TART VMS", "CPU", "AGENT MEM")
	for _, w := range a.Worktrees {
		ag := agents[w.ID]
		holds := len(w.Containers) > 0 || len(w.TartVMs) > 0
		if !r.o.All && len(ag) == 0 && !holds {
			hidden++
			continue
		}
		mark := ""
		if holds && !anyWorking(ag) {
			mark = "⚑" // holds resources while no agent works (R4, #24)
		}
		r.add(rowFormat, short(name(w), 18), agentSummary(ag), mark,
			containers(w.Usage), vms(w.Usage), cpu(w.Usage), gbOrUnknown(w.AgentMemoryBytes))
	}
	u := a.Unattributed
	if len(u.Containers)+len(u.TartVMs) > 0 {
		r.add(rowFormat, "unattributed", "", "", containers(u), vms(u), cpu(u), "")
		var items []string
		for _, c := range u.Containers {
			items = append(items, fmt.Sprintf("%s (%s)", c.Name, c.Reason))
		}
		for _, vm := range u.TartVMs {
			items = append(items, fmt.Sprintf("%s (%s)", vm.Name, vm.Reason))
		}
		r.add("  %s", strings.Join(items, ", "))
	}
	return hidden
}

// footer notes hidden rows, stale sources and the snapshot's age.
func (r *renderer) footer(s *protocol.Snapshot, hidden int) {
	r.add("")
	var parts []string
	if s.Seq == 0 {
		parts = append(parts, "no data yet")
	} else {
		parts = append(parts, fmt.Sprintf("updated %s ago", r.o.Now.Sub(s.CollectedAt).Round(time.Second)))
	}
	switch hidden {
	case 0:
	case 1:
		parts = append(parts, "1 idle worktree hidden (--all)")
	default:
		parts = append(parts, fmt.Sprintf("%d idle worktrees hidden (--all)", hidden))
	}
	names := make([]string, 0, len(s.Sources))
	for n := range s.Sources {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		st := s.Sources[n]
		if !st.Stale {
			continue
		}
		msg := st.Err
		if n == "orca" && s.Attribution != nil && s.Attribution.OrcaStale {
			msg = "worktree list may be out of date"
		}
		parts = append(parts, r.paint(yellow, n+" stale: "+msg))
	}
	r.add("%s", strings.Join(parts, " · "))
}

func name(w protocol.WorktreeUsage) string {
	if w.Name != "" {
		return w.Name
	}
	return filepath.Base(w.Path)
}

// short cuts s to n columns, marking the cut with an ellipsis.
func short(s string, n int) string {
	rs := []rune(s)
	if len(rs) < n {
		return s
	}
	return string(rs[:n-2]) + "…"
}

// stateRank orders agent states for the summary: the busiest comes first.
var stateRank = map[string]int{"working": 0, "waiting": 1, "done": 2}

func rank(state string) int {
	if r, ok := stateRank[state]; ok {
		return r
	}
	return len(stateRank)
}

// agentSummary names the busiest agent and counts the rest.
func agentSummary(ag []protocol.Agent) string {
	if len(ag) == 0 {
		return "–"
	}
	first := slices.MinFunc(ag, func(a, b protocol.Agent) int { return rank(a.State) - rank(b.State) })
	s := first.Type + " " + first.State
	if len(ag) > 1 {
		s += fmt.Sprintf(" +%d", len(ag)-1)
	}
	return s
}

func anyWorking(ag []protocol.Agent) bool {
	return slices.ContainsFunc(ag, func(a protocol.Agent) bool { return a.State == "working" })
}

func containers(u protocol.Usage) string {
	if len(u.Containers) == 0 {
		return "–"
	}
	return fmt.Sprintf("%-3d %s GB", len(u.Containers), num(u.ContainerMemoryBytes))
}

// vms shows the VMs' configured memory: what they reserve.
func vms(u protocol.Usage) string {
	if len(u.TartVMs) == 0 {
		return "–"
	}
	return fmt.Sprintf("%-3d %s GB", len(u.TartVMs), num(u.TartMemoryBytes))
}

func cpu(u protocol.Usage) string {
	switch {
	case len(u.Containers) == 0:
		return "–"
	case u.ContainerCPUPercent == nil:
		return "?"
	}
	return fmt.Sprintf("%.0f%%", *u.ContainerCPUPercent)
}

func gbOrUnknown(b *uint64) string {
	if b == nil {
		return "?"
	}
	return num(*b) + " GB"
}

func (r *renderer) trend(t protocol.Trend) string {
	label := fmt.Sprintf("%dm", int(r.o.TrendWindow.Minutes()))
	if t.Worst == "" || t.Worst == "normal" {
		return fmt.Sprintf("(%s: %s)", label, t.Direction)
	}
	secs := t.WarnSeconds
	if t.Worst == "critical" {
		secs = t.CriticalSeconds
	}
	return fmt.Sprintf("(%s: %s, worst %s %.0fs)", label, t.Direction, t.Worst, secs)
}

// ANSI colours.
const (
	red    = "\x1b[31m"
	yellow = "\x1b[33m"
	green  = "\x1b[32m"
	dim    = "\x1b[2m"
	reset  = "\x1b[0m"
)

func (r *renderer) pressure(level string) string {
	switch level {
	case "critical":
		return r.paint(red, level)
	case "warn":
		return r.paint(yellow, level)
	case "normal":
		return r.paint(green, level)
	}
	return level
}

func (r *renderer) paint(color, s string) string {
	if !r.o.Color {
		return s
	}
	return color + s + reset
}

// num formats bytes as GB (GiB, as macOS reports memory) to one decimal.
func num(b uint64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<30)) }

func signed(b int64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<30)) }

// cut shortens s to width visible columns. ANSI escapes take no columns and
// are kept whole; a cut line ends with a reset so colour cannot leak.
func cut(s string, width int) string {
	if width <= 0 {
		return s
	}
	var b strings.Builder
	cols, esc, colored := 0, false, false
	for _, c := range s {
		switch {
		case esc:
			b.WriteRune(c)
			if c == 'm' {
				esc = false
			}
			continue
		case c == '\x1b':
			esc, colored = true, true
			b.WriteRune(c)
			continue
		}
		if cols == width {
			if colored {
				b.WriteString(reset)
			}
			return b.String()
		}
		b.WriteRune(c)
		cols++
	}
	return b.String()
}
