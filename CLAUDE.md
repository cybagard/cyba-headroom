# headroom

Admission control for parallel coding agents on one Mac. Spec: `docs/SPEC.md`; task list: `docs/plan.md`.

## Build and test in the devcontainer

Run every build, test, lint, fmt and tidy through `make`. Each target runs inside the devcontainer (`.devcontainer/`, image `headroom-dev`), so the host needs only Docker:

- `make test` runs `go test -race ./...`
- `make lint` runs `go vet` and `golangci-lint` for Linux and for darwin, as CI does on macOS. Run it before every commit.
- `make build` cross-compiles `bin/headroom` for darwin/arm64. Run that binary on the host for smoke tests.
- `make test-host` runs the tests as darwin test binaries on the host. They are compiled in the container and need no Go on the host.
- `make test-tart` runs the same test binaries in a clean macOS Tart VM (`headroom-mac`, a 4 GB clone of `macos-xcode`). The VM holds one of the two macOS VM slots while it runs, so the script stops it afterwards. `scripts/tart-test.sh <cmd…>` runs any command in the VM instead; the repo is at `/Volumes/My Shared Files/src`. The VM runs macOS 15 and the host a newer release, so it catches sysctls and APIs that differ between versions.
- `make dc-shell` opens a shell for one-off `go` commands.

Run `make hooks` once per clone, and again after changing `scripts/hooks/`. It copies the hook into `.git/hooks` as `pre-commit` and `commit-msg`. It is copied, not linked, so checking out a branch can't change what runs. The hook blocks staged changes and commit messages that name other projects, their worktrees or branches, or your home path, or that contain likely secrets (cloud and API keys, tokens, private keys, passwords in URLs). This repo is public. The hook reads the names from Orca and from the uncommitted `.git/info/scrub-names`, so they are never written into the repo. Write fixtures with neutral names (`project-a`, `/Users/dev`) and fake IDs (`00000000-0000-4000-8000-000000000001`), not values copied from a live capture. `make test-hooks` tests the hook.

`NATIVE=1` bypasses the container. Only CI uses it, because its macOS runners have no Docker.

## Linux container, macOS target

Tests run on Linux, but headroom ships only for macOS. So:

- Put macOS-only code (libproc, `memory_pressure`, Apple Virtualization) behind `//go:build darwin`, with a small interface that has a fake for tests. Containers can then test the logic.
- Verify macOS behaviour with `make test-host` or `make test-tart`, and by running `bin/headroom` on the host. CI's macOS runners check it again.
- Sysctls come and go between macOS releases, and so do their widths: `vm.page_wired_count` and the swapper totals are missing on macOS 15, and the page counters mix 32- and 64-bit values. Read any sysctl that isn't core as optional (nil = unknown) and accept either width. Then check it with `make test-tart`.
- To check terminal behaviour (`--watch`) without a real TTY, run it under a pty: `(sleep 8) | script -q out.log bin/headroom --watch`, and send it signals with `pkill -f '^bin/headroom --watch'` (an unanchored pattern also matches `script`). That pty reports 0 columns, so code must not treat width 0 as "not a terminal".
- The shim's flag tables are checked against the CLIs' own `--help` text in `internal/shim/testdata`. After a docker, podman or tart upgrade, recapture it (`docker run --help > docker-run.help`, …), replace your home path with `/Users/dev` before committing, and let `TestFlagTablesMatchTheCLIs` list what changed.
- To smoke-test the gate without touching the installed agent, run a second daemon from `bin/headroom` with `HEADROOM_CONFIG_DIR` set to a short `/tmp` dir, and put a shim dir with a `docker` symlink first on PATH. `HEADROOM_SHIM_DEBUG=1` shows each call's verdict and how its worktree was found.
- Socket tests need a short path under `/tmp` (`os.MkdirTemp("/tmp", "hr")`). macOS caps `sun_path` at 104 bytes, and `t.TempDir()` there is longer. This includes any test that loads a config from a temp dir, because config validation checks the default socket path in that dir. Those tests pass on Linux and fail only in `make test-host`.

