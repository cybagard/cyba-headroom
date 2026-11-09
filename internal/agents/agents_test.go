package agents_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/agents"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// fakeProcs is a process table: each session's tree, and each process's
// environment.
type fakeProcs struct {
	trees map[int][]vmproc.Process
	env   map[int]map[string]string
	// gone are processes that exit before their environment is read.
	gone map[int]bool
	// cancel, if set, runs on the first Tree.
	cancel func()
	calls  int
}

func (f *fakeProcs) Tree(root int) ([]vmproc.Process, error) {
	f.calls++
	if f.cancel != nil {
		f.cancel()
	}
	t, ok := f.trees[root]
	if !ok {
		return nil, fmt.Errorf("no process %d", root)
	}
	return t, nil
}

func (f *fakeProcs) PathOf(pid int) (string, error) {
	env, ok := f.env[pid]
	if f.gone[pid] || !ok {
		return "", errors.New("no such process")
	}
	p, ok := env["PATH"]
	if !ok {
		return "", errors.New("no PATH")
	}
	return p, nil
}

// bins makes, under a short temp dir, a shim dir whose tools link to a
// binary named headroom, and a real dir holding a real docker and tart.
func bins(t *testing.T) (shims, realDir string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	shims, realDir = filepath.Join(dir, "shims"), filepath.Join(dir, "real")
	for _, d := range []string{shims, realDir, filepath.Join(dir, "bin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	hr := filepath.Join(dir, "bin", "headroom")
	write := func(p string) {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(hr)
	for _, n := range []string{"docker", "podman", "tart"} {
		if err := os.Symlink(hr, filepath.Join(shims, n)); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(realDir, "docker"))
	write(filepath.Join(realDir, "tart"))
	return shims, realDir
}

var tools = []string{"docker", "docker-compose", "podman", "tart"}

func TestUnshimmed(t *testing.T) {
	shims, realDir := bins(t)
	shimsFirst := shims + ":" + realDir + ":/usr/bin"
	realFirst := realDir + ":" + shims + ":/usr/bin"
	const session = 100
	// login → zsh → the agent, as an Orca terminal runs it.
	tree := func(agent vmproc.Process) []vmproc.Process {
		return []vmproc.Process{
			{PID: session, Exec: "/usr/bin/login", Args: []string{"login", "-fp", "dev"}},
			{PID: 101, PPID: session, Exec: "/bin/zsh", Args: []string{"-zsh"}},
			agent,
		}
	}
	claude := vmproc.Process{PID: 102, PPID: 101, Exec: "/Users/dev/.local/share/claude/versions/2.1.0", Args: []string{"claude", "--resume"}}
	env := func(path string) map[int]map[string]string {
		return map[int]map[string]string{101: {"PATH": "/usr/bin"}, 102: {"PATH": path, "SECRET": "x"}}
	}
	for _, c := range []struct {
		name  string
		procs *fakeProcs
		want  []agents.Agent
	}{
		{"shims first", &fakeProcs{trees: map[int][]vmproc.Process{session: tree(claude)}, env: env(shimsFirst)}, nil},
		{"the realDir docker first, a version-named exec", &fakeProcs{trees: map[int][]vmproc.Process{session: tree(claude)}, env: env(realFirst)},
			[]agents.Agent{{PID: 102, Name: "claude", Tools: []string{"docker", "tart"}}}},
		{"a node-hosted agent", &fakeProcs{
			trees: map[int][]vmproc.Process{session: tree(vmproc.Process{PID: 102, PPID: 101, Exec: "/opt/node/bin/node", Args: []string{"node", "/Users/dev/lib/codex.js", "exec"}})},
			env:   env(realFirst)},
			[]agents.Agent{{PID: 102, Name: "codex", Tools: []string{"docker", "tart"}}}},
		{"no agent under the sessions", &fakeProcs{trees: map[int][]vmproc.Process{session: tree(vmproc.Process{PID: 102, PPID: 101, Exec: "/usr/bin/vim", Args: []string{"vim"}})}, env: env(realFirst)}, nil},
		{"a non-agent process with the realDir tools first", &fakeProcs{
			trees: map[int][]vmproc.Process{session: append(tree(claude), vmproc.Process{PID: 103, PPID: 101, Exec: "/usr/bin/make", Args: []string{"make"}})},
			env:   map[int]map[string]string{102: {"PATH": shimsFirst}, 103: {"PATH": realFirst}}}, nil},
		{"an agent that exited", &fakeProcs{trees: map[int][]vmproc.Process{session: tree(claude)}, env: env(realFirst), gone: map[int]bool{102: true}}, nil},
		{"an agent with no PATH", &fakeProcs{trees: map[int][]vmproc.Process{session: tree(claude)}, env: map[int]map[string]string{102: {"HOME": "/Users/dev"}}}, nil},
		{"a session that exited", &fakeProcs{trees: map[int][]vmproc.Process{}, env: env(realFirst)}, nil},
		{"tools absent from PATH", &fakeProcs{trees: map[int][]vmproc.Process{session: tree(claude)}, env: env("/usr/bin")}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := agents.Unshimmed(context.Background(), c.procs, []int{session}, shims, tools)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Unshimmed = %+v, want %+v", got, c.want)
			}
			if s := fmt.Sprintf("%#v", got); strings.Contains(s, "SECRET") || strings.Contains(s, realDir) {
				t.Fatalf("the result carries the environment: %s", s)
			}
		})
	}
}

func TestAgentNames(t *testing.T) {
	for args, want := range map[string]string{
		"claude":                           "claude",
		"/Users/dev/.local/bin/opencode x": "opencode",
		"kilo":                             "kilo",
		"node /x/codex.js":                 "codex",
		"bun /x/opencode.mjs":              "opencode",
		"/usr/bin/deno /x/kilo.cjs run":    "kilo",
		"node /x/server.js":                "",
		"node":                             "",
		"claudex":                          "",
		"-zsh":                             "",
	} {
		if got := agents.Name(strings.Fields(args)); got != want {
			t.Errorf("Name(%q) = %q, want %q", args, got, want)
		}
	}
}

func TestUnshimmedStopsOnItsContext(t *testing.T) {
	shims, realDir := bins(t)
	ctx, cancel := context.WithCancel(context.Background())
	agent := []vmproc.Process{{PID: 2, Exec: "/x/claude", Args: []string{"claude"}}}
	procs := &fakeProcs{cancel: cancel,
		trees: map[int][]vmproc.Process{1: agent, 3: agent},
		env:   map[int]map[string]string{2: {"PATH": realDir + ":" + shims}}}
	if got := agents.Unshimmed(ctx, procs, []int{1, 3}, shims, tools); got != nil || procs.calls != 1 {
		t.Fatalf("Unshimmed after a cancel = %+v after %d trees, want nothing after 1", got, procs.calls)
	}
}
