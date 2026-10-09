# Open questions

## Open questions

**Blocking (answer before building)**

- [ ] What exactly does `orca worktree ps --json` return: PIDs, paths, agent state? Decides whether attribution can walk the process tree or must rely on paths and labels. *(engineering, spike)*
- [ ] Can Orca set a launch command or environment per agent, so PATH and `HEADROOM_WORKTREE` reach every agent? If not, does a wrapper script as the agent command work? *(engineering, spike)*
- [ ] In the tool shells Claude CLI and Kilo actually spawn, does the injected PATH survive (login vs. non-login shells, `path_helper`)? *(engineering, spike)*
- [ ] How do Docker Desktop and Podman report per-container memory on this machine, and how far does it differ from the host-side RSS of their VMs? *(engineering, measurement)*

**Non-blocking (resolve during build)**

- [ ] Default minimum headroom and per-worktree cap: set from the two-week baseline. *(data)*
- [ ] Host baseline to reserve for macOS, Orca and the agents' own processes. *(data)*
- [x] Does LM Studio's API expose loaded model memory, or only the model file size? *(engineering)* → **Only the file size.** The real cost is the footprint of LM Studio's per-model worker process (≈ size + context + runtime; 15.1 GB for a 14.4 GiB MLX model). See #16.
- [ ] Tart VM naming convention: enforce `<worktree>-<suffix>` via the shim, or attribute by process cwd only? *(engineering)*
- [x] Language and packaging: Python for speed of build, or Go for a single static binary the shims call quickly? *(engineering)* → **Go**, see [ADR 0001](adr/0001-language.md)
- [ ] Shim latency budget: the daemon check must not noticeably slow every `docker` call. *(engineering)*
- [x] Does Kilo's CLI offer a before-tool-execution hook (its `.kilo` JS plugins suggest an OpenCode-style one), and can it deny with a message to the model? Decides whether P1 agent hooks cover Kilo. *(engineering)* → **Yes.** A plugin's `tool.execute.before` hook is awaited, and if it throws, the call is blocked and the model sees the message. It must catch its own errors to fail open, and `kilo --pure` skips it. See [the spike](spikes/kilo-hook.md) (#11).

