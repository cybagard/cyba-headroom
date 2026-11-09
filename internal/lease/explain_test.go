package lease_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/agents"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

const plainUngated = `msg="ungated: a container or VM appeared without a check (socket or SDK use, a login shell, an agent not launched through headroom run, or the daemon down)" name=c2 kind=container worktree=w1` + "\n"

// The ungated warning names the worktree's unshimmed agent when there is
// one (#297), asking once per worktree per reading.
func TestTheUngatedWarningNamesAnUnshimmedAgent(t *testing.T) {
	b, c, log := book(t)
	var asked []string
	b.SetExplain(func(_ context.Context, w protocol.Worktree) []agents.Agent {
		asked = append(asked, w.ID)
		return []agents.Agent{{PID: 4242, Name: "claude", Tools: []string{"docker", "tart"}}}
	})
	b.Observe(snap())
	c.t = c.t.Add(5 * time.Second)
	b.Observe(withContainer(withContainer(snap(), "c2", "w1"), "c3", "w1"))
	want := `msg="ungated: a container or VM appeared without a check in A; its agent claude (pid 4242) runs docker, tart without the shims: launch it through headroom run" name=c2 kind=container worktree=w1 agent=claude agent_pid=4242 tools="[docker tart]"`
	if !strings.Contains(log.String(), want) || strings.Count(log.String(), "its agent claude") != 2 {
		t.Fatalf("log = %s\nwant %s", log, want)
	}
	if len(asked) != 1 || asked[0] != "w1" {
		t.Fatalf("asked for %v, want w1 once", asked)
	}
}

func TestTheUngatedWarningIsUnchangedWithoutAnUnshimmedAgent(t *testing.T) {
	for name, explain := range map[string]func(context.Context, protocol.Worktree) []agents.Agent{
		"no hook":  nil,
		"no agent": func(context.Context, protocol.Worktree) []agents.Agent { return nil },
	} {
		b, c, log := book(t)
		if explain != nil {
			b.SetExplain(explain)
		}
		b.Observe(snap())
		c.t = c.t.Add(5 * time.Second)
		b.Observe(withContainer(snap(), "c2", "w1"))
		if _, line, _ := strings.Cut(log.String(), "level=WARN "); line != plainUngated {
			t.Fatalf("%s: log = %q, want %q", name, line, plainUngated)
		}
	}
}

// envProcs is one agent, pid 7, under session 1, with env.
type envProcs struct{ env map[string]string }

func (envProcs) Descendants(int) ([]vmproc.Process, error) {
	return []vmproc.Process{{PID: 1, Args: []string{"login"}}, {PID: 7, PPID: 1, Args: []string{"claude"}}}, nil
}

func (p envProcs) PathOf(int) (string, error) { return p.env["PATH"], nil }

// Of an agent's environment, the warning carries nothing: not PATH, not
// another variable.
func TestTheUngatedWarningCarriesNoEnvironment(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	procs := envProcs{map[string]string{"PATH": dir, "SECRET": "planted-secret"}}
	b, c, log := book(t)
	b.SetExplain(func(ctx context.Context, _ protocol.Worktree) []agents.Agent {
		return agents.Unshimmed(ctx, procs, []int{1}, "/Users/dev/.config/headroom/shims", []string{"docker"})
	})
	b.Observe(snap())
	c.t = c.t.Add(5 * time.Second)
	b.Observe(withContainer(snap(), "c2", "w1"))
	if l := log.String(); !strings.Contains(l, "agent_pid=7") || strings.Contains(l, "planted-secret") || strings.Contains(l, dir) {
		t.Fatalf("log = %s", l)
	}
}

// The warning names an agent that runs a tool that can make the resource:
// one that bypasses only tart did not start a container.
func TestTheUngatedWarningNamesAnAgentThatCanMakeIt(t *testing.T) {
	tartOnly := agents.Agent{PID: 11, Name: "codex", Tools: []string{"tart"}}
	docker := agents.Agent{PID: 12, Name: "claude", Tools: []string{"docker"}}
	for name, c := range map[string]struct {
		found []agents.Agent
		want  string
	}{
		"a docker-bypassing agent after a tart-only one": {[]agents.Agent{tartOnly, docker}, `its agent claude (pid 12) runs docker without the shims`},
		"only a tart-only agent":                         {[]agents.Agent{tartOnly}, ""},
	} {
		b, clk, log := book(t)
		calls := 0
		b.SetExplain(func(context.Context, protocol.Worktree) []agents.Agent { calls++; return c.found })
		b.Observe(snap())
		clk.t = clk.t.Add(5 * time.Second)
		b.Observe(withContainer(withContainer(snap(), "c2", "w1"), "c3", "w1"))
		if calls != 1 {
			t.Errorf("%s: the hook ran %d times, want once", name, calls)
		}
		_, line, _ := strings.Cut(log.String(), "level=WARN ")
		line, _, _ = strings.Cut(line, "\n")
		if c.want == "" && line+"\n" != plainUngated || c.want != "" && !strings.Contains(line, c.want) {
			t.Errorf("%s: log = %q, want %q", name, line, c.want)
		}
	}
}
