# Issue tracker: GitHub

Issues and specs for this repo live as GitHub issues in `cybagard/cyba-headroom`. Use the `gh` CLI for all operations. Never use the local-markdown tracker (`.scratch/`): a skill that falls back to it when no tracker is configured uses GitHub here.

## Conventions

- **Create an issue**: `gh issue create --title "..." --body "..."`. Use a heredoc for multi-line bodies.
- **Read an issue**: `gh issue view <number> --comments`, filtering comments by `jq` and also fetching labels.
- **List issues**: `gh issue list --state open --json number,title,body,labels,comments --jq '[.[] | {number, title, body, labels: [.labels[].name], comments: [.comments[].body]}]'` with appropriate `--label` and `--state` filters.
- **Comment on an issue**: `gh issue comment <number> --body "..."`
- **Apply / remove labels**: `gh issue edit <number> --add-label "..."` / `--remove-label "..."`
- **Close**: `gh issue close <number> --comment "..."`

Infer the repo from `git remote -v`; `gh` does this automatically when run inside a clone.

## Pull requests as a triage surface

**PRs as a request surface: no.** _(Set to `yes` if this repo treats external PRs as feature requests; `/triage` reads this flag.)_

When set to `yes`, PRs run through the same labels and states as issues, using the `gh pr` equivalents:

- **Read a PR**: `gh pr view <number> --comments` and `gh pr diff <number>` for the diff.
- **List external PRs for triage**: `gh pr list --state open --json number,title,body,labels,author,authorAssociation,comments` then keep only `authorAssociation` of `CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`, or `NONE` (drop `OWNER`/`MEMBER`/`COLLABORATOR`).
- **Comment / label / close**: `gh pr comment`, `gh pr edit --add-label`/`--remove-label`, `gh pr close`.

GitHub shares one number space across issues and PRs, so a bare `#42` may be either: resolve with `gh pr view 42` and fall back to `gh issue view 42`.

## When a skill says "publish to the issue tracker"

Create a GitHub issue.

## When a skill says "fetch the relevant ticket"

Run `gh issue view <number> --comments`.

## Wayfinding operations

Used by `/wayfinder`. The **map** is a single issue with **child** issues as tickets.

- **Map**: a single issue labelled `wayfinder:map`, holding the Notes / Decisions-so-far / Fog body. `gh issue create --label wayfinder:map`.
- **Child ticket**: an issue linked to the map as a GitHub sub-issue (`gh api` on the sub-issues endpoint). Where sub-issues aren't enabled, add the child to a task list in the map body and put `Part of #<map>` at the top of the child body. Labels: `wayfinder:<type>` (`research`/`prototype`/`grilling`/`task`). Once claimed, the ticket is assigned to the driving dev.
- **Blocking**: GitHub's **native issue dependencies**, the canonical, UI-visible representation. Add an edge with `gh api --method POST repos/cybagard/cyba-headroom/issues/<child>/dependencies/blocked_by -F issue_id=<blocker-db-id>`, where `<blocker-db-id>` is the blocker's numeric **database id** (`gh api repos/cybagard/cyba-headroom/issues/<n> --jq .id`, _not_ the `#number` or `node_id`). GitHub reports `issue_dependencies_summary.blocked_by` (open blockers only, the live gate). Where dependencies aren't available, fall back to a `Blocked by: #<n>, #<n>` line at the top of the child body. A ticket is unblocked when every blocker is closed.
- **Frontier query**: list the map's open children (`gh issue list --state open`, scoped to the map's sub-issues / task list), drop any with an open blocker (`issue_dependencies_summary.blocked_by > 0`, or an open issue in the `Blocked by` line) or an assignee; first in map order wins.
- **Claim**: `gh issue edit <n> --add-assignee @me`, the session's first write.
- **Resolve**: `gh issue comment <n> --body "<answer>"`, then `gh issue close <n>`, then append a context pointer (gist + link) to the map's Decisions-so-far.

## This repo

- The plan lives here, not in a file. Each phase is an epic issue (label `epic`), blocked by the phase before it; its tasks and follow-ups are its sub-issues, in the epic's order. List an epic's open sub-issues in that order (all pages: GitHub returns 30 by default): `gh api 'repos/cybagard/cyba-headroom/issues/<epic>/sub_issues?per_page=100' --paginate --jq '.[] | select(.state=="open") | .number'`. Attach one: `gh api --method POST repos/cybagard/cyba-headroom/issues/<epic>/sub_issues -F sub_issue_id=<db-id>`, with the database id as in **Blocking** above.
- Milestones are releases, earliest first: `v0.1.0 — observe + gate preview`, `v0.2.0 — observe complete`, `v0.3.0 — gate on`, `v0.4.0 — learn and advise`, `Later`. Every open issue in a phase carries one, alongside its phase epic and `phase:*` label; an issue in no phase carries one only if the user or the release work placed it there. A milestone closes when its tag is pushed and its release is published. List a milestone's open issues: `gh issue list --state open --milestone "<title>"`. Set one: `gh issue edit <n> --milestone "<title>"`.
- A new issue goes to GitHub only. Give it its phase's `phase:*` label, make it a sub-issue of that phase's epic, and give it the epic's release milestone, or the earliest open one if it fixes or follows up an issue there; leave one that fits no phase without any of the three, for the user to place, unless it is part of a release's own work (its release milestone only). A triage role is added only when one applies (`docs/agents/triage-labels.md`).
- Blocking edges are GitHub's native issue dependencies, read and added as in **Blocking** above; when an issue's body names a blocker, add the edge too.
- **Pick** (the Delivery loop's step 1 in `CLAUDE.md`): a pickable issue is open, not an epic, has no open blocker and no `ready-for-human` label, and its epic, if any, has no open blocker (`gh api repos/cybagard/cyba-headroom/issues/<n> --jq .parent_issue_url` names an issue's epic; `gh api repos/cybagard/cyba-headroom/issues/<epic> --jq .issue_dependencies_summary.blocked_by` counts the epic's open blockers). In the earliest open milestone with a pickable issue, take the first such sub-issue by epic (phase) order, then in its epic's sub-issue order, else the lowest-numbered such issue with no epic. If no milestone has a pickable issue, report that to the user and stop.
