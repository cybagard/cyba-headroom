package orca_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/source/orca"
)

// fakeCLI answers orca subcommands from testdata files or inline JSON. Like
// the real CLI, a reply with "ok": false comes with a non-zero exit.
type fakeCLI map[string]string

func (f fakeCLI) Run(_ context.Context, args ...string) ([]byte, error) {
	body, ok := f[strings.Join(args, " ")]
	if !ok {
		return nil, errors.New("unexpected orca " + strings.Join(args, " "))
	}
	b := []byte(body)
	if !strings.HasPrefix(body, "{") {
		var err error
		if b, err = os.ReadFile("testdata/" + body); err != nil {
			return nil, err
		}
	}
	if strings.Contains(string(b), `"ok": false`) {
		return b, errors.New("exit status 1")
	}
	return b, nil
}

func collect(t *testing.T, src *orca.Source) protocol.Orca {
	t.Helper()
	r, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var snap protocol.Snapshot
	r.Apply(&snap)
	if snap.Orca == nil {
		t.Fatal("reading did not fill Snapshot.Orca")
	}
	return *snap.Orca
}

func TestOrcaNotInstalled(t *testing.T) {
	got := collect(t, orca.New(nil))
	if got.Installed || got.Running || got.Worktrees == nil || len(got.Worktrees) != 0 {
		t.Fatalf("got %+v, want not installed with an empty worktree list", got)
	}
}

func TestOrcaNotRunningIsAReading(t *testing.T) {
	cli := fakeCLI{
		"worktree ps --json":        "not-running.json",
		"diagnostics memory --json": "not-running.json",
	}
	got := collect(t, orca.New(cli))
	if !got.Installed || got.Running || len(got.Worktrees) != 0 {
		t.Fatalf("got %+v, want installed but not running", got)
	}
}

const thisWorktree = "00000000-0000-4000-8000-000000000008::/Users/dev/orca/workspaces/cyba-headroom/main"

func runningCLI() fakeCLI {
	return fakeCLI{
		"worktree ps --json":        "worktree-ps.json",
		"diagnostics memory --json": "diagnostics-memory.json",
	}
}

func find(t *testing.T, o protocol.Orca, id string) protocol.Worktree {
	t.Helper()
	for _, w := range o.Worktrees {
		if w.ID == id {
			return w
		}
	}
	t.Fatalf("worktree %s not in %+v", id, o.Worktrees)
	return protocol.Worktree{}
}

func TestWorktreesAndAgentStates(t *testing.T) {
	got := collect(t, orca.New(runningCLI()))
	if !got.Running || len(got.Worktrees) != 7 {
		t.Fatalf("running=%v worktrees=%d, want running with 7", got.Running, len(got.Worktrees))
	}
	w := find(t, got, thisWorktree)
	if w.Path != "/Users/dev/orca/workspaces/cyba-headroom/main" || w.Branch != "main" || w.Status != "working" || w.LiveTerminals != 1 {
		t.Fatalf("worktree = %+v", w)
	}
	if !w.LastActivityAt.Equal(time.UnixMilli(1791394967031)) {
		t.Fatalf("last activity = %v", w.LastActivityAt)
	}
	want := protocol.Agent{
		PaneKey:    "00000000-0000-4000-8000-00000000000c:00000000-0000-4000-8000-000000000003",
		Type:       "claude",
		State:      "working",
		StateSince: time.UnixMilli(1791395044345),
	}
	if len(w.Agents) != 1 || w.Agents[0] != want {
		t.Fatalf("agents = %+v, want [%+v]", w.Agents, want)
	}
}

func TestSkipsArchivedAndRemoteWorktrees(t *testing.T) {
	cli := fakeCLI{
		"worktree ps --json": `{"ok": true, "result": {"worktrees": [
			{"worktreeId": "r::/a", "path": "/a", "hostId": "local", "isArchived": false, "agents": []},
			{"worktreeId": "r::/b", "path": "/b", "hostId": "local", "isArchived": true, "agents": []},
			{"worktreeId": "r::/c", "path": "/c", "hostId": "ssh-box", "isArchived": false, "agents": []}]}}`,
		"diagnostics memory --json": `{"ok": true, "result": {"app": {"memory": 0}, "worktrees": []}}`,
	}
	got := collect(t, orca.New(cli))
	if len(got.Worktrees) != 1 || got.Worktrees[0].ID != "r::/a" {
		t.Fatalf("worktrees = %+v, want only the local unarchived one", got.Worktrees)
	}
}

const orchestrator = "00000000-0000-4000-8000-00000000000b::/Users/dev/orca/workspaces/project-a/feature-2"

