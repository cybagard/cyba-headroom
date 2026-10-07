# headroom

Admission control for parallel coding agents on one Mac. Spec: `docs/SPEC.md`; task list: `docs/plan.md`.

## Build and test in the devcontainer

Run every build, test, lint, fmt and tidy through `make`. Each target runs inside the devcontainer (`.devcontainer/`, image `headroom-dev`), so the host needs only Docker:

- `make test` runs `go test -race ./...`
- `make lint` runs `go vet` and `golangci-lint`, which CI enforces. Run it before every commit.
- `make build` cross-compiles `bin/headroom` for darwin/arm64. Run that binary on the host for smoke tests.
- `make test-host` runs the tests as darwin test binaries on the host. They are compiled in the container and need no Go on the host.
- `make test-tart` runs the same test binaries in a clean macOS Tart VM (`headroom-mac`, a 4 GB clone of `macos-xcode`). The VM holds one of the two macOS VM slots while it runs, so the script stops it afterwards.
- `make dc-shell` opens a shell for one-off `go` commands.

`NATIVE=1` bypasses the container. Only CI uses it, because its macOS runners have no Docker.

## Linux container, macOS target

Tests run on Linux, but headroom ships only for macOS. So:

- Put macOS-only code (libproc, `memory_pressure`, Apple Virtualization) behind `//go:build darwin`, with a small interface that has a fake for tests. Containers can then test the logic.
- Verify macOS behaviour with `make test-host` or `make test-tart`, and by running `bin/headroom` on the host. CI's macOS runners check it again.
- Socket tests need a short path under `/tmp` (`os.MkdirTemp("/tmp", "hr")`). macOS caps `sun_path` at 104 bytes, and `t.TempDir()` there is longer.
