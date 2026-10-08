# Spike: what the gate shim costs a call

Issue: #30 · headroom built from #30's branch · Apple Silicon Mac, macOS 27 · 2026-10-08

## Answer

**The shim adds about 3 ms to a docker, podman or tart call, whether or not it is gated. When the daemon hangs, the cost is capped at the 500 ms timeout.**

| Call (40 runs; fake `docker` = a copy of `/usr/bin/true`) | Median | p95 |
|---|---|---|
| fake docker, called directly | 1.9 ms | 3.7 ms |
| through the shim, a call that starts nothing (`docker ps`) | 4.6 ms | 4.8 ms |
| through the shim, a gated call (`docker run -m 1m alpine`): config, check, lease | 4.8 ms | 5.0 ms |
| through the shim, daemon stopped (fails open) | 5.3 ms | 10.1 ms |
| through the shim, daemon hung (a socket that never accepts; fails open) | 519 ms | 521 ms |

- **Pass-through:** almost all of the ~2.7 ms is a second process start, Go runtime init plus one exec. The shim's own work is about 3.5 µs (`BenchmarkShimPassThrough`: parse plus resolve).
- **Gated:** the check adds about 0.2 ms over pass-through. In the container, `BenchmarkShimGatedCall` (config load, socket round trip, policy, lease) takes about 94 µs.
- **Fail open:** a dead daemon costs nothing noticeable, since the dial fails at once. Only a hung daemon costs the full `policy.daemon_timeout`.

So 500 ms is only a ceiling for a broken daemon, not a cost a healthy call pays. Lowering it would only shorten the hung case, which should not happen. Not worth a follow-up.

## Method

- A second daemon from the branch build ran with `HEADROOM_CONFIG_DIR` in `/tmp`.
- A shim dir with `docker → bin/headroom` came first on PATH, and the fake docker after it.
- Each call was timed in Python with `perf_counter` around `subprocess.run`.
- For the hung case, a Unix socket was bound and `listen`ed at the daemon's path but never `accept`ed.

To re-run it: build with `make build`, then follow "To smoke-test the gate" in `CLAUDE.md`.
