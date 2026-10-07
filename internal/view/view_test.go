package view_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/view"
)

const gib = uint64(1 << 30)

func u64(v uint64) *uint64   { return &v }
func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// busy is a 64 GB Mac with two active worktrees, an idle one holding a VM,
// an idle one holding nothing, and one stray container.
func busy() *protocol.Snapshot {
	return &protocol.Snapshot{
		Seq: 9, CollectedAt: now.Add(-3 * time.Second),
		Sources: map[string]protocol.SourceStatus{"host": {}, "docker": {}, "tart": {}, "orca": {}, "lmstudio": {}},
		Host: &protocol.Host{TotalBytes: 64 * gib, UsedBytes: u64(22*gib + gib/2), Pressure: "normal",
			SwapTotalBytes: 2 * gib, SwapUsedBytes: gib / 10,
			Trend: protocol.Trend{Samples: 60, Worst: "warn", WarnSeconds: 12, Direction: "steady"}},
		Budget: &protocol.Budget{TotalBytes: 64 * gib, ReservedBytes: 37 * gib, HeadroomBytes: i64(int64(27 * gib)),
			Components: []protocol.BudgetComponent{
				{Name: "docker", ReservedBytes: 6 * gib, UsedBytes: u64(6 * gib)},
				{Name: "tart", ReservedBytes: 8 * gib, UsedBytes: u64(5 * gib)},
				{Name: "lmstudio", ReservedBytes: 13 * gib, UsedBytes: u64(13 * gib)},
				{Name: "host_baseline", ReservedBytes: 10 * gib, UsedBytes: u64(19*gib + gib/2)},
			}},
		Orca: &protocol.Orca{Installed: true, Running: true, Worktrees: []protocol.Worktree{
			{ID: "w1", Name: "Fix login", Agents: []protocol.Agent{{Type: "claude", State: "working"}}},
			{ID: "w2", Name: "Release prep", Agents: []protocol.Agent{{Type: "claude", State: "done"}}},
			{ID: "w3", Name: "Old spike"},
			{ID: "w4", Name: "Two agents", Agents: []protocol.Agent{{Type: "codex", State: "waiting"}, {Type: "claude", State: "working"}}},
		}},
		Attribution: &protocol.Attribution{
			Worktrees: []protocol.WorktreeUsage{
				{ID: "w1", Name: "Fix login", AgentMemoryBytes: u64(gib), AgentCPUPercent: f64(30), Usage: protocol.Usage{
					Containers:           []protocol.AttributedContainer{{Name: "db"}, {Name: "web"}},
					ContainerMemoryBytes: 3 * gib, ContainerCPUPercent: f64(15),
					TartVMs: []protocol.AttributedVM{}, TartFootprintBytes: u64(0)}},
				{ID: "w2", Name: "Release prep", AgentMemoryBytes: u64(gib / 2), Usage: protocol.Usage{
					Containers: []protocol.AttributedContainer{}, ContainerCPUPercent: f64(0),
					TartVMs: []protocol.AttributedVM{{Name: "ci"}}, TartMemoryBytes: 8 * gib}},
				{ID: "w3", Name: "Old spike", Usage: protocol.Usage{
					Containers: []protocol.AttributedContainer{}, TartVMs: []protocol.AttributedVM{}}},
				{ID: "w4", Name: "Two agents", AgentMemoryBytes: u64(2 * gib), Usage: protocol.Usage{
					Containers: []protocol.AttributedContainer{}, TartVMs: []protocol.AttributedVM{}}},
			},
			Unattributed: protocol.Usage{
				Containers:           []protocol.AttributedContainer{{Name: "db-old", Reason: "no_match"}},
				ContainerMemoryBytes: 3 * gib,
				TartVMs:              []protocol.AttributedVM{},
			},
		},
	}
}

func render(t *testing.T, s *protocol.Snapshot, o view.Options) []string {
	t.Helper()
	if o.Width == 0 {
		o.Width = 200
	}
	if o.Now.IsZero() {
		o.Now = now
	}
	if o.TrendWindow == 0 {
		o.TrendWindow = 5 * time.Minute
	}
	var b bytes.Buffer
	view.Render(&b, s, o)
	return strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
}

func has(t *testing.T, line, want string) {
	t.Helper()
	if !strings.Contains(line, want) {
		t.Errorf("line %q lacks %q", line, want)
	}
}

func TestTopLine(t *testing.T) {
	top := render(t, busy(), view.Options{})[0]
	for _, want := range []string{"total 64.0", "reserved 37.0", "used 22.5", "headroom 27.0 GB", "swap 0.1/2.0 GB",
		"pressure normal", "(5m: steady, worst warn 12s)"} {
		has(t, top, want)
	}
	if !strings.HasPrefix(top, "headroom 27.0 GB · pressure normal") {
		t.Errorf("headroom and pressure must lead, so a narrow terminal keeps them: %q", top)
	}
}

func TestTopLineUnknowns(t *testing.T) {
	s := busy()
	s.Host.UsedBytes = nil // macOS 15
	s.Budget.Unknown = []string{"lmstudio"}
	top := render(t, s, view.Options{})[0]
	has(t, top, "used ?")
	has(t, top, "headroom ≤27.0 GB")

	s.Budget.HeadroomBytes = i64(-int64(3 * gib))
	s.Budget.Unknown = nil
	has(t, render(t, s, view.Options{})[0], "headroom -3.0 GB")

	s.Budget.HeadroomBytes = nil
	has(t, render(t, s, view.Options{})[0], "headroom ?")

	s.Host, s.Budget = nil, nil
	has(t, render(t, s, view.Options{})[0], "host unknown")
}