func TestAgentsMemoryCPUAndSessionPIDs(t *testing.T) {
	got := collect(t, orca.New(runningCLI()))
	if got.AppMemoryBytes != 1009139712 {
		t.Fatalf("app memory = %d", got.AppMemoryBytes)
	}
	cases := []struct {
		id   string
		mem  uint64
		cpu  float64
		pids []int
	}{
		{thisWorktree, 852475904, 15.8, []int{69132}},
		{orchestrator, 381140992, 0.2, []int{1557}},
	}
	for _, tc := range cases {
		w := find(t, got, tc.id)
		if w.MemoryBytes != tc.mem || w.CPUPercent != tc.cpu || !slices.Equal(w.SessionPIDs, tc.pids) {
			t.Errorf("%s: mem=%d cpu=%v pids=%v, want %d %v %v", w.Name, w.MemoryBytes, w.CPUPercent, w.SessionPIDs, tc.mem, tc.cpu, tc.pids)
		}
	}
	// No live terminals, no diagnostics entry: nothing running there.
	for _, w := range got.Worktrees {
		if w.ID != thisWorktree && w.ID != orchestrator && (w.MemoryBytes != 0 || len(w.SessionPIDs) != 0) {
			t.Errorf("%s: mem=%d pids=%v, want none", w.Name, w.MemoryBytes, w.SessionPIDs)
		}
	}
}

func TestMemoryDiagnosticsFailureKeepsWorktrees(t *testing.T) {
	cli := runningCLI()
	cli["diagnostics memory --json"] = `{"ok": false, "error": {"code": "unknown_command", "message": "no such command"}}`
	got := collect(t, orca.New(cli))
	if len(got.Worktrees) != 7 || got.MemoryError == "" {
		t.Fatalf("worktrees=%d memory_error=%q, want 7 and an error", len(got.Worktrees), got.MemoryError)
	}
}

func TestOtherCLIFailuresAreErrors(t *testing.T) {
	for name, body := range map[string]string{
		"error reply": `{"ok": false, "error": {"code": "internal", "message": "boom"}}`,
		"not JSON":    `{garbage`,
		"wrong shape": `{"ok": true, "result": {"worktrees": "nope"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			cli := runningCLI()
			cli["worktree ps --json"] = body
			if _, err := orca.New(cli).Collect(context.Background()); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestOrcaQuittingBetweenCallsLeavesMemoryUnknown(t *testing.T) {
	cli := runningCLI()
	cli["diagnostics memory --json"] = "not-running.json"
	got := collect(t, orca.New(cli))
	if got.MemoryError == "" || got.AppMemoryBytes != 0 {
		t.Fatalf("memory_error=%q app=%d, want memory marked unknown", got.MemoryError, got.AppMemoryBytes)
	}
}

func TestHalfDecodedMemoryIsNotUsed(t *testing.T) {
	cli := runningCLI()
	// A newer Orca changes a field's type after app memory was decoded.
	cli["diagnostics memory --json"] = `{"ok": true, "result": {"app": {"memory": 123},
		"worktrees": [{"worktreeId": "` + thisWorktree + `", "memory": 5, "cpu": 1, "sessions": [{"pid": "x"}]}]}}`
	got := collect(t, orca.New(cli))
	w := find(t, got, thisWorktree)
	if got.MemoryError == "" || got.AppMemoryBytes != 0 || w.MemoryBytes != 0 {
		t.Fatalf("memory_error=%q app=%d worktree=%d, want error and no partial figures", got.MemoryError, got.AppMemoryBytes, w.MemoryBytes)
	}
}

func TestRowWithoutHostIDIsLocal(t *testing.T) {
	cli := fakeCLI{
		"worktree ps --json":        `{"ok": true, "result": {"worktrees": [{"worktreeId": "r::/a", "path": "/a", "agents": []}]}}`,
		"diagnostics memory --json": `{"ok": true, "result": {"app": {"memory": 0}, "worktrees": []}}`,
	}
	if got := collect(t, orca.New(cli)); len(got.Worktrees) != 1 {
		t.Fatalf("worktrees = %+v, want the row without hostId kept as local", got.Worktrees)
	}
}

func TestErrorsCarryTheCodeNotOrcasMessage(t *testing.T) {
	cli := runningCLI()
	cli["diagnostics memory --json"] = `{"ok": false, "error": {"code": "internal", "message": "failed reading /Users/alice/secret-project"}}`
	got := collect(t, orca.New(cli))
	if !strings.Contains(got.MemoryError, "internal") || strings.Contains(got.MemoryError, "alice") {
		t.Fatalf("memory_error = %q, want the code without Orca's free text", got.MemoryError)
	}
}

// slowCLI delays every call, like CLI start-up on a loaded machine.
type slowCLI struct {
	fakeCLI
	delay time.Duration
}

func (s slowCLI) Run(ctx context.Context, args ...string) ([]byte, error) {
	time.Sleep(s.delay)
	return s.fakeCLI.Run(ctx, args...)
}

func TestBothCallsRunConcurrently(t *testing.T) {
	start := time.Now()
	collect(t, orca.New(slowCLI{runningCLI(), 200 * time.Millisecond}))
	if el := time.Since(start); el >= 400*time.Millisecond {
		t.Fatalf("collect took %s, want the two 200 ms calls overlapped", el)
	}
}
