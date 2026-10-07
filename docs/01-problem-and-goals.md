# Problem, goals and users

## Problem statement

`headroom` is admission control for a fleet of parallel coding agents on one Mac: it shows what each agent costs across Docker/Podman, Tart and LM Studio, and it lets agents ask before they spawn more. Display alone is not the product; the gate is.

The stack: Orca runs several agents in parallel, each in its own git worktree, using Claude CLI and Kilo Code. Agents launch Docker or Podman containers and Tart macOS VMs from inside their worktree; Kilo Code sends inference to LM Studio. All of it shares 64 GB of unified memory on one M4 Max.

No layer sees the others. Orca knows an agent is "working", not that it holds 6 GB of idle containers. Docker shows one VM process, not which agent filled it. Tart has no usage view at all. When several agents build at once, the machine swaps, LM Studio's model gets evicted, every agent slows down at the same time, and nothing reports why. A third Tart runner fails outright, because macOS allows only two concurrent macOS VMs per host.

## Goals

1. **No thrash from the fleet.** Running 3+ agents in parallel no longer pushes the host into sustained swap or red memory pressure.
2. **Every resource has an owner.** Each container, VM and loaded model is attributed to an Orca worktree, or explicitly flagged as unattributed.
3. **Agents negotiate instead of failing.** When there is no room, an agent gets a reason and an alternative (reuse, stop, wait), not an out-of-memory crash or a cryptic Tart error.
4. **One glance answers "who is costing me what".** The human can see the full budget and the top consumer in under 5 seconds.
5. **Leaks get found.** Orphaned containers, VMs and Tart clones from finished or deleted worktrees are surfaced within minutes, not discovered weeks later as missing disk.

## Non-goals

- **Not a general system monitor.** `btop`, `lazydocker` and `ctop` already do host and container views well; `headroom` shows only what serves the budget question.
- **Not a security boundary.** The gate is cooperative. An agent can call the real binary or the Docker socket directly; that is detected, not prevented.
- **Not a proxy for LM Studio.** Model loads are controlled through LM Studio's own settings (single loaded model, idle TTL), not intercepted.
- **Not CI pipeline monitoring.** Queue depth, flaky retries and stage timings are a different product.
- **No remote placement.** Deciding which agents run on Orca's remote runtimes is out of scope for v1; only local resources are budgeted.
- **No Linux Tart guests yet.** macOS guests only; Linux guests are a future case.

## User stories

**Developer running the fleet (human)**

- As a developer running parallel agents, I want to see how much memory is reserved, used and free across all runtimes so that I know whether I can start another agent.
- As a developer, I want each container, VM and model attributed to the worktree that started it so that I know which agent to stop.
- As a developer, I want waiting or idle agents that still hold resources highlighted so that I can reclaim memory without hunting for it.
- As a developer, I want orphaned containers, VMs and Tart clones listed so that I can clean up leaks from finished worktrees.
- As a developer, I want to send an agent a back-off message from the same tool so that I don't switch into the Orca UI to intervene.
- As a developer, I want the gate to fail open when the daemon is down so that a crashed monitor never blocks my CI.

**Coding agent (Claude CLI, Kilo Code)**

- As an agent, I want a clear deny message with a reason and alternatives when I try to start a container that won't fit so that I can reuse, clean up or wait instead of failing.
- As an agent, I want to queue for a macOS VM slot when both are taken so that my test run starts as soon as a slot frees up.
- As an agent, I want my existing idle containers named in the deny message so that I can reuse them instead of starting new ones.

