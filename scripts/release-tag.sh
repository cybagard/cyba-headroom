#!/usr/bin/env bash
# Checks a release tag before anything builds it: release-tag.sh <tag> <sha>.
# Fails unless <tag> is vX.Y.Z or vX.Y.Z-suffix and <sha> is on origin/main
# (fetch it first: a tag checkout need not have it). Prints whether the release
# is a pre-release. The tag is only matched, never run.
set -euo pipefail

die() {
	echo "release-tag: $*" >&2
	exit 1
}

[[ $# -eq 2 ]] || die "usage: release-tag.sh <tag> <sha>"
tag=$1
sha=$2

re='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'
[[ $tag =~ $re ]] || die "tag is not vX.Y.Z or vX.Y.Z-suffix"

git merge-base --is-ancestor "$sha" origin/main ||
	die "commit $sha is not on origin/main"

# A suffixed tag is a pre-release, and so is all of v0.
case $tag in
v0.* | *-*) echo true ;;
*) echo false ;;
esac
