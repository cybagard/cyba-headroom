#!/usr/bin/env bash
# Tests for scripts/hooks/pre-commit (also run as commit-msg). Runs on the
# host: make test-hooks.
set -euo pipefail

hook="$(cd "$(dirname "$0")" && pwd)/pre-commit"
fail=0

# Fake secrets are split so this file holds none; each contains EXAMPLE.
aws="AKIA""EXAMPLE234567ABC"
ghp="ghp_""EXAMPLEaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
ant="sk-ant-""api03-EXAMPLEaaaaaaaaaaaaaaaaaaaa"
oai="sk-proj-""EXAMPLEaaaaaaaaaaaaaaaaaaaa"
slack="xoxb-""1234567890-EXAMPLEabcdef"
gapi="AIza""EXAMPLEaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
pkey="-----BEGIN ""OPENSSH PRIVATE KEY-----"
creds="postgres:/""/admin:hunter2EXAMPLE@db.internal:5432/app"

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
 {"repo": "api", "path": "/Users/x/ws/api/main", "branch": "refs/heads/main", "displayName": "main"},
 {"repo": "nested-repo", "path": "/Users/x/nested-repo/worktrees/wt-1", "branch": "refs/heads/main", "displayName": "main"}]}}
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
		action=${action//@AWS@/$aws}
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
	# Blocked as a secret, and the value itself is never echoed.
	# "secret:<text>" also wants <text> in the report.
	secret*) [[ $code == 1 && $out == *"possible secrets"* && $out != *EXAMPLE* && $out == *"${want#secret:}"* ]] && ok=1 ;;
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
check "generic folder names are ignored" allow 'stage "Attribution.Worktrees"'
check "repo under a generic folder blocked" block 'stage "see nested-repo"'
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
check "AWS access key is blocked"          secret 'stage "key = $aws"'
check "GitHub token is blocked"            secret 'stage "token: $ghp"'
check "Anthropic API key is blocked"       secret 'stage "ANTHROPIC_API_KEY=$ant"'
check "OpenAI API key is blocked"          secret 'stage "OPENAI_API_KEY=$oai"'
check "Slack token is blocked"             secret 'stage "$slack"'
check "Google API key is blocked"          secret 'stage "$gapi"'
check "private key is blocked"             secret 'stage "$pkey"'
check "password in a URL is blocked"       secret 'stage "dsn: $creds"'
check "secret in the commit message"       secret 'msg:Rotate @AWS@'
check "secret on a # line of the message"  secret 'msg:Rotate
# @AWS@'
check "secret below the scissors is ignored" allow 'msg:Remove the key
# ------------------------ >8 ------------------------
-key = @AWS@'
check "secret after a non-UTF-8 byte"      secret 'msg:Fix na'$'\xef''ve parser
key @AWS@'
check "secret in a file name"              secret 'stage "x" "keys-$aws.json"'
check "added ++ line after removed -- line" secret 'stage "-- old" f.sql && git commit -qm init --no-verify && stage "++ $aws" f.sql'
check "non-ASCII file name in the report"  secret:'in é.txt' 'stage "$aws" "é.txt"'
check "words ending in sk are not keys"    allow 'stage "--risk-assessment_threshold_default_value task-runner_configuration_value"'
check "token inside a longer identifier"   allow 'stage "base64AIzaxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx xghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"'
check "removing a secret is allowed"       allow 'stage "$aws" && git commit -qm init --no-verify && stage "rotated"'
check "look-alikes are allowed"            allow 'stage "AKIA task-ant sk-1 https://example.com/@user tcp://10.0.0.5:2375 -----BEGIN PUBLIC KEY-----"'
check "unreadable Orca output warns"       warn 'export FAKE_ORCA_BROKEN=1 && stage "fine"'

# Public names: `make hooks` copies scripts/hooks/public-names next to the
# hook, and the hook reads only that copy.
check "copied public name is allowed"      allow 'printf "# public\norca-only-repo\n" >.git/hooks/public-names && stage "github.com/o/orca-only-repo"'
check "unlisted name still blocks"         block 'printf "other-repo\n" >.git/hooks/public-names && stage "orca-only-repo"'
check "public repo's worktree name blocks" block 'printf "orca-only-repo\n" >.git/hooks/public-names && stage "dir wt-zebra"'
check "scrub-names wins over the list"     block 'printf "secret-project\norca-only-repo\n" | tee .git/hooks/public-names >>.git/info/scrub-names && stage "orca-only-repo"'
check "list in the tree but not copied"    block 'mkdir -p scripts/hooks && printf "orca-only-repo\n" >scripts/hooks/public-names && stage "orca-only-repo"'

# make hooks installs the hook and copies the list, in a scratch repo.
root=$(cd "$(dirname "$0")/../.." && pwd)
check "make hooks copies the public names" allow 'mkdir -p scripts/hooks && cp "'"$root"'"/scripts/hooks/pre-commit scripts/hooks/ && printf "orca-only-repo\n" >scripts/hooks/public-names && make -s -f "'"$root"'"/Makefile NATIVE=1 hooks >/dev/null && cmp scripts/hooks/public-names .git/hooks/public-names && stage "orca-only-repo" && PATH="$PWD/bin:$PATH" HOME=/Users/tester USER=tester .git/hooks/pre-commit'
exit $fail
