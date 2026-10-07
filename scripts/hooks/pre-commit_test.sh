#!/usr/bin/env bash
# Tests for scripts/hooks/pre-commit. Runs on the host: make test-hooks.
set -euo pipefail

hook="$(cd "$(dirname "$0")" && pwd)/pre-commit"
fail=0
t() { # name, expected exit (0 = commit allowed), file content
	local name=$1 want=$2 content=$3 dir
	dir=$(mktemp -d)
	(
		cd "$dir"
		git init -q
		git config user.email t@example.com
		git config user.name t
		printf 'secret-project\n# a comment line\n\nalpha-internal\n' >.git/info/scrub-names
		printf '%s\n' "${content//@REPO@/$(basename "$dir")}" >file.txt
		git add file.txt
		# Fake orca: lists this repo and one other.
		mkdir bin
		cat >bin/orca <<'ORCA'
#!/bin/sh
cat <<JSON
{"ok": true, "result": {"worktrees": [
 {"repo": "$(basename "$PWD")", "path": "$PWD"},
 {"repo": "orca-only-repo", "path": "/Users/x/ws/orca-only-repo/wt-zebra"},
 {"repo": "orca-only-repo", "path": "/Users/x/ws/orca-only-repo/main"}]}}
JSON
ORCA
		chmod +x bin/orca
		PATH="$dir/bin:$PATH" HOME=/Users/tester USER=tester "$hook" >/dev/null 2>&1
	) && got=0 || got=1
	rm -rf "$dir"
	if [[ $got == "$want" ]]; then echo "ok   $name"; else echo "FAIL $name (exit $got, want $want)"; fail=1; fi
}

t "clean change is allowed"            0 "func main() {}"
t "name from scrub-names is blocked"   1 "see secret-project for details"
t "second name from the file"          1 "alpha-internal"
t "other repo from Orca is blocked"    1 "path /ws/orca-only-repo/x"
t "other repo's worktree name blocked" 1 "branch wt-zebra"
t "generic worktree names are ignored" 0 "checkout main"
t "own repo name is allowed"           0 "this repo is @REPO@"
t "home path is blocked"               1 "/Users/tester/projects"
t "comments in scrub-names ignored"    0 "# a comment line"
exit $fail
