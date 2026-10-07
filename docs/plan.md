# Implementation plan

Phases are sequential; each epic is blocked by the previous one. Each epic is a GitHub issue with its tasks attached as sub-issues. Issues were created from the (now removed) `scripts/issues.json`.


## [#1](https://github.com/cybagard/cyba-headroom/issues/1) Phase 1: Spike — answer the blocking questions

Exit criterion: attribution and the Orca launch environment are confirmed to reach every agent's tool shells. Findings go to `docs/spikes/`. Spec: docs/06-phasing.md, docs/05-open-questions.md.

- [x] #6 **Spike: what does `orca worktree ps --json` return?**
- [x] #7 **Spike: can Orca set launch command/env per agent?**
- [x] #8 **Spike: does injected PATH survive in Claude CLI and Kilo tool shells?**
- [x] #9 **Spike: container memory reporting vs. VM RSS (Docker Desktop, Podman)**
- [x] #10 **Decide language and packaging (Python vs Go)**
- [ ] #11 **Spike: Kilo CLI before-tool-execution hook?**

## [#2](https://github.com/cybagard/cyba-headroom/issues/2) Phase 2: Observe — daemon, budget model, attribution, --watch (R1–R4)

Run for two weeks after completion to collect the baseline and set default thresholds. Blocked by Phase 1. Spec: docs/02-requirements.md R1–R4.

- [x] #12 **Scaffold project (build, CI, lint, tests, config dir)**
- [ ] #13 **R1: daemon core — collection loop and in-memory budget state**
- [ ] #14 **R1: Docker/Podman collector**
- [ ] #15 **R1: Tart collector**
- [ ] #16 **R1: LM Studio collector**
- [ ] #17 **R1: Orca collector**
- [ ] #18 **R1: host memory pressure and swap (no sudo) with 5-min trend**
- [ ] #19 **R2: budget model (reserved vs used vs headroom)**
- [ ] #20 **R3: worktree attribution**
- [ ] #21 **R4: observe view — `headroom` and `headroom --watch`**
- [ ] #22 **Daemon lifecycle: launchd agent, install/uninstall, logs**
- [ ] #23 **Baseline: two-week observe run, set default thresholds**

## [#3](https://github.com/cybagard/cyba-headroom/issues/3) Phase 3: Gate — launch env, shims, leases, slot gate, fail-open, policy (R5–R10)

Exit criterion: agents recover from denies without human help in most cases. Blocked by Phase 2. Spec: docs/02-requirements.md R5–R10.

- [ ] #24 **R8: policy engine (min headroom, per-worktree cap, idle-holder rule)**
- [ ] #25 **R10: leases**
- [ ] #26 **R5: shim core — real-binary resolution and passthrough**
- [ ] #27 **R5: command parser for docker/podman/tart**
- [ ] #28 **R5: identity resolution and deny/wait UX**
- [ ] #29 **R6: macOS VM slot gate**
- [ ] #30 **R7: fail-open**
- [ ] #31 **R9: Orca launch environment integration**
- [ ] #32 **R9: `headroom doctor`**
- [ ] #33 **R4: ungated detection using leases**
- [ ] #34 **Gate rollout: enable, measure deny recovery rate**

## [#4](https://github.com/cybagard/cyba-headroom/issues/4) Phase 4: Learn and advise (P1)

Blocked by Phase 3. Spec: docs/02-requirements.md (P1).

- [ ] #35 **Audit log of allow/deny/wait decisions**
- [ ] #36 **Learned cost estimates**
- [ ] #37 **Advise mode**
- [ ] #38 **Tart disk view**
- [ ] #39 **LM Studio swap counter**
- [ ] #40 **MCP tool: budget.check / budget.status**
- [ ] #41 **Claude Code PreToolUse hook (and Kilo equivalent if available)**

## [#5](https://github.com/cybagard/cyba-headroom/issues/5) Phase 5: Later (P2)

Unscheduled. Spec: docs/02-requirements.md (Future).

- [ ] #42 **Linux Tart guests**
- [ ] #43 **Placement advice for Orca remote runtimes**
- [ ] #44 **Priority ordering (foreground worktree first)**
- [ ] #45 **Full TUI (Textual)**
