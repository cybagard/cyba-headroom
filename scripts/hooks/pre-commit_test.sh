#!/usr/bin/env bash
# Tests for scripts/hooks/pre-commit (also run as commit-msg). Runs on the
# host: make test-hooks.
set -euo pipefail

hook="$(cd "$(dirname "$0")" && pwd)/pre-commit"
fail=0

# setup creates a repo in $1 with a scrub list and a fake orca listing this
# repo, one other repo, and its worktrees.
setup() {
	local dir=$1
	cd "$dir"
	git init -q
	git config user.email t@example.com
	git config user.name t
	printf 'secret-project\n# a comment line\n\nalpha-internal\n' >.git/info/scrub-names
	mkdir bin
	cat >bin/orca <<'ORCA'
#!/bin/sh
[ -n "$FAKE_ORCA_BROKEN" ] && { echo '{"ok": true, "result": {"rows": []}}'; exit 0; }
cat <<JSON
{"ok": true, "result": {"worktrees": [
 {"repo": "$(basename "$PWD")", "path": "$PWD", "branch": "refs/heads/main", "displayName": "main"},
 {"repo": "orca-only-repo", "path": "/Users/x/ws/orca-only-repo/wt-zebra", "branch": "refs/heads/team/fix-billing", "displayName": "Billing Fix"},
 {"repo": "api", "path": "/Users/x/ws/api/main", "branch": "refs/heads/main", "displayName": "main"}]}}
JSON
ORCA
	chmod +x bin/orca
}

# check: name, want ("allow" | "block" | "warn"), then a command that stages
# or prepares the change; "msg:<text>" runs the hook in commit-msg mode.
check() {
	local name=$1 want=$2 action=$3 dir out code
	dir=$(mktemp -d)
	set +e
	out=$(
		set -e
		setup "$dir"
		action=${action//@REPO@/$(basename "$dir")}
		if [[ $action == msg:* ]]; then
			printf '%s\n' "${action#msg:}" >"$dir/MSG"
			PATH="$dir/bin:$PATH" HOME=/Users/tester USER=tester "$hook" "$dir/MSG" 2>&1
		else
			eval "$action"
			PATH="$dir/bin:$PATH" HOME=/Users/tester USER=tester "$hook" 2>&1
		fi
	)
	code=$?
	set -e
	rm -rf "$dir"
	local ok=0
	case $want in
	allow) [[ $code == 0 ]] && ok=1 ;;
	block) [[ $code == 1 && $out == *"internal names"* ]] && ok=1 ;;
	warn) [[ $code == 0 && $out == *"warning"* ]] && ok=1 ;;
	esac
	if [[ $ok == 1 ]]; then echo "ok   $name"; else
		echo "FAIL $name (exit $code, want $want): $out"
		fail=1
	fi
}

stage() { printf '%s\n' "$1" >"${2:-file.txt}" && git add "${2:-file.txt}"; }

check "clean change is allowed"            allow 'stage "func main() {}"'
check "name from scrub-names is blocked"   block 'stage "see secret-project for details"'
check "second name from the file"          block 'stage "alpha-internal"'
check "case variants are blocked"          block 'stage "SECRET-PROJECT"'
check "comments in scrub-names ignored"    allow 'stage "# a comment line"'
check "other repo from Orca is blocked"    block 'stage "path /ws/orca-only-repo/x"'
check "other repo's worktree name blocked" block 'stage "dir wt-zebra"'
check "other repo's branch is blocked"     block 'stage "merged team/fix-billing"'
check "other repo's display name blocked"  block 'stage "see Billing Fix"'
check "short repo names cause no false hits" allow 'stage "rapid client golang"'
check "generic worktree names are ignored" allow 'stage "checkout main"'
check "own repo name is allowed"           allow 'stage "this repo is @REPO@"'
check "own repo via origin when Orca lacks this path" allow 'git remote add origin https://github.com/o/orca-only-repo.git && stage "orca-only-repo is us"'
check "home path is blocked"               block 'stage "/Users/tester/projects"'
check "added line starting with ++ is scanned" block 'stage "++ /Users/tester/notes"'
check "file name is blocked"               block 'stage "x" "secret-project.txt"'
check "non-ASCII file name is scanned"     block 'stage "x" "secret-project-ü.txt"'
check "deleting a leaked file is allowed"  allow 'stage "x" "secret-project.txt" && git commit -qm init --no-verify && git rm -q "secret-project.txt"'
check "binary attribute does not hide content" block 'printf "*.txt binary\n" >.gitattributes && stage "secret-project"'
check "commit message is checked"          block 'msg:Port fix from secret-project'
check "clean commit message is allowed"    allow 'msg:Add the Orca collector'
check "comment lines in the message are ignored" allow 'msg:Add it
# On branch secret-project'
check "unreadable Orca output warns"       warn 'export FAKE_ORCA_BROKEN=1 && stage "fine"'
exit $fail
