# ADR 0001: Language and packaging

Status: accepted · 2026-10-07 · Issue #10

## Context

Every `docker`, `podman` and `tart` call from every agent goes through the shim (R5). The shim asks the daemon over a local socket, then `exec`s the real binary. It must not noticeably slow those calls. Open question in docs/05-open-questions.md: Python for build speed, or Go for a single static binary.

## Measurement

Apple Silicon Mac, 200 runs each. Prototype shim = connect to a Unix socket, send one line, read the reply, `exec` `/usr/bin/true`. Stub daemon = Python `socketserver` replying `allow`. Harness: `subprocess.run` wall time.

| Case | Median | p95 | Overhead vs. bare exec |
|---|---|---|---|
| `/usr/bin/true` (bare exec) | 1.4 ms | 1.6 ms | — |
| **Go shim** (2.0 MB, `-ldflags="-s -w"`) | **3.2 ms** | **3.6 ms** | **+1.8 ms** |
| Python 3.14 shim | 16.1 ms | 17.2 ms | +14.7 ms |
| Python 3.14 shim, `-I -S` | 13.6 ms | 14.5 ms | +12.2 ms |
| `docker --version`, for scale | 10.1 ms | 10.9 ms | — |

A Python shim costs more than an entire `docker --version`. Agents run short read-only commands (`docker ps`, `docker logs`, `docker inspect`) in loops, so a 15 ms tax on each would be visible. Go's 2 ms is not.

## Decision

**Go, one binary for everything.**

- `headroom` is a multi-call binary. The daemon (`headroom daemon`), CLI (`headroom`, `--watch`, `doctor`, `run`) and the shims are all the same binary. Shims are symlinks named `docker` / `podman` / `tart` in the shim dir, dispatched on `argv[0]`.
- The shim path stays minimal: no config parsing beyond what a decision needs, a 500 ms dial timeout (R7 fail-open), then `syscall.Exec`.
- Packaging: Homebrew tap built from source, plus `go install` for development. The launchd plist is installed by `headroom install` (#22).

## Consequences

- No Python runtime dependency on the host. Shim behaviour does not depend on which `python3` is first on PATH, which matters because the shim is what *changes* PATH.
- **Unix socket path limit:** macOS caps `sun_path` at 104 bytes. The prototype failed to bind under a long temp dir. Put the socket at a short, fixed path, e.g. `~/.config/headroom/d.sock`. Fall back to a loopback TCP port if the path would overflow.
- Phase 5's "Full TUI (Textual)" (#45) assumed Python. Re-scope it to a Go TUI (Bubble Tea) or a separate client over the daemon socket.
- The MCP tool (#40) uses the official Go MCP SDK, or is served by the daemon.
- Collectors call the Docker/Podman API over their sockets and shell out to `tart`, `lms` and `orca` (`--json`). Nothing there favours Python.

## Alternatives considered

- **Python everywhere:** fastest to build, but fails the shim latency budget. Ruled out.
- **Go shim + Python daemon:** keeps the hot path fast, but means two toolchains, two packaging stories and a socket protocol across languages for no real gain. Ruled out.
- **Rust:** comparable latency, slower to build. No advantage over Go here.