## Implementation loop

The default for every issue. Each step ends when its condition holds.

1. **Pick.** Take the next open issue in phase order (`docs/plan.md`, `gh issue list`). Done when its blockers are closed.
2. **Plan.** Post the plan as a comment on the issue: interface, steps, tests, and what is out of scope. Done when the comment is posted and the user has approved it.
3. **Branch.** Create `cybagard/<issue>-<slug>` from `main`.
4. **Build test-first** with `mattpocock-skills:tdd`. Write one failing test (*red*), then the least code that passes it (*green*), then refactor. Repeat one behaviour at a time. Done when every acceptance criterion in the issue has a passing test.
5. **Verify.**
   - `make test` and `make lint` are green.
   - `make test-host` is green. Run `make test-tart` as well when the change touches macOS-only code.
   - Smoke-test `bin/headroom` on the host when the CLI or daemon behaviour changed. Use the real tools (Docker, Tart, Ollama) and the concurrent or racing cases the change is about, such as several containers in one tick. Do this before review: it finds what a reading of the diff misses.
6. **Review.** Run rounds of `/code-review` on the whole branch (`origin/main...HEAD`), and in each round a `/security-review` as well.
   - **Brief the reviewers.** Give each review the issue, the approved plan, the threat model (what is trusted, who the attacker is), and the findings already declined, with the reason for each, so they are not raised again.
   - **Verify before fixing.** Confirm each finding with a failing test, or by reading the code it names. A finding can be wrong: it may describe intended behaviour, or a bug in a test. Fix the ones that hold up, re-run `make test lint` and `make test-host`, and smoke-test again if the fix changes behaviour. Commit before the next round.
   - **Hard findings.** If a finding holds up but has no obvious fix, investigate it with `mattpocock-skills:diagnosing-bugs` before deciding. If several findings come from one design, change the design rather than adding a special case for each.
   - **Out of scope.** For a finding that belongs in another issue, open a follow-up issue and link it from the PR.
   - **Declining a finding.** Before you decline a finding or leave it open, state its consequence: what goes wrong, when, and how often. Then decide whether it hurts someone using headroom: a wrong allow or deny, a reservation that is lost or held for no reason, or a false or missing warning that users will meet. If it does, fix it, even when the fix is large. If it does not, accept it and write that reason down. Each declined or open finding goes into the PR with its consequence and the reason.
   - **Budget.** A PR gets five review rounds in all. Nothing resets the count: not a fix, not late scope, not a redesign.
   - **Done when:**
     - at least three rounds have run, and
     - the last round found no confirmed correctness or security finding, and
     - the last commit is the one that round reviewed: no fix or feature has gone in unreviewed.
   - **Budget spent with findings still open.** Do not run a sixth round.
     1. Run `mattpocock-skills:diagnosing-bugs` on every confirmed finding from all the rounds. Tally them by feature and design to find what keeps producing them.
     2. Fix what the diagnosis points to, usually a design change, and verify it with tests and a smoke test.
     3. Run one final round on the whole branch.
     4. Stop there. Whatever that round confirms goes into the PR as open, with its consequence and a follow-up issue. The user decides whether to ship or to run more rounds. Never start another round on your own.
   - **Late scope.** New behaviour added after the review started (a follow-up asked for in review, a redesign) draws on the same budget. If it would need more rounds than are left, put it in its own issue and PR.
7. **Ship.** Open a PR that closes the issue. The body covers what changed, how it was verified, and, per round, what each review found and how it was resolved. For each finding declined or left open, give its consequence and why it does not hurt users. If the budget ran out, include the diagnosis and what it changed. Name the commit the last round reviewed. Done when CI is green.
8. **Learn.** Record whatever the issue taught you that later issues need: a spike note, a new acceptance criterion on a later issue, or a line in this file.
