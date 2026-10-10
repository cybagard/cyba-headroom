# headroom

Admission control for parallel coding agents on one Mac. Spec: `docs/SPEC.md`. The plan is on GitHub: one epic issue per phase, its tasks as sub-issues, and one milestone per release (`docs/agents/issue-tracker.md`, "This repo").

## Build and test in the devcontainer

Run every build, test, lint, fmt and tidy through `make`. Each target runs in the devcontainer (`.devcontainer/`, image `headroom-dev`), so the host needs only Docker. `NATIVE=1` bypasses it; only CI uses that, because its macOS runners have no Docker.

- `make test`: `go test -race ./...`.
- `make lint`: `go vet` and `golangci-lint`, for Linux and darwin, as CI does. Run it before every commit.
- `make build`: `bin/headroom` for darwin/arm64, for smoke tests on the host.
- `make test-host`: the tests as darwin binaries on the host. They are compiled in the container and need no Go on the host.
- `make test-tart`: the same binaries in a clean macOS 15 Tart VM (`headroom-mac`, a 4 GB clone of `macos-xcode`), which catches sysctls and APIs that differ from the host's release. The VM holds one of the two macOS VM slots while it runs, so the script stops it afterwards. `scripts/tart-test.sh <cmd…>` runs any command there; the repo is at `/Volumes/My Shared Files/src`.
- `make dc-shell`: a shell for one-off `go` commands.

Run `make hooks` once per clone and after changing `scripts/hooks/` (`make test-hooks` tests it). It copies the hook into `.git/hooks` as `pre-commit` and `commit-msg`; copied, not linked, so checking out a branch can't change what runs. The hook blocks commits that name other projects, their branches or worktree folders, your home path, or likely secrets. This repo is public. It reads the names from Orca and from the uncommitted `.git/info/scrub-names`, and some are ordinary words: when it blocks one, rephrase. Write fixtures with neutral names (`project-a`, `/Users/dev`) and fake IDs (`00000000-0000-4000-8000-000000000001`), not values copied from a live capture.

## Linux container, macOS target

Tests run on Linux; headroom ships only for macOS.

- Put macOS-only code (libproc, `memory_pressure`, Apple Virtualization) behind `//go:build darwin`, with a small interface that has a fake for tests. Verify macOS behaviour with `make test-host` or `make test-tart`, and by running `bin/headroom` on the host; CI's macOS runners check it again.
- Read any sysctl that isn't core as optional (nil = unknown) and accept 32- or 64-bit widths: they come and go between releases (`vm.page_wired_count` and the swapper totals are missing on macOS 15). Check with `make test-tart`.
- `--watch` without a TTY: `(sleep 8) | script -q out.log bin/headroom --watch`; signal it with `pkill -f '^bin/headroom --watch'`. That pty reports 0 columns, so width 0 must not mean "not a terminal".
- The shim's flag tables are checked against `internal/shim/testdata/*.help`. After a docker, podman or tart upgrade, recapture them (`docker run --help > docker-run.help`, …), replace your home path with `/Users/dev`, and let `TestFlagTablesMatchTheCLIs` list what changed.
- Smoke-test the gate without touching the installed agent: run a second daemon from `bin/headroom` with `HEADROOM_CONFIG_DIR` set to a short `/tmp` dir, and put a shim dir with a `docker` symlink first on PATH. `HEADROOM_SHIM_DEBUG=1` shows each verdict and how the worktree was found.
- Socket tests, and any test that loads a config from a temp dir, need a short path: `os.MkdirTemp("/tmp", "hr")`. macOS caps `sun_path` at 104 bytes; `t.TempDir()` is longer there, so such tests fail only in `make test-host`.

## Agent skills

### Issue tracker

GitHub Issues, through `gh`. See `docs/agents/issue-tracker.md`.

### Triage labels

