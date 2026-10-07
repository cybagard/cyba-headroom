# headroom

Admission control for parallel coding agents on one Mac. Spec: `docs/SPEC.md`; task list: `docs/plan.md`.

## Build and test in the devcontainer

Run every build, test, lint, fmt and tidy through `make`. Each target runs inside the devcontainer (`.devcontainer/`, image `headroom-dev`), so the host needs only Docker:

- `make test` runs `go test -race ./...`
- `make lint` runs `go vet` and `golangci-lint`, which CI enforces. Run it before every commit.
- `make build` cross-compiles `bin/headroom` for darwin/arm64. Run that binary on the host for smoke tests.
- `make test-host` runs the tests as darwin test binaries on the host. They are compiled in the container and need no Go on the host.
- `make test-tart` runs the same test binaries in a clean macOS Tart VM (`headroom-mac`, a 4 GB clone of `macos-xcode`). The VM holds one of the two macOS VM slots while it runs, so the script stops it afterwards. `scripts/tart-test.sh <cmd…>` runs any command in the VM instead; the repo is at `/Volumes/My Shared Files/src`. The VM runs macOS 15 and the host a newer release, so it catches sysctls and APIs that differ between versions.
- `make dc-shell` opens a shell for one-off `go` commands.

Run `make hooks` once per clone. Its pre-commit hook blocks commits that name other projects, their worktrees or your home path. This repo is public. The hook reads the names from Orca and from the uncommitted `.git/info/scrub-names`, so they are never written into the repo. Write fixtures with neutral names (`project-a`, `/Users/dev`). `make test-hooks` tests the hook.

`NATIVE=1` bypasses the container. Only CI uses it, because its macOS runners have no Docker.

## Linux container, macOS target

Tests run on Linux, but headroom ships only for macOS. So:

- Put macOS-only code (libproc, `memory_pressure`, Apple Virtualization) behind `//go:build darwin`, with a small interface that has a fake for tests. Containers can then test the logic.
- Verify macOS behaviour with `make test-host` or `make test-tart`, and by running `bin/headroom` on the host. CI's macOS runners check it again.
- Socket tests need a short path under `/tmp` (`os.MkdirTemp("/tmp", "hr")`). macOS caps `sun_path` at 104 bytes, and `t.TempDir()` there is longer.

## Implementation loop

The default for every issue. Each step ends when its condition holds.

1. **Pick.** Take the next open issue in phase order (`docs/plan.md`, `gh issue list`). Done when its blockers are closed.
2. **Plan.** Post the plan as a comment on the issue: interface, steps, tests, and what is out of scope. Done when the comment is posted and the user has approved it.
3. **Branch.** Create `cybagard/<issue>-<slug>` from `main`.
4. **Build test-first** with `mattpocock-skills:tdd`. Write one failing test (*red*), then the least code that passes it (*green*), then refactor. Repeat one behaviour at a time. Done when every acceptance criterion in the issue has a passing test.
5. **Verify.**
   - `make test` and `make lint` are green.
   - `make test-host` is green. Run `make test-tart` as well when the change touches macOS-only code.
   - Smoke-test `bin/headroom` on the host when the CLI or daemon behaviour changed.
6. **Review.** Run `/code-review` on the branch, then `/security-review`. Fix every finding that holds up, and re-run `make test lint` after the fixes.
7. **Ship.** Open a PR that closes the issue. The body covers what changed, how it was verified, and what each review found and how it was resolved. Done when CI is green.
8. **Learn.** Record whatever the issue taught you that later issues need: a spike note, a new acceptance criterion on a later issue, or a line in this file.
