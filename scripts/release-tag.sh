#!/usr/bin/env bash
# Checks a release tag before anything builds it: release-tag.sh <tag> <sha>.
# release-tag.sh --still <tag> <sha>, just before the release, fails unless the
# tag on origin still points at <sha>; it expects a tag already checked.
# Fails unless <tag> is vX.Y.Z or vX.Y.Z-suffix and <sha> is on main, which it
# fetches from origin into refs/remotes/origin/main: a tag checkout need not
# have it. Both refs are spelled out, so a tag named main or origin/main cannot
# stand in for the branch. Prints whether the release is a pre-release. The tag
# is only matched, never run.
set -euo pipefail

die() {
	echo "release-tag: $*" >&2
	exit 1
}

if [[ $# -eq 3 && $1 == --still ]]; then
	tag=$2
	sha=$3
	# An annotated tag lists its commit as refs/tags/<tag>^{}; a lightweight
	# one only as refs/tags/<tag>. GITHUB_SHA is the commit either way.
	refs=$(git ls-remote origin "refs/tags/$tag" "refs/tags/$tag^{}") ||
		die "cannot list tag $tag on origin"
	at=$(awk -v t="refs/tags/$tag" '$2 == t "^{}" { p = $1 } $2 == t { l = $1 } END { print (p != "" ? p : l) }' <<<"$refs")
	[[ $at == "$sha" ]] || die "tag $tag on origin is at '$at', not $sha"
	exit 0
fi

[[ $# -eq 2 ]] || die "usage: release-tag.sh [--still] <tag> <sha>"
tag=$1
sha=$2

re='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'
[[ $tag =~ $re ]] || die "tag is not vX.Y.Z or vX.Y.Z-suffix"

git fetch -q --no-tags origin refs/heads/main:refs/remotes/origin/main ||
	die "cannot fetch main from origin"
git merge-base --is-ancestor "$sha" refs/remotes/origin/main ||
	die "commit $sha is not on main"

# A suffixed tag is a pre-release, and so is all of v0.
case $tag in
v0.* | *-*) echo true ;;
*) echo false ;;
esac
