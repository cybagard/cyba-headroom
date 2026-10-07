# Implementation plan

Phases are sequential; each epic is blocked by the previous one. Each epic is a GitHub issue with its tasks attached as sub-issues. Issues were created from the (now removed) `scripts/issues.json`.


## [#1](https://github.com/cybagard/cyba-headroom/issues/1) Phase 1: Spike — answer the blocking questions

Exit criterion: attribution and the Orca launch environment are confirmed to reach every agent's tool shells. Findings go to `docs/spikes/`. Spec: docs/06-phasing.md, docs/05-open-questions.md.

- [ ] #2 **Spike: what does `orca worktree ps --json` return?**
- [ ] #3 **Spike: can Orca set launch command/env per agent?**
- [ ] #4 **Spike: does injected PATH survive in Claude CLI and Kilo tool shells?**
- [ ] #5 **Spike: container memory reporting vs. VM RSS (Docker Desktop, Podman)**
- [ ] #6 **Decide language and packaging (Python vs Go)**
- [ ] #7 **Spike: Kilo CLI before-tool-execution hook?**

## [#8](https://github.com/cybagard/cyba-headroom/issues/8) Phase 2: Observe — daemon, budget model, attribution, --watch (R1–R4)

Run for two weeks after completion to collect the baseline and set default thresholds. Blocked by Phase 1. Spec: docs/02-requirements.md R1–R4.

- [ ] #9 **Scaffold project (build, CI, lint, tests, config dir)**
- [ ] #10 **R1: daemon core — collection loop and in-memory budget state**
- [ ] #11 **R1: Docker/Podman collector**
- [ ] #12 **R1: Tart collector**
- [ ] #13 **R1: LM Studio collector**
- [ ] #14 **R1: Orca collector**
- [ ] #15 **R1: host memory pressure and swap (no sudo) with 5-min trend**
- [ ] #16 **R2: budget model (reserved vs used vs headroom)**
- [ ] #17 **R3: worktree attribution**
- [ ] #18 **R4: observe view — `headroom` and `headroom --watch`**
- [ ] #19 **Daemon lifecycle: launchd agent, install/uninstall, logs**
- [ ] #20 **Baseline: two-week observe run, set default thresholds**

## [#21](https://github.com/cybagard/cyba-headroom/issues/21) Phase 3: Gate — launch env, shims, leases, slot gate, fail-open, policy (R5–R10)

Exit criterion: agents recover from denies without human help in most cases. Blocked by Phase 2. Spec: docs/02-requirements.md R5–R10.

- [ ] #22 **R8: policy engine (min headroom, per-worktree cap, idle-holder rule)**
- [ ] #23 **R10: leases**
- [ ] #24 **R5: shim core — real-binary resolution and passthrough**
- [ ] #25 **R5: command parser for docker/podman/tart**
- [ ] #26 **R5: identity resolution and deny/wait UX**
- [ ] #27 **R6: macOS VM slot gate**
- [ ] #28 **R7: fail-open**
- [ ] #29 **R9: Orca launch environment integration**
- [ ] #30 **R9: `headroom doctor`**
- [ ] #31 **R4: ungated detection using leases**
- [ ] #32 **Gate rollout: enable, measure deny recovery rate**

## [#33](https://github.com/cybagard/cyba-headroom/issues/33) Phase 4: Learn and advise (P1)

Blocked by Phase 3. Spec: docs/02-requirements.md (P1).

- [ ] #34 **Audit log of allow/deny/wait decisions**
- [ ] #35 **Learned cost estimates**
- [ ] #36 **Advise mode**
- [ ] #37 **Tart disk view**
- [ ] #38 **LM Studio swap counter**
- [ ] #39 **MCP tool: budget.check / budget.status**
- [ ] #40 **Claude Code PreToolUse hook (and Kilo equivalent if available)**

## [#41](https://github.com/cybagard/cyba-headroom/issues/41) Phase 5: Later (P2)

Unscheduled. Spec: docs/02-requirements.md (Future).

- [ ] #42 **Linux Tart guests**
- [ ] #43 **Placement advice for Orca remote runtimes**
- [ ] #44 **Priority ordering (foreground worktree first)**
- [ ] #45 **Full TUI (Textual)**
