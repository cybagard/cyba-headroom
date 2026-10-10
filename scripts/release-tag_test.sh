#!/usr/bin/env bash
# Tests for scripts/release-tag.sh, in a throwaway git repo. Runs on the
# host: make test-release.
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/release-tag.sh"
fail=0

# A repo with one commit on origin/main and one on a branch off it.
repo=$(mktemp -d)
trap 'rm -rf "$repo"' EXIT
cd "$repo"
git init -q
git config user.email t@example.com
git config user.name t
git commit -q --allow-empty -m on-main
on_main=$(git rev-parse HEAD)
git update-ref refs/remotes/origin/main "$on_main"
git checkout -q -b side
git commit -q --allow-empty -m off-main
off_main=$(git rev-parse HEAD)

# check: name, tag, sha, want ("fail", or the line the script must print).
# A failure must exit 1 with the script's own message, so a missing or broken
# script does not pass.
check() {
	local name=$1 tag=$2 sha=$3 want=$4 out err code
	err="$repo/.git/stderr"
	set +e
	out=$("$script" "$tag" "$sha" 2>"$err")
	code=$?
	set -e
	if [[ $want == fail ]]; then
		if [[ $code -ne 1 ]] || ! grep -q '^release-tag: ' "$err"; then
			echo "FAIL $name: exit $code, stderr '$(cat "$err")', want exit 1 and a release-tag: message"
			fail=1
			return
		fi
	elif [[ $code -ne 0 || $out != "$want" ]]; then
		echo "FAIL $name: exit $code, printed '$out', want exit 0 and '$want'"
		fail=1
		return
	fi
	echo "ok   $name"
}

# shellcheck disable=SC2016 # the tag is literal: it must not expand
check "an injection tag fails" 'v1";touch${IFS}X;"' "$on_main" fail
if [[ -e X ]]; then
	echo "FAIL the injection tag ran a command: X exists"
	fail=1
fi
check "two-part version fails" v1.2 "$on_main" fail
check "no v prefix fails" 1.2.3 "$on_main" fail
check "empty suffix fails" v1.2.3- "$on_main" fail
check "a release is not a pre-release" v1.2.3 "$on_main" false
check "a suffixed tag is a pre-release" v1.2.3-rc1 "$on_main" true
check "v0 is a pre-release" v0.2.0 "$on_main" true
check "a commit not on main fails" v1.2.3 "$off_main" fail
check "a commit on main passes" v1.2.3 "$on_main" false

exit $fail
