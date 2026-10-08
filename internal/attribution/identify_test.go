package attribution

import (
	"testing"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

func TestIdentify(t *testing.T) {
	s := &protocol.Snapshot{Orca: &protocol.Orca{Running: true, Worktrees: []protocol.Worktree{
		{ID: "repo::/Users/dev/src/project-a", Path: "/Users/dev/src/project-a", SessionPIDs: []int{100}},
		{ID: "repo::/Users/dev/src/project-a/nested", Path: "/Users/dev/src/project-a/nested", SessionPIDs: []int{200}},
		{ID: "repo::/tmp/project-b", Path: "/tmp/project-b", SessionPIDs: []int{300}},
	}}}
	for _, c := range []struct {
		name   string
		in     Caller
		id, by string
	}{
		{"env wins", Caller{Worktree: "repo::/elsewhere", Cwd: "/Users/dev/src/project-a"}, "repo::/elsewhere", protocol.IdentifiedByCaller},
		{"cwd in a worktree", Caller{Cwd: "/Users/dev/src/project-a/cmd"}, "repo::/Users/dev/src/project-a", protocol.IdentifiedByCwd},
		{"deepest worktree", Caller{Cwd: "/Users/dev/src/project-a/nested/x"}, "repo::/Users/dev/src/project-a/nested", protocol.IdentifiedByCwd},
		{"private alias", Caller{Cwd: "/private/tmp/project-b"}, "repo::/tmp/project-b", protocol.IdentifiedByCwd},
		{"case", Caller{Cwd: "/users/DEV/src/project-a"}, "repo::/Users/dev/src/project-a", protocol.IdentifiedByCwd},
		{"ancestor", Caller{Cwd: "/tmp/elsewhere", Ancestors: []int{900, 300, 1}}, "repo::/tmp/project-b", protocol.IdentifiedByProcess},
		{"nearest ancestor", Caller{Ancestors: []int{200, 100}}, "repo::/Users/dev/src/project-a/nested", protocol.IdentifiedByProcess},
		{"cwd before ancestry", Caller{Cwd: "/tmp/project-b", Ancestors: []int{100}}, "repo::/tmp/project-b", protocol.IdentifiedByCwd},
		{"manual", Caller{Cwd: "/Users/dev", Ancestors: []int{900, 1}}, "", ""},
		{"relative cwd", Caller{Cwd: "project-a"}, "", ""},
		{"through a symlink", Caller{Cwd: "/Users/dev/work/x", RealCwd: "/Users/dev/src/project-a/x"}, "repo::/Users/dev/src/project-a", protocol.IdentifiedByCwd},
		{"spelled as Orca does", Caller{Cwd: "/tmp/project-b", RealCwd: "/Volumes/data/project-b"}, "repo::/tmp/project-b", protocol.IdentifiedByCwd},
	} {
		id, by := Identify(s, c.in)
		if id != c.id || by != c.by {
			t.Errorf("%s: got %q by %q, want %q by %q", c.name, id, by, c.id, c.by)
		}
	}
	// Without Orca, only the environment can say.
	if id, by := Identify(&protocol.Snapshot{}, Caller{Cwd: "/tmp/project-b", Ancestors: []int{300}}); id != "" || by != "" {
		t.Errorf("no Orca: %q by %q", id, by)
	}
	if id, by := Identify(nil, Caller{Worktree: "w"}); id != "w" || by != protocol.IdentifiedByCaller {
		t.Errorf("nil snapshot, env: %q by %q", id, by)
	}
}
