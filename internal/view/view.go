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
	"unicode"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/units"
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
			parts = append(parts, Clean(c.Name)+" "+num(c.ReservedBytes))
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
	// Worktrees ("" for unattributed) holding a container or VM that
	// appeared without a check (#33).
	ungated := map[string]bool{}
	for _, u := range s.Ungated {
		ungated[u.Worktree] = true
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
		if ungated[w.ID] {
			// Holds something started without a check (R4, #33). One mark
			// per row: two wide glyphs would shift the columns.
			mark = "⚠"
		}
		r.add(rowFormat, short(name(w), 18), agentSummary(ag), mark,
			containers(w.Usage), vms(w.Usage), cpu(w.Usage, w.AgentCPUPercent, true), gbOrUnknown(w.AgentMemoryBytes))
	}
	u := a.Unattributed
	if len(u.Containers)+len(u.TartVMs) > 0 {
		mark := ""
		if ungated[""] {
			mark = "⚠"
		}
		r.add(rowFormat, "unattributed", "", mark, containers(u), vms(u), cpu(u, nil, false), "")
		var items []string
		for _, c := range u.Containers {
			items = append(items, fmt.Sprintf("%s (%s)", Clean(c.Name), Clean(c.Reason)))
		}
		for _, vm := range u.TartVMs {
			items = append(items, fmt.Sprintf("%s (%s)", Clean(vm.Name), Clean(vm.Reason)))
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
	if n := len(s.Ungated); n > 0 {
		var names []string
		for i, u := range s.Ungated {
			if i == 3 {
				names = append(names, "…")
				break
			}
			names = append(names, Clean(u.Name))
		}
		parts = append(parts, r.paint(yellow, fmt.Sprintf("%d ungated: %s (started without headroom; see headroom doctor)", n, strings.Join(names, ", "))))
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
		msg := Clean(st.Err)
		if n == "orca" && s.Attribution != nil && s.Attribution.OrcaStale {
			msg = "worktree list may be out of date"
		}
		parts = append(parts, r.paint(yellow, Clean(n)+" stale: "+msg))
	}
	r.add("%s", strings.Join(parts, " · "))
}

func name(w protocol.WorktreeUsage) string {
	if w.Name != "" {
		return Clean(w.Name)
	}
	return Clean(filepath.Base(w.Path))
}

// Clean makes text from outside headroom (names, errors) safe to print:
// control characters, which could move the cursor, set the title or write
// the clipboard, and Unicode bidi overrides become "?". After this, the only
// escapes in a line are the view's own colours.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return '?'
		}
		return r
	}, s)
}

// durationLabel names the trend window: 5m, 30s, 1m30s, 1h.
func durationLabel(d time.Duration) string {
	s := d.String() // 5m0s, 30s, 1m30s, 1h0m0s
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// short cuts s to n columns, marking the cut with an ellipsis.
func short(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-1]) + "…"
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
	s := Clean(first.Type) + " " + Clean(first.State)
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

// cpu is the containers' CPU plus, for a worktree, its agents' own; "?" if
// a part is unknown. Tart VMs' CPU is not measured yet.
func cpu(u protocol.Usage, agents *float64, withAgents bool) string {
	total := 0.0
	if len(u.Containers) > 0 {
		if u.ContainerCPUPercent == nil {
			return "?"
		}
		total += *u.ContainerCPUPercent
	}
	if withAgents {
		if agents == nil {
			return "?"
		}
		total += *agents
	} else if len(u.Containers) == 0 {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", total)
}

func gbOrUnknown(b *uint64) string {
	if b == nil {
		return "?"
	}
	return num(*b) + " GB"
}

func (r *renderer) trend(t protocol.Trend) string {
	label := durationLabel(r.o.TrendWindow)
	dir := Clean(t.Direction)
	if dir == "" {
		dir = "?"
	}
	if t.Worst == "" || t.Worst == "normal" {
		return fmt.Sprintf("(%s: %s)", label, dir)
	}
	secs := t.WarnSeconds
	if t.Worst == "critical" {
		secs = t.CriticalSeconds
	}
	return fmt.Sprintf("(%s: %s, worst %s %.0fs)", label, dir, Clean(t.Worst), secs)
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
	case "":
		return "?"
	}
	return Clean(level)
}

func (r *renderer) paint(color, s string) string {
	if !r.o.Color {
		return s
	}
	return color + s + reset
}

// num formats bytes as GB (GiB, as macOS reports memory) to one decimal.
func num(b uint64) string { return units.GB(b) }

func signed(b int64) string { return units.SignedGB(b) }

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
