package attribution_test

import (
	"testing"

	"github.com/cybagard/cyba-headroom/internal/attribution"
)

var wts = []attribution.Worktree{
	{ID: "a", Path: "/Users/dev/w/project-a"},
	{ID: "a2", Path: "/Users/dev/w/project-a2"},
	{ID: "b", Path: "/Users/dev/w/project-b"},
	{ID: "nested", Path: "/Users/dev/w/project-b/sub/wt"},
	{ID: "tmp", Path: "/tmp/scratch"},
	{ID: "m1", Path: "/Users/dev/w/one/main"},
	{ID: "m2", Path: "/Users/dev/w/two/main"},
	{ID: "ci", Path: "/Users/dev/w/ci"},
	{ID: "test", Path: "/Users/dev/w/test"},
	{ID: "case", Path: "/Users/dev/w/Fix-Login"},
}

func TestMatchPaths(t *testing.T) {
	cases := map[string]struct {
		k    attribution.Keys
		want attribution.Match
	}{
		"equal":             {attribution.Keys{Mounts: []string{"/Users/dev/w/project-a"}}, attribution.Match{WorktreeID: "a", By: attribution.ByMount}},
		"under":             {attribution.Keys{Mounts: []string{"/Users/dev/w/project-a/data/db"}}, attribution.Match{WorktreeID: "a", By: attribution.ByMount}},
		"segment boundary":  {attribution.Keys{Mounts: []string{"/Users/dev/w/project-a2/x"}}, attribution.Match{WorktreeID: "a2", By: attribution.ByMount}},
		"deepest wins":      {attribution.Keys{Mounts: []string{"/Users/dev/w/project-b/sub/wt/x"}}, attribution.Match{WorktreeID: "nested", By: attribution.ByMount}},
		"unclean path":      {attribution.Keys{Mounts: []string{"/Users/dev/w/project-a/../project-b/./x/"}}, attribution.Match{WorktreeID: "b", By: attribution.ByMount}},
		"private alias":     {attribution.Keys{Mounts: []string{"/private/tmp/scratch/x"}}, attribution.Match{WorktreeID: "tmp", By: attribution.ByMount}},
		"prefix not parent": {attribution.Keys{Mounts: []string{"/Users/dev/w/project"}}, attribution.Match{Reason: attribution.NoMatch}},
		"no keys":           {attribution.Keys{}, attribution.Match{Reason: attribution.NoMatch}},
		"relative ignored":  {attribution.Keys{Mounts: []string{"project-a"}}, attribution.Match{Reason: attribution.NoMatch}},
		// A repo's .git is shared by every linked worktree of the repo: a container
		// mounts the main checkout's so git works in a linked one (#94).
		"main .git with a linked worktree": {attribution.Keys{Mounts: []string{"/Users/dev/w/project-a", "/Users/dev/w/project-b/.git"}}, attribution.Match{WorktreeID: "a", By: attribution.ByMount}},
		// With nothing else, a .git still names its repo (a git daemon).
		".git only":                  {attribution.Keys{Mounts: []string{"/Users/dev/w/project-b/.git"}}, attribution.Match{WorktreeID: "b", By: attribution.ByMount}},
		"inside .git, with a linked": {attribution.Keys{Mounts: []string{"/Users/dev/w/project-b/.git/objects/pack", "/Users/dev/w/project-a/x"}}, attribution.Match{WorktreeID: "a", By: attribution.ByMount}},
		"linked's own .git file":     {attribution.Keys{Mounts: []string{"/Users/dev/w/project-a/.git", "/Users/dev/w/project-b/.git"}}, attribution.Match{Reason: attribution.Ambiguous}},
		"nested worktree's .git":     {attribution.Keys{Mounts: []string{"/Users/dev/w/project-b/sub/wt/.git", "/Users/dev/w/project-a"}}, attribution.Match{WorktreeID: "a", By: attribution.ByMount}},
		".GIT, trailing slash":       {attribution.Keys{Mounts: []string{"/Users/dev/w/project-b/.GIT/", "/Users/dev/w/project-a"}}, attribution.Match{WorktreeID: "a", By: attribution.ByMount}},
		// Only mounts: a compose dir inside .git still decides.
		"compose dir in .git":  {attribution.Keys{ComposeDir: "/Users/dev/w/project-b/.git/x", Mounts: []string{"/Users/dev/w/project-a"}}, attribution.Match{WorktreeID: "b", By: attribution.ByComposeDir}},
		".github still counts": {attribution.Keys{Mounts: []string{"/Users/dev/w/project-b/.github"}}, attribution.Match{WorktreeID: "b", By: attribution.ByMount}},
		"case-insensitive":     {attribution.Keys{ComposeDir: "/users/dev/w/fix-login"}, attribution.Match{WorktreeID: "case", By: attribution.ByComposeDir}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := attribution.MatchKeys(wts, tc.k); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMatchRanksEvidence(t *testing.T) {
	cases := map[string]struct {
		k    attribution.Keys
		want attribution.Match
	}{
		"compose dir beats mounts": {
			attribution.Keys{ComposeDir: "/Users/dev/w/project-a", Mounts: []string{"/Users/dev/w/project-b/x"}},
			attribution.Match{WorktreeID: "a", By: attribution.ByComposeDir},
		},
		"compose dir elsewhere falls through to mounts": {
			attribution.Keys{ComposeDir: "/opt/stack", Mounts: []string{"/Users/dev/w/project-b/x"}},
			attribution.Match{WorktreeID: "b", By: attribution.ByMount},
		},
		"mounts in two worktrees are ambiguous": {
			attribution.Keys{Mounts: []string{"/Users/dev/w/project-a/x", "/Users/dev/w/project-b/y"}},
			attribution.Match{Reason: attribution.Ambiguous},
		},
		"two mounts in one worktree": {
			attribution.Keys{Mounts: []string{"/Users/dev/w/project-a/x", "/Users/dev/w/project-a/y", "/etc/hosts"}},
			attribution.Match{WorktreeID: "a", By: attribution.ByMount},
		},
		"launch cwd beats shared dirs": {
			attribution.Keys{LaunchCwd: "/Users/dev/w/project-b", SharedDirs: []string{"/Users/dev/w/project-a"}},
			attribution.Match{WorktreeID: "b", By: attribution.ByLaunchCwd},
		},
		"shared dir": {
			attribution.Keys{LaunchCwd: "/Users/dev", SharedDirs: []string{"/Users/dev/w/project-a"}},
			attribution.Match{WorktreeID: "a", By: attribution.BySharedDir},
		},
		"vm name, whole word": {
			attribution.Keys{VMName: "ci-project-a2-mac"},
			attribution.Match{WorktreeID: "a2", By: attribution.ByVMName},
		},
		"vm name, exact": {
			attribution.Keys{VMName: "project-b"},
			attribution.Match{WorktreeID: "b", By: attribution.ByVMName},
		},
		"vm name, not a substring": {
			attribution.Keys{VMName: "myproject-amac"},
			attribution.Match{Reason: attribution.NoMatch},
		},
		"vm name shared by two worktrees is never used": {
			attribution.Keys{VMName: "main-runner"},
			attribution.Match{Reason: attribution.NoMatch},
		},
		"vm name naming two worktrees is ambiguous": {
			attribution.Keys{VMName: "project-a_project-b"},
			attribution.Match{Reason: attribution.Ambiguous},
		},
		"short dir name is never used": {
			attribution.Keys{VMName: "ci-runner-sequoia"},
			attribution.Match{Reason: attribution.NoMatch},
		},
		"generic dir name is never used": {
			attribution.Keys{VMName: "test-vm"},
			attribution.Match{Reason: attribution.NoMatch},
		},
		"paths beat the vm name": {
			attribution.Keys{VMName: "project-a", SharedDirs: []string{"/Users/dev/w/project-b"}},
			attribution.Match{WorktreeID: "b", By: attribution.BySharedDir},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := attribution.MatchKeys(wts, tc.k); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMatcherMatchesLikeMatchKeys(t *testing.T) {
	m := attribution.NewMatcher(wts)
	k := attribution.Keys{Mounts: []string{"/Users/dev/w/project-a/x"}}
	if got, want := m.Match(k), attribution.MatchKeys(wts, k); got != want || got.WorktreeID != "a" {
		t.Fatalf("matcher %+v, MatchKeys %+v", got, want)
	}
}
