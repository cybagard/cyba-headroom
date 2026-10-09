# Implementation plan

Phases are sequential; each epic is blocked by the previous one. Each epic is a GitHub issue with its tasks attached as sub-issues. Issues were created from the (now removed) `scripts/issues.json`.

Follow-up issues, mostly opened by reviews, are listed under each phase's tasks, by issue number. Those with no phase label are under "Follow-ups without a phase" at the end. Add each new issue here when it is opened, and tick an issue when it closes.


## [#1](https://github.com/cybagard/cyba-headroom/issues/1) Phase 1: Spike — answer the blocking questions

Exit criterion: attribution and the Orca launch environment are confirmed to reach every agent's tool shells. Findings go to `docs/spikes/`. Spec: docs/06-phasing.md, docs/05-open-questions.md.

- [x] #6 **Spike: what does `orca worktree ps --json` return?**
- [x] #7 **Spike: can Orca set launch command/env per agent?**
- [x] #8 **Spike: does injected PATH survive in Claude CLI and Kilo tool shells?**
- [x] #9 **Spike: container memory reporting vs. VM RSS (Docker Desktop, Podman)**
- [x] #10 **Decide language and packaging (Python vs Go)**
- [x] #11 **Spike: Kilo CLI before-tool-execution hook?**

Follow-ups:

- [x] #96 **Kilo hook note and lease comments: four statements to tighten**

## [#2](https://github.com/cybagard/cyba-headroom/issues/2) Phase 2: Observe — daemon, budget model, attribution, --watch (R1–R4)

Run for two weeks after completion to collect the baseline and set default thresholds. Blocked by Phase 1. Spec: docs/02-requirements.md R1–R4.

- [x] #12 **Scaffold project (build, CI, lint, tests, config dir)**
- [x] #13 **R1: daemon core — collection loop and in-memory budget state**
- [x] #14 **R1: Docker/Podman collector**
- [ ] #49 **R1: runtime autodiscovery — Docker Desktop, Podman, every VM process** (parked: needs a LuLu rule for Podman)
- [x] #59 **R1: Ollama collector**
- [x] #15 **R1: Tart collector**
- [x] #16 **R1: LM Studio collector**
- [x] #17 **R1: Orca collector**
- [x] #18 **R1: host memory pressure and swap (no sudo) with 5-min trend**
- [x] #19 **R2: budget model (reserved vs used vs headroom)**
- [x] #20 **R3: worktree attribution**
- [x] #21 **R4: observe view — `headroom` and `headroom --watch`**
- [x] #22 **Daemon lifecycle: launchd agent, install/uninstall, logs**
- [x] #55 **R1: record samples to disk for baseline and suggest**
- [x] #23 **Baseline: `headroom suggest` learns thresholds from recorded samples; two-week run**
- [ ] #63 **Baseline run: headroom suggest on the dev Mac, docs/baseline.md, defaults** (not before about 2026-10-22)

Follow-ups:

- [ ] #78 **Suggest: learn ollama_idle_gb and flag Ollama models kept loaded**
- [ ] #81 **Ollama: see a server run by another user**
- [ ] #84 **Compose: a slow or failing `compose config` leaves the lease without a key**
- [ ] #85 **Compose: `-f -` ignores COMPOSE_PROJECT_NAME from .env and --env-file**
- [ ] #87 **Lease: a held container missing from one reading loses its held state**
- [x] #88 **Lease: a denied start still renews the lease that covers it**
- [ ] #89 **Lease: a restart after a crash can bind another worktree's same-named compose lease**
- [ ] #90 **Shim: remote-context edge cases**
- [ ] #91 **Lease model: cover missing readings, restart policies and denials**
- [ ] #98 **Attribution: .git rule edge cases left after #94**
- [ ] #102 **Observe view: show each worktree's project, so primary checkouts named main can be told apart**
- [ ] #107 **Lease comments: every source of a keyless lease, and the name key's field comment**

## [#3](https://github.com/cybagard/cyba-headroom/issues/3) Phase 3: Gate — launch env, shims, leases, slot gate, fail-open, policy (R5–R10)

Exit criterion: agents recover from denies without human help in most cases. Blocked by Phase 2. Spec: docs/02-requirements.md R5–R10.

- [x] #24 **R8: policy engine (min headroom, per-worktree cap, idle-holder rule)**
- [x] #25 **R10: leases**
- [x] #26 **R5: shim core — real-binary resolution and passthrough**
- [x] #27 **R5: command parser for docker/podman/tart**
- [x] #28 **R5: identity resolution and deny/wait UX**
- [x] #29 **R6: macOS VM slot gate**
- [x] #30 **R7: fail-open**
- [x] #31 **R9: Orca launch environment integration**
- [x] #32 **R9: `headroom doctor`**
- [x] #33 **R4: ungated detection using leases**
- [ ] #34 **Gate rollout: enable, measure deny recovery rate**

Follow-ups:

- [ ] #67 **Leases: settle short-lived containers through Docker events**
- [ ] #95 **Docs: R9 says HEADROOM_WORKTREE is set for every agent, but nothing sets it**

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

## Follow-ups without a phase

- [ ] #71 **Gate: resolve named Docker contexts and Podman connections**
- [ ] #72 **Gate: FIFO queue for BUDGET_WAIT in the daemon**
- [ ] #74 **Attribution: give a container or VM the worktree of the lease it settled**
- [ ] #100 **CLAUDE.md: the smoke test should name the racing cases again**
- [ ] #104 **Loop Pick: what counts as ready (epics, parked and dated issues, Phase 5, phase order)**