The five default roles (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`), alongside the `phase:*` labels. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` at the root (created lazily) and `docs/adr/`. See `docs/agents/domain.md`.

## Delivery loop

The implementation loop used until 2026-10-09, which ran review rounds in one agent, is retired. Every issue goes through the loop below. Its skills are Matt Pocock's (`mattpocock-skills:*`, always by that full name: Claude Code has its own `code-review`) and Orca's orchestration. Install the plugin at user scope (`claude plugin install mattpocock-skills@claude-plugins-official --scope user`): a project-scope install covers one worktree path, so workers in a new worktree start without the skills.

**Roles.** The **coordinator** is the Orca terminal the user talks to. It plans with the user, starts and supervises **workers**, classifies findings and owns the PR. A worker is an Orca worker started with `orca orchestration worker-start --agent claude` (the skills below exist only in Claude Code): an **implementer** builds one issue in its own worktree and branch, `cybagard/<issue>-<slug>` (`worker-start --worktree new-top-level --name <issue>-<slug>` creates the branch without the prefix: rename it with `git branch -m` before the implementer commits); a **reviewer** only reviews, in the implementer's worktree (`--worktree` set to it), so the skills' `HEAD` is the branch. Load `orca skills get orchestration` before the first orchestration command of a session, and follow it for Runs, Dispatches, `check --wait`, `worker_done` and releasing workers. Each worker's spec follows Orca's Task-spec contract (target, change, constraints, ownership, observable acceptance) and is the approved plan, not a summary of it.

1. **Pick.** In the earliest open milestone, the first open issue in its epic's sub-issue order, with no open blocker and no `ready-for-human` label. Milestones, epics, sub-issues and blockers are as `docs/agents/issue-tracker.md` ("This repo") says. If it carries `needs-triage` or `needs-info`, ask the user to run `/mattpocock-skills:triage` on it (user-invoked); an issue with no triage label is ready once its blockers are closed. Independent issues may run in parallel, each in its own worktree. Done when the issue is ready.
2. **Plan.** The coordinator posts the plan on the issue: the design or rule, settled; the seams under test (`tdd` writes tests only at agreed seams); acceptance criteria; out of scope; and the change kind, *code* or *docs*. Sharpen an uncertain design with `mattpocock-skills:grilling`, and the shape of a module or seam with `mattpocock-skills:codebase-design`. Work too big for one context goes back to the user to split with `/mattpocock-skills:to-tickets` (user-invoked). Done when the user has approved it. A design change after approval returns here for approval again; review never changes the design.
3. **Build.** The implementer works test-first with `mattpocock-skills:tdd`: red, then green, one seam at a time. For stateful logic (the lease book, attribution), a failing case goes into the model test (`TestTheLeaseModel`) before the fix; when the model cannot express the case (it never denies until #91), the approved plan names the seam instead. Verify: `make test lint test-host`; `make test-tart` for macOS-only code; and a smoke test on the host with the real tools (Docker, Tart, Ollama) when CLI or daemon behaviour changed. Done when every acceptance criterion has a passing test, the checks are green, and `worker_done` carries the evidence. Before review, the coordinator re-runs that evidence itself: the new tests against `origin/main`'s code (red), and the checks on the branch (green).
4. **Review.** Each round is one reviewer running `mattpocock-skills:code-review` (the Standards and Spec axes; the spec is the issue plus the approved plan) and `/security-review` with the threat model (what is trusted, who the attacker is) on `origin/main...<branch>`. Brief every reviewer with the plan and the findings already declined. The coordinator confirms each finding against the code, or with a failing test, and classes it:
   - **Blocking:** it would hurt someone using headroom (a wrong allow or deny, a reservation lost or held for no reason, a false or missing warning, a security hole), or it contradicts the approved plan. The implementer fixes it minimally: change what the finding names, cite the source rather than restate it, add nothing else. Then re-verify and commit.
   - **Non-blocking:** everything else, such as wording, style, cleanups, or more precise prose. Not fixed in this PR. List it in the PR, and open a follow-up issue only if it is worth doing.

   **Budget:** 3 rounds for code, 1 for docs. Review ends at the first round with no blocking finding that reviewed the last commit. If the budget is spent with blocking findings open, run `mattpocock-skills:diagnosing-bugs` over every finding: tally them by feature, and build a tight loop that goes red on them. Fix the cause, run one final round, and stop. What that round confirms goes into the PR as open, each with a follow-up issue; only the user can extend the budget.
5. **Ship.** The coordinator opens the PR, which closes the issue and carries its milestone (`gh pr create --milestone "<title>"`). Its body has: **Summary** (the smallest view of the change: pseudocode, a call tree or a file tree), **Evidence** (a failing run before, a passing one after), **Merge danger** (one-way or two-way door, blast radius), and the review record: per round, the blocking findings and how each was fixed, the non-blocking ones listed, every open finding with its consequence, and the commit the last round reviewed. Done when CI is green. The user merges.
6. **Learn.** Record what later issues need: a spike note, an acceptance criterion on a later issue, an ADR, or a line in this file.
