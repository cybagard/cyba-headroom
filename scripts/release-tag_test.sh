#!/usr/bin/env bash
# Tests for scripts/release-tag.sh, in a throwaway git repo. Runs on the
# host: make test-release.
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/release-tag.sh"
fail=0

# A bare repo as origin, with one commit on main, and a clone with one commit
# on a branch off it. The script fetches main from origin itself.
repo=$(mktemp -d)
trap 'rm -rf "$repo"' EXIT
git init -q --bare "$repo/origin.git"
git init -q "$repo/work"
cd "$repo/work"
git config user.email t@example.com
git config user.name t
git remote add origin "$repo/origin.git"
git commit -q --allow-empty -m on-main
git branch -M main
on_main=$(git rev-parse HEAD)
git push -q origin main
# The push set origin/main here; a tag checkout need not have it.
git update-ref -d refs/remotes/origin/main
git checkout -q -b side
git commit -q --allow-empty -m off-main
off_main=$(git rev-parse HEAD)

# check: name, tag, sha, want ("fail", or the line the script must print).
# A failure must exit 1 with the script's own message, so a missing or broken
# script does not pass. still: the same, for release-tag.sh --still.
check() {
	local name=$1 tag=$2 sha=$3 want=$4 out err code
	err="$repo/stderr"
	set +e
	out=$("$script" ${mode:+"$mode"} "$tag" "$sha" 2>"$err")
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
still() { mode=--still check "$@"; }

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

# A tag named like the ref must not stand in for main: neither a local tag
# origin/main (a tag checkout fetches every tag) nor a tag main on origin.
git tag origin/main "$off_main"
check "a local tag origin/main on the commit does not put it on main" v1.2.3 "$off_main" fail
git tag -d origin/main >/dev/null
git push -q origin "$off_main:refs/tags/main"
check "a tag main on origin does not put the commit on main" v1.2.3 "$off_main" fail
git push -q origin :refs/tags/main

# --still: just before the release, the tag on origin is still at the commit.
git tag v1.2.3 "$on_main"
git push -q origin v1.2.3
still "a lightweight tag at the commit passes" v1.2.3 "$on_main" ""
git tag -a -m v1.2.4 v1.2.4 "$on_main"
git push -q origin v1.2.4
still "an annotated tag at the commit passes" v1.2.4 "$on_main" ""
git push -q -f origin "$off_main:refs/tags/v1.2.3"
still "a lightweight tag moved off the commit fails" v1.2.3 "$on_main" fail
git tag -f -a -m v1.2.4 v1.2.4 "$off_main" >/dev/null
git push -q -f origin v1.2.4
still "an annotated tag moved off the commit fails" v1.2.4 "$on_main" fail
still "a tag missing from origin fails" v1.2.5 "$on_main" fail

exit $fail
