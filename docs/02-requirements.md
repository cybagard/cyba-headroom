# Requirements

### Must-have (P0)

**R1 — Collector daemon.** One background process reads all sources on a fixed interval and holds the current budget.

- [ ] Reads Docker Desktop or Podman via the Docker-compatible API socket, including per-container memory and labels
- [ ] Reads the container VM ceiling (`docker info` or `podman machine inspect`) and the host-side RSS of the VM process
- [ ] Reads Tart VMs (`tart list`) with configured memory, state and disk size
- [ ] Reads LM Studio's loaded model and size from its local API
- [ ] Reads Orca worktrees and agent state (working / waiting / done) via `orca worktree ps --json` and the agent status hooks
- [ ] Reads host memory pressure and swap without sudo
- [ ] If Docker Desktop and Podman VMs are both running, flags it as a warning

**R2 — Budget model.** The budget is reserved vs. used vs. headroom, not free memory.

- [x] Reserved = container VM reservation + configured memory of running Tart VMs + loaded model size + host baseline. The container VM reservation is `max(VM footprint, Σ containers + VM overhead)`, not the VM's memory limit: spike #9 showed the limit (31 GiB) would leave no headroom (#19)
- [x] A loaded model counts as reserved even when idle: `max(idle footprint + Σ model file sizes, LM Studio footprint)` (#19)
- [ ] Headroom = host memory (64 GB here) minus reserved, shown next to memory pressure and swap (computed in #19; shown by #21)
- [x] Memory in use that no component accounts for is reported, so the host baseline can be learned from samples (#19, #55, #23)

**R3 — Worktree attribution.** The worktree path is the join key.

- [x] Containers attributed by compose project label or bind-mount path (#20). Launching process cwd needs the shim's labels or leases (#26, #28, #25)
- [x] Tart VMs attributed by naming convention (VM name contains the worktree's directory name as whole words; the name must be unique among live worktrees, at least four characters, and not a generic word such as `main` or `test`), by the cwd of the process that launched `tart run`, or by its `--dir` host paths. (`tart run` itself changes cwd to the VM bundle, so its own cwd says nothing; see #15.)
- [x] Anything that matches no live worktree, or more than one, appears under **unattributed**, with the reason
- [x] Given a container started from worktree `fix-login`, when the view refreshes, then it appears under `fix-login` (attribution, #20; view, #21)

**R4 — Observe view.** `headroom` prints once; `headroom --watch` refreshes in place.

- [x] Top line: host total, reserved, used, headroom, swap, pressure, plus the 5-minute pressure trend (#21; headroom and pressure lead, so a narrow terminal keeps them)
- [x] One row per worktree: agent, state, containers + GB, Tart VMs + GB, CPU, and the agents' own memory (#21). Worktrees with no agent and nothing running are hidden unless `--all` is given
- [x] Rows where the agent is waiting or done, or there is no agent, but the worktree still holds resources are marked ⚑ (#21)
- [x] Unattributed row always shown when non-empty, with each item's reason (#21)
- [ ] Containers or VMs that appear without a matching lease (R10) are flagged as **ungated** (#33): direct socket use, SDKs such as Testcontainers, or a broken PATH

**R5 — Gate shim for `docker`, `podman` and `tart` (authoritative).** One global shim directory, put first on PATH by the Orca launch environment (R9). It is the only enforcement point; agent hooks and MCP are advisory.

- [ ] Finds the real binary by removing its own directory from PATH; never calls itself
- [ ] Parses global flags and long forms (`docker --context x run`, `docker container run`, `docker compose -f a.yml up`) to recognize resource-creating calls: `run`, `create`, `start`, `compose up`, `tart run`, `tart clone`
- [ ] Everything else passes straight through without contacting the daemon
- [ ] Resolves identity in order: `HEADROOM_WORKTREE`, git toplevel of the cwd, process ancestry; calls with no Orca worktree (a human typing `docker`) pass through and are attributed as **manual**
- [ ] On allow, replaces itself with the real binary (`exec`), so TTY, signals and exit codes behave as if the shim were not there
- [ ] On deny, exits 75 (temporary failure) with a message written for an agent: headroom, estimated cost, and the agent's own idle resources it could reuse or stop
- [ ] `BUDGET_WAIT=1` queues the request instead of denying, with a timeout
- [ ] Resolves `docker` aliased to `podman` to the real target

**R6 — macOS VM slot gate.** Tart is gated on count first, memory second.

- [ ] Given two macOS VMs running, when a third `tart run` is requested, then the shim denies or queues it with the names of the two slot holders
- [ ] Slots held by waiting or done agents are named first in the message

**R7 — Fail open.**

- [ ] If the daemon is unreachable within 500 ms, the shim execs the real binary and prints a one-line warning
- [ ] The gate never blocks a command that does not create resources

**R8 — Fixed-threshold policy.**

- [ ] Configurable minimum headroom (default to be set after measurement)
- [ ] Configurable per-worktree cap in GB
- [ ] A waiting or done agent cannot start new resources while it holds idle ones
- [ ] Config in `~/.config/headroom`

**R9 — Orca launch environment.** Every agent Orca launches starts with the shim directory first on PATH and `HEADROOM_WORKTREE` set. Variables set in the agent's own process reach its tool shells, which is more reliable than direnv or `.zshenv`.

- [ ] Set through Orca's agent launch command or environment; fallback: a worktree setup hook that writes a small wrapper script used as the agent command
- [ ] Given an agent launched by Orca, when it runs a tool command in a non-interactive shell, then `command -v docker` resolves to the shim
- [ ] Verified separately for Claude CLI and Kilo Code tool shells, including login shells where `path_helper` reorders PATH
- [ ] `headroom doctor`, run inside a worktree terminal, checks PATH order and identity and says what is wrong

**R10 — Leases.** An allow reserves the estimated cost until the resource appears, so simultaneous requests cannot overcommit.

- [x] Given two agents each request 6 GB with 8 GB headroom at the same moment, then exactly one is allowed and the other waits or is denied (#25)
- [x] A lease turns into tracked usage when its container or VM appears, or expires after a timeout (default 2 minutes) (#25). The daemon recognises the new resource itself; the shim cannot report back once it has exec'd
- [x] Expired leases are logged (#25)

### Nice-to-have (P1)

- **Learned cost estimates.** Record peak memory per compose project and image; estimate new requests by p95 of past runs, conservative default for unknown ones.
- **Agent hooks (early warning).** Claude Code's PreToolUse hook, and Kilo's equivalent if it has one, ask the daemon before a command runs, deny obvious cases with the reason fed straight to the model, and add session-level attribution. Advisory only: they see the command string the model typed, so `make ci` or a script that calls `docker` passes unseen. The shim stays authoritative.
- **Advise mode.** Send back-off messages to agents via `orca terminal send` or a worktree comment, manually from the view or automatically on red pressure.
- **Tart disk view.** Disk usage per Tart clone, flagging clones whose worktree no longer exists.
- **LM Studio swap counter.** Model loads and evictions in the last hour, with which agent's request triggered them where inferable.
- **MCP tool.** `budget.check` / `budget.status` so agents can ask before planning work.
- **Audit log.** Every allow / deny / wait decision with worktree, command and numbers.

### Future (P2)

- Linux Tart guests (outside the two-macOS-VM limit; memory-gated like containers)
- Placement advice: which worktrees should move to an Orca remote runtime given local headroom
- Priority ordering, e.g. the human's foreground worktree first
- Full TUI (Textual) once the `--watch` view has proven which columns matter

