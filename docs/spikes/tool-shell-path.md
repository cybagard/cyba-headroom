# Spike: does injected PATH survive in Claude CLI and Kilo tool shells?

Issue: #8 · Claude Code 2.1.292 · Kilo CLI 7.8.3 · Orca 1.4.221 · macOS · 2026-10-07

## Answer

**Yes, for both agents, as long as PATH is set in the agent's own environment at launch.** A wrapper that prepends the shim dir and then runs the agent is enough. `command -v docker` resolved to the shim in every agent tool shell tested. Rc-file edits are not needed.

## Method

The shim dir holds a fake `docker`. Each agent was launched as `env HEADROOM_WORKTREE=probe PATH=<shimdir>:$PATH <agent> …` and asked to run this in its own tool shell:

```sh
echo "docker=$(command -v docker) hw=${HEADROOM_WORKTREE:-unset} login=$([[ -o login ]] && echo y || echo n)"
```

## Result matrix

| Launch path | Tool shell | Login? | `command -v docker` | `HEADROOM_WORKTREE` | `ORCA_WORKTREE_ID` |
|---|---|---|---|---|---|
| Claude, interactive, in Orca terminal (`orca terminal create --command "env … claude"`) | `/bin/zsh -c` + snapshot | **yes** | **shim** ✅ | ✅ | ✅ |
| Claude, headless (`claude -p`) | `/bin/zsh -c` + snapshot | **yes** | **shim** ✅ | ✅ | ✅ |
| Kilo, headless (`kilo run --auto`) | `/bin/zsh -c`, parent `.kilo` | no | **shim** ✅ | ✅ | ✅ |
| *Control:* `zsh -lc` / `bash -lc` started directly under the wrapper ([#7](orca-launch-env.md)) | — | yes | `/usr/local/bin/docker` ❌ | — | — |

## Why Claude survives despite a login shell

The Claude tool shell is a login zsh, which runs `/etc/zprofile` → `path_helper`. On its own that drops the shim (see control row). But every tool command first sources a session snapshot (`~/.claude/shell-snapshots/snapshot-zsh-*.sh`), which ends with `export PATH='…'` holding the PATH the `claude` process **inherited**. That export runs after the login init, so the launch-time PATH wins.

Consequences:

- PATH is **frozen at agent start**. Installing headroom, or changing the shim dir, takes effect only for agents started afterwards. `headroom doctor` should say so.
- A user's `.zshrc` cannot accidentally remove the shim from Claude tool shells, and cannot add it later either.

## Why Kilo survives

Kilo spawns `/bin/zsh -c` (non-login) directly from the `.kilo` process with its environment, so there is no `path_helper` re-run.

## Caveats

- Kilo's interactive TUI was not tested, only `kilo run`. Both use the same tool runtime (`.kilo` binary), so the result should be the same. Re-check in the #31 integration.
- Kilo's default model is a local provider (`bionic`) that wasn't running. The test used `kilo/kilo-auto/free`.
- Agents that explicitly spawn `bash -l` / `zsh -l` *without* re-exporting PATH would lose the shim (control row). Neither tested agent does. A subprocess an agent runs (e.g. a Makefile calling `bash -l -c docker …`) still could. That is the residual ungated path R4 (#33) must detect.

## Decision for R9 (#31)

- **Use `agentCmdOverrides`** (from #7): `{"claude": "headroom run -- claude", "kilo": "headroom run -- kilo"}`. `headroom run` prepends the shim dir (and sets `HEADROOM_WORKTREE` from `ORCA_WORKTREE_ID` if unset), then `exec`s.
- An rc-file PATH edit is **not required**. Keep it optional, mainly for a human's own terminals.
- `headroom doctor` (#32): from inside an agent tool shell, check that `command -v docker`, `podman` and `tart` resolve to the shim dir. If not, report "agent started before headroom was installed, or not launched via `headroom run`".

**Phase 1 exit criterion:** attribution (`ORCA_WORKTREE_ID`) and the launch environment (wrapper-injected PATH) are confirmed to reach Claude and Kilo tool shells. ✅
