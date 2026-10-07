# Spike: can Orca set launch command/env per agent?

Issue: #7 · Orca 1.4.221 · macOS · 2026-10-07

## Answer

**Yes, there are three mechanisms, and worktree identity is already there for free.**

| Need | Mechanism | Status |
|---|---|---|
| Worktree identity in every agent | Orca exports `ORCA_WORKTREE_ID=<repoId>::<path>` (plus `ORCA_PANE_KEY`, `ORCA_TAB_ID`, `ORCA_TERMINAL_HANDLE`) into every terminal | **Verified**: present in the agent and in Claude's tool shell |
| Extra env per agent type | Settings → `agentDefaultEnv` (`{ "<agent>": { "VAR": "value" } }`) | Exists (`goose` ships with `GOOSE_MODE=auto`). Not exercised; see caveat |
| Wrap the agent command | Settings → `agentCmdOverrides` (`{ "<agent>": "<command>" }`) | Exists. Plus `orca terminal create --command` and `worktree create --agent` |
| Wrapper-script fallback (R9) | Wrapper exports vars, prepends shim dir, then `exec`s the agent | **Verified** via `orca terminal create --command <wrapper>` |

So `HEADROOM_WORKTREE` is unnecessary: use `ORCA_WORKTREE_ID` directly, and treat `HEADROOM_WORKTREE` as an optional override for non-Orca launches.

## How Orca launches an agent

From the app bundle (`out/shared/agent-startup-plan-inputs.js`, `tui-agent-startup.js`, renderer):

- The startup plan is built from `agentCmdOverrides[agent]` (or the default binary) plus `agentDefaultArgs[agent]`. It becomes `launchCommand`, with `agentDefaultEnv[agent]` as `env`.
- The terminal is created with `{ command: launchCommand, env }`. The PTY host spawns a **login shell** (`login → -zsh`), and the command is **typed into that shell**.
- Orca's shell wrappers (`$ORCA_USER_DATA_PATH/shell-wrappers/*/shell-ready/`) source `/etc/profile` / the user's dotfiles, **then re-prepend** Orca's own dirs (`ORCA_CLI_BIN_DIR`, `ORCA_AGENT_TEAMS_SHIM_DIR`). Orca does this because user rc files and `path_helper` clobber PATH set through spawn env. **Implication:** a PATH set only through `agentDefaultEnv` will likely lose to rc/`path_helper` the same way. A non-PATH var like `HEADROOM_*` survives.

## Experiment

`orca terminal create --worktree active --command <scratch>/probe.sh`. The probe exports `PATH=<shimdir>:$PATH`, defines a fake `docker` in `<shimdir>`, then checks child shells:

| Shell started under the wrapper | `command -v docker` | `ORCA_WORKTREE_ID` |
|---|---|---|
| wrapper itself | shim ✅ | ✅ |
| `zsh -c` (non-login) | shim ✅ | ✅ |
| `bash -c` (non-login) | shim ✅ | — |
| `zsh -lc` (login) | `/usr/local/bin/docker` ❌ | — |
| `bash -lc` (login) | `/usr/local/bin/docker` ❌ | — |

**Login shells drop the shim** (`/etc/zprofile` runs `path_helper`, and user dotfiles re-order PATH).

Claude Code's tool shell **is a login zsh** (`[[ -o login ]]` is true). It also sources a shell snapshot (`~/.claude/shell-snapshots/snapshot-zsh-*.sh`) that ends with a hard-coded `export PATH='…'` captured when the session started. Whether the shim survives therefore depends on how that snapshot is captured. That's the core of #8 and is not settled here.

## Recommendation for R9 (#31)

1. **Identity:** rely on `ORCA_WORKTREE_ID`. No Orca config needed.
2. **PATH:** don't depend on spawn env alone. Pick one of these, in order of preference, once #8 decides:
   - a. `agentCmdOverrides`: `{"claude": "headroom run -- claude", "kilo": "headroom run -- kilo"}`. `headroom run` prepends the shim dir and `exec`s. Survives if the agent's tool shells inherit rather than rebuild PATH.
   - b. Prepend the shim dir in the user's `~/.zshrc` / `~/.zprofile`, guarded by `[[ -n $ORCA_WORKTREE_ID ]]`. This survives login shells, since Orca's own re-prepend follows the same pattern. `headroom doctor` (#32) can check for it.
   - c. Both: a for non-shell launches, b as belt and braces.
3. `headroom doctor` should verify end to end: from inside an agent's tool shell, `command -v docker` resolves to the shim dir.

## Open (hand to #8)

- Does Claude's snapshot capture PATH from the parent env or from a fresh login shell? Test: launch `claude` via wrapper (option a) and run `command -v docker` in a tool call.
- Kilo's tool-shell mode (login or not).
- Whether `agentDefaultEnv` is applied as PTY spawn env or as an inline prefix on the typed command. The bundle suggests spawn env. Confirm by setting it for one agent if option a is not enough.
