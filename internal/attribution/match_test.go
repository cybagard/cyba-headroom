package attribution_test

import (
	"slices"
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
		"equal":                {attribution.Keys{Mounts: []string{"/Users/dev/w/project-a"}}, attribution.Match{WorktreeID: "a", By: attribution.ByMount}},
		"under":                {attribution.Keys{Mounts: []string{"/Users/dev/w/project-a/data/db"}}, attribution.Match{WorktreeID: "a", By: attribution.ByMount}},
		"segment boundary":     {attribution.Keys{Mounts: []string{"/Users/dev/w/project-a2/x"}}, attribution.Match{WorktreeID: "a2", By: attribution.ByMount}},
		"deepest wins":         {attribution.Keys{Mounts: []string{"/Users/dev/w/project-b/sub/wt/x"}}, attribution.Match{WorktreeID: "nested", By: attribution.ByMount}},
		"unclean path":         {attribution.Keys{Mounts: []string{"/Users/dev/w/project-a/../project-b/./x/"}}, attribution.Match{WorktreeID: "b", By: attribution.ByMount}},
		"private alias":        {attribution.Keys{Mounts: []string{"/private/tmp/scratch/x"}}, attribution.Match{WorktreeID: "tmp", By: attribution.ByMount}},
		"prefix not parent":    {attribution.Keys{Mounts: []string{"/Users/dev/w/project"}}, attribution.Match{Reason: attribution.NoMatch}},
		"no keys":              {attribution.Keys{}, attribution.Match{Reason: attribution.NoMatch}},
		"relative ignored":     {attribution.Keys{Mounts: []string{"project-a"}}, attribution.Match{Reason: attribution.NoMatch}},
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

// A repo's .git is shared by every linked worktree of the repo: a container
// of a linked one mounts the main checkout's so git works (#94). IDs are
// <repoId>::<path>, as Orca's.
func TestGitMounts(t *testing.T) {
	gwts := []attribution.Worktree{
		{ID: "r1::main", Path: "/Users/dev/r1"},
		{ID: "r1::feat", Path: "/Users/dev/wt/r1-feat"},
		{ID: "r2::main", Path: "/Users/dev/r2"},
		{ID: "r2::odd", Path: "/Users/dev/r2/.git/wt/odd"},
	}
	feat := attribution.Match{WorktreeID: "r1::feat", By: attribution.ByMount}
	r1 := attribution.Match{WorktreeID: "r1::main", By: attribution.ByMount}
	amb := attribution.Match{Reason: attribution.Ambiguous}
	for name, tc := range map[string]struct {
		k    attribution.Keys
		want attribution.Match
	}{
		"main .git with its linked":            {attribution.Keys{Mounts: []string{"/Users/dev/wt/r1-feat", "/Users/dev/r1/.git"}}, feat},
		"inside .git with its linked":          {attribution.Keys{Mounts: []string{"/Users/dev/r1/.git/objects/pack", "/Users/dev/wt/r1-feat/x"}}, feat},
		".GIT/ with its linked":                {attribution.Keys{Mounts: []string{"/Users/dev/r1/.GIT/", "/Users/dev/wt/r1-feat"}}, feat},
		"nested repo's .git":                   {attribution.Keys{Mounts: []string{"/Users/dev/r1/tools/sub/.git", "/Users/dev/wt/r1-feat"}}, feat},
		".git only":                            {attribution.Keys{Mounts: []string{"/Users/dev/r1/.git"}}, r1},
		"one repo's .git twice":                {attribution.Keys{Mounts: []string{"/Users/dev/r1/.git", "/Users/dev/r1/.git/objects"}}, r1},
		"two repos' .git only":                 {attribution.Keys{Mounts: []string{"/Users/dev/r1/.git", "/Users/dev/r2/.git"}}, amb},
		"a .git with another repo's":           {attribution.Keys{Mounts: []string{"/Users/dev/r2/.git", "/Users/dev/wt/r1-feat"}}, amb},
		"another repo's .git with data":        {attribution.Keys{Mounts: []string{"/Users/dev/r2/.git", "/Users/dev/r1/data"}}, amb},
		"worktree inside a .git":               {attribution.Keys{Mounts: []string{"/Users/dev/r2/.git/wt/odd", "/Users/dev/r2/x"}}, amb},
		"two .git of one repo, only":           {attribution.Keys{Mounts: []string{"/Users/dev/r1/.git", "/Users/dev/wt/r1-feat/sub/.git"}}, amb},
		"the same, reversed":                   {attribution.Keys{Mounts: []string{"/Users/dev/wt/r1-feat/sub/.git", "/Users/dev/r1/.git"}}, amb},
		".github counts":                       {attribution.Keys{Mounts: []string{"/Users/dev/r1/.github", "/Users/dev/wt/r1-feat"}}, amb},
		"shared dirs too":                      {attribution.Keys{SharedDirs: []string{"/Users/dev/r1/.git", "/Users/dev/wt/r1-feat"}}, attribution.Match{WorktreeID: "r1::feat", By: attribution.BySharedDir}},
		"own .git with own data":               {attribution.Keys{Mounts: []string{"/Users/dev/r1/.git", "/Users/dev/r1/data"}}, r1},
		"shared dirs, another repo":            {attribution.Keys{SharedDirs: []string{"/Users/dev/r2/.git", "/Users/dev/wt/r1-feat"}}, amb},
		"launch cwd in .git decides":           {attribution.Keys{LaunchCwd: "/Users/dev/r1/.git/hooks"}, attribution.Match{WorktreeID: "r1::main", By: attribution.ByLaunchCwd}},
		"compose dir in .git decides":          {attribution.Keys{ComposeDir: "/Users/dev/r1/.git/x", Mounts: []string{"/Users/dev/wt/r1-feat"}}, attribution.Match{WorktreeID: "r1::main", By: attribution.ByComposeDir}},
		"ids without a repo: no setting aside": {attribution.Keys{Mounts: []string{"/Users/dev/w/project-a", "/Users/dev/w/project-b/.git"}}, amb},
	} {
		t.Run(name, func(t *testing.T) {
			all := append(slices.Clone(gwts), wts...)
			if got := attribution.MatchKeys(all, tc.k); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
