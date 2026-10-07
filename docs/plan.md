# Implementation plan

Source of truth for the GitHub issue hierarchy: [`scripts/issues.json`](../scripts/issues.json). Create issues with `scripts/create-issues.sh`. Phases are sequential; each epic is blocked by the previous one. 


## Phase 1: Spike — answer the blocking questions

Exit criterion: attribution and the Orca launch environment are confirmed to reach every agent's tool shells. Findings go to `docs/spikes/`. Spec: docs/06-phasing.md, docs/05-open-questions.md.

- [ ] **Spike: what does `orca worktree ps --json` return?** — Do PIDs, paths and agent state come back? Decides whether attribution can walk the process tree or must rely on paths and labels.
- [ ] **Spike: can Orca set launch command/env per agent?** — Can PATH and `HEADROOM_WORKTREE` reach every agent? If not, does a wrapper script as the agent command work (fallback in R9)?
- [ ] **Spike: does injected PATH survive in Claude CLI and Kilo tool shells?** — Test login vs. non-login shells and `path_helper` reordering. `command -v docker` must resolve to a shim dir for both agents.
- [ ] **Spike: container memory reporting vs. VM RSS (Docker Desktop, Podman)** — Measure how per-container memory differs from the host-side RSS of the VM process on this machine. Informs the 'used' vs 'reserved' numbers.
- [ ] **Decide language and packaging (Python vs Go)** — Non-blocking open question but needed before Phase 2 scaffolding. Shim must be fast (single static binary favours Go); consider latency budget. Record as ADR in `docs/adr/0001-language.md`.
- [ ] **Spike: Kilo CLI before-tool-execution hook?** — Does Kilo's `.kilo` JS plugin system offer a pre-tool hook that can deny with a message? Decides P1 hook coverage for Kilo.

## Phase 2: Observe — daemon, budget model, attribution, --watch (R1–R4)

Run for two weeks after completion to collect the baseline and set default thresholds. Blocked by Phase 1. Spec: docs/02-requirements.md R1–R4.

- [ ] **Scaffold project (build, CI, lint, tests, config dir)** — Repo skeleton per ADR 0001, CI workflow, `~/.config/headroom` loader, `headroom` binary entry point.
- [ ] **R1: daemon core — collection loop and in-memory budget state** — Fixed-interval loop, source plug-in interface, unix socket API for clients.
- [ ] **R1: Docker/Podman collector** — Docker-compatible API socket: per-container memory and labels; VM ceiling (`docker info` / `podman machine inspect`); host-side VM process RSS; warn when Docker Desktop and Podman VMs both run.
- [ ] **R1: Tart collector** — `tart list`: configured memory, state, disk size.
- [ ] **R1: LM Studio collector** — Loaded model and size from local API (resolve whether memory or only file size is exposed).
- [ ] **R1: Orca collector** — `orca worktree ps --json` plus agent status hooks (working/waiting/done).
- [ ] **R1: host memory pressure and swap (no sudo) with 5-min trend** — Sample every interval; keep trend history for the top line.
- [ ] **R2: budget model (reserved vs used vs headroom)** — Reserved = container VM ceiling + running Tart VM memory + loaded model + host baseline; idle models still reserved; headroom = 64 GB − reserved.
- [ ] **R3: worktree attribution** — Containers by compose label, bind mount path or process cwd; Tart by name convention or `tart run` cwd; everything else unattributed. Test: container from `fix-login` shows under `fix-login`.
- [ ] **R4: observe view — `headroom` and `headroom --watch`** — Top line (total/reserved/used/headroom/swap/pressure/trend), per-worktree rows, waiting/done-but-holding marker, unattributed row, ungated flag (needs R10 leases, stub until Phase 3).
- [ ] **Daemon lifecycle: launchd agent, install/uninstall, logs** — Run as a user LaunchAgent; `headroom daemon` subcommands.
- [ ] **Baseline: two-week observe run, set default thresholds** — Collect data; decide default min headroom, per-worktree cap and host baseline; write results to `docs/baseline.md`.

## Phase 3: Gate — launch env, shims, leases, slot gate, fail-open, policy (R5–R10)

Exit criterion: agents recover from denies without human help in most cases. Blocked by Phase 2. Spec: docs/02-requirements.md R5–R10.

- [ ] **R8: policy engine (min headroom, per-worktree cap, idle-holder rule)** — Allow/wait/deny decision from budget; config in `~/.config/headroom`; waiting/done agents holding idle resources cannot start new ones.
- [ ] **R10: leases** — Reserve estimated cost on allow until the resource appears or 2-min timeout; log expiries. Test: two 6 GB requests with 8 GB headroom → exactly one allowed.
- [ ] **R5: shim core — real-binary resolution and passthrough** — Remove own dir from PATH, never self-call, `exec` real binary, resolve `docker`→`podman` alias.
- [ ] **R5: command parser for docker/podman/tart** — Handle global flags and long forms (`--context`, `container run`, `compose -f … up`); recognise run/create/start/compose up/tart run/clone; others pass through without daemon contact.
- [ ] **R5: identity resolution and deny/wait UX** — HEADROOM_WORKTREE → git toplevel → process ancestry; no worktree = `manual`. Exit 75 with agent-oriented message naming reusable idle resources; `BUDGET_WAIT=1` queues with timeout.
- [ ] **R6: macOS VM slot gate** — Count-first gate (2 macOS VMs); name slot holders, waiting/done agents first; queue support.
- [ ] **R7: fail-open** — 500 ms daemon timeout → exec real binary with one-line warning; never block non-creating commands. Include latency benchmark.
- [ ] **R9: Orca launch environment integration** — Set shim dir first on PATH and `HEADROOM_WORKTREE` per agent using the mechanism found in the spike; wrapper-script fallback.
- [ ] **R9: `headroom doctor`** — Check PATH order and identity inside a worktree terminal; report what's wrong. Verify for Claude CLI and Kilo.
- [ ] **R4: ungated detection using leases** — Flag containers/VMs with no matching lease (direct socket, Testcontainers, broken PATH).
- [ ] **Gate rollout: enable, measure deny recovery rate** — Run two weeks with gate on; compare against baseline metrics in docs/04-metrics.md.

## Phase 4: Learn and advise (P1)

Blocked by Phase 3. Spec: docs/02-requirements.md (P1).

- [ ] **Audit log of allow/deny/wait decisions** — Record worktree, command and numbers; feeds metrics.
- [ ] **Learned cost estimates** — Record peak memory per compose project/image; estimate by p95, conservative default for unknown.
- [ ] **Advise mode** — Back-off messages via `orca terminal send` or worktree comment; manual from view or automatic on red pressure.
- [ ] **Tart disk view** — Per-clone disk usage; flag clones whose worktree is gone.
- [ ] **LM Studio swap counter** — Loads/evictions in the last hour with triggering agent where inferable.
- [ ] **MCP tool: budget.check / budget.status** — Let agents ask before planning work.
- [ ] **Claude Code PreToolUse hook (and Kilo equivalent if available)** — Advisory early warning and session-level attribution; shim stays authoritative.

## Phase 5: Later (P2)

Unscheduled. Spec: docs/02-requirements.md (Future).

- [ ] **Linux Tart guests** — Outside two-VM limit; memory-gated like containers.
- [ ] **Placement advice for Orca remote runtimes** — Which worktrees should move given local headroom.
- [ ] **Priority ordering (foreground worktree first)** — Human's foreground worktree gets precedence in the policy.
- [ ] **Full TUI (Textual)** — After `--watch` shows which columns matter.
