#!/usr/bin/env bash
# Creates epics and sub-issues from scripts/issues.json using the gh CLI.
# Needs: gh (authenticated, with issues:write), jq. Run once; not idempotent.
set -euo pipefail
cd "$(dirname "$0")"
REPO="${REPO:-cybagard/cyba-headroom}"
mklabel() { gh label create "$1" --repo "$REPO" --force >/dev/null; }
for l in epic spike feature chore decision data; do mklabel "$l"; done
n=$(jq length issues.json)
for i in $(seq 0 $((n-1))); do
  elabel=$(jq -r ".[$i].label" issues.json); mklabel "$elabel"
  etitle=$(jq -r ".[$i].title" issues.json)
  ebody=$(jq -r ".[$i].body" issues.json)
  eurl=$(gh issue create --repo "$REPO" --title "$etitle" --body "$ebody" --label epic --label "$elabel")
  enum=${eurl##*/}
  echo "epic #$enum: $etitle"
  m=$(jq ".[$i].subs | length" issues.json)
  for j in $(seq 0 $((m-1))); do
    st=$(jq -r ".[$i].subs[$j].title" issues.json)
    sb=$(jq -r ".[$i].subs[$j].body" issues.json)
    args=(--label "$elabel")
    while read -r l; do args+=(--label "$l"); done < <(jq -r ".[$i].subs[$j].labels[]" issues.json)
    surl=$(gh issue create --repo "$REPO" --title "$st" --body "$sb" "${args[@]}")
    snum=${surl##*/}
    sid=$(gh api "repos/$REPO/issues/$snum" --jq .id)
    gh api -X POST "repos/$REPO/issues/$enum/sub_issues" -F sub_issue_id="$sid" >/dev/null
    echo "  #$snum: $st"
  done
done