func TestTrendQuietWhenNormal(t *testing.T) {
	s := busy()
	s.Host.Trend = protocol.Trend{Samples: 60, Worst: "normal", Direction: "falling"}
	top := render(t, s, view.Options{})[0]
	has(t, top, "(5m: falling)")
	if strings.Contains(top, "worst") {
		t.Errorf("top line %q mentions worst at normal", top)
	}
}

func TestPressureColourOnlyWhenAsked(t *testing.T) {
	s := busy()
	s.Host.Pressure = "critical"
	if top := render(t, s, view.Options{})[0]; strings.Contains(top, "\x1b[") {
		t.Errorf("colour without Color: %q", top)
	}
	has(t, render(t, s, view.Options{Color: true})[0], "\x1b[31mcritical\x1b[0m")
}

// line returns the first line containing sub, failing if none does.
func line(t *testing.T, lines []string, sub string) string {
	t.Helper()
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return l
		}
	}
	t.Fatalf("no line contains %q:\n%s", sub, strings.Join(lines, "\n"))
	return ""
}

func TestReservedLine(t *testing.T) {
	l := render(t, busy(), view.Options{})[1]
	has(t, l, "reserved: docker 6.0 · tart 8.0 · lmstudio 13.0 · baseline 10.0 (in use 19.5)")

	s := busy()
	s.Budget.Components[3].UsedBytes = nil
	s.Budget.Components[0].ReservedBytes = 0
	l = render(t, s, view.Options{})[1]
	has(t, l, "reserved: tart 8.0 · lmstudio 13.0 · baseline 10.0 (in use ?)")
}

func TestWorktreeRows(t *testing.T) {
	lines := render(t, busy(), view.Options{})
	line(t, lines, "WORKTREE")
	r := line(t, lines, "Fix login")
	for _, want := range []string{"claude working", "2   3.0 GB", "15%", "1.0 GB"} {
		has(t, r, want)
	}
	if strings.Contains(r, "⚑") {
		t.Errorf("working row marked idle: %q", r)
	}
	r = line(t, lines, "Release prep")
	for _, want := range []string{"claude done", "⚑", "1   8.0 GB", "0.5 GB"} {
		has(t, r, want)
	}
	// The working agent is shown first; the other is counted.
	r = line(t, lines, "Two agents")
	has(t, r, "claude working +1")
}

func TestUnknownTotalsShowQuestionMarks(t *testing.T) {
	s := busy()
	s.Attribution.Worktrees[0].ContainerCPUPercent = nil
	s.Attribution.Worktrees[0].AgentMemoryBytes = nil
	r := line(t, render(t, s, view.Options{}), "Fix login")
	if strings.Count(r, "?") != 2 {
		t.Fatalf("want CPU and agent memory as ?: %q", r)
	}
}

func TestIdleWorktreesHiddenUnlessAll(t *testing.T) {
	lines := render(t, busy(), view.Options{})
	for _, l := range lines {
		if strings.Contains(l, "Old spike") {
			t.Fatalf("idle worktree shown: %q", l)
		}
	}
	has(t, lines[len(lines)-1], "1 idle worktree hidden (--all)")
	line(t, render(t, busy(), view.Options{All: true}), "Old spike")
}

func TestIdleHolderWithoutAgentIsMarked(t *testing.T) {
	s := busy()
	s.Attribution.Worktrees[2].Containers = []protocol.AttributedContainer{{Name: "left-over"}}
	s.Attribution.Worktrees[2].ContainerMemoryBytes = gib
	r := line(t, render(t, s, view.Options{}), "Old spike")
	has(t, r, "⚑")
	has(t, r, "–")
}

func TestUnattributedRowAndItems(t *testing.T) {
	lines := render(t, busy(), view.Options{})
	has(t, line(t, lines, "unattributed"), "1   3.0 GB")
	line(t, lines, "db-old (no_match)")

	s := busy()
	s.Attribution.Unattributed = protocol.Usage{}
	for _, l := range render(t, s, view.Options{}) {
		if strings.Contains(l, "unattributed") {
			t.Fatalf("empty unattributed row shown: %q", l)
		}
	}
}

func TestFooter(t *testing.T) {
	s := busy()
	s.Sources["docker"] = protocol.SourceStatus{Stale: true, Err: "timed out after 3s"}
	s.Attribution.OrcaStale = true
	s.Sources["orca"] = protocol.SourceStatus{Stale: true, Err: "orca: exit 1"}
	lines := render(t, s, view.Options{})
	f := lines[len(lines)-1]
	for _, want := range []string{"updated 3s ago", "docker stale: timed out after 3s", "orca stale: worktree list may be out of date"} {
		has(t, f, want)
	}
}

func TestNoAttributionYet(t *testing.T) {
	s := busy()
	s.Attribution, s.Seq = nil, 0
	lines := render(t, s, view.Options{})
	line(t, lines, "worktrees unknown")
	has(t, lines[len(lines)-1], "no data yet")
}

func TestLinesFitTheWidth(t *testing.T) {
	for _, w := range []int{60, 100} {
		for _, l := range render(t, busy(), view.Options{Width: w, Color: true}) {
			if n := visible(l); n > w {
				t.Errorf("width %d: %d columns: %q", w, n, l)
			}
		}
	}
}

// visible counts columns, skipping ANSI escapes.
func visible(s string) int {
	n, esc := 0, false
	for _, c := range s {
		switch {
		case esc:
			esc = c != 'm'
		case c == '\x1b':
			esc = true
		default:
			n++
		}
	}
	return n
}
