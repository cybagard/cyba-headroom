# Spike: what does `orca worktree ps --json` return?

Issue: #6 · Orca 1.4.222 (CLI) / 1.4.221 (app) · macOS · 2026-10-07

## Answer

**No PIDs.** `worktree ps` returns worktree metadata and agent *state*, not processes. Attribution cannot take PIDs from it, but it does not need to: Orca exports `ORCA_WORKTREE_ID` into every terminal it spawns, and that variable is inherited by the agent and every tool shell under it (see [orca-launch-env.md](orca-launch-env.md)).

## Shape

Envelope: `{ id, ok, result: { worktrees: [...], hostScope: { hostIds, omittedHostIds }, totalCount, truncated } }`.

Per worktree (fields headroom cares about in **bold**):

| Field | Example | Use |
|---|---|---|
| **`worktreeId`** | `8b21…::/Users/…/cyba-headroom/Orchestra` | Stable key, `<repoId>::<path>`. Same value as `ORCA_WORKTREE_ID`. |
| **`path`** | `/Users/…/cyba-headroom/Orchestra` | Fallback attribution by cwd prefix |
| `repoId`, `repo`, **`branch`**, `displayName` | `cyba-headroom`, `refs/heads/cybagard/Orchestra` | Display |
| **`status`** | `working` / `active` / `inactive` | Idle-holder rule (R8) |
| **`liveTerminalCount`**, `hasAttachedPty` | `1`, `true` | Whether anything can still spawn |
| **`lastActivityAt`**, `lastOutputAt` | epoch ms | Idle detection |
| `isArchived`, `isMainWorktree`, `isActive`, `workspaceStatus` | | Filtering / priority (#44) |
| `parentWorktreeId`, `childWorktreeIds`, `worktreeInstanceId` | | Lineage; not needed for v1 |
| `hostId`, `terminalPlatform` | `local`, `darwin` | Skip non-local hosts (#43) |
| `linkedIssue`, `linkedPR`, `comment`, `preview` | | Ignore. `preview` holds terminal text, so don't log it. |
| **`agents[]`** | see below | Who is running and what they're doing |

Per agent: `paneKey`, `parentPaneKey`, **`agentType`** (`claude`, …), **`state`** (`working` / `done` / …), `stateStartedAt`, `updatedAt`, `toolName`, `toolInput` (truncated), `prompt`, `lastAssistantMessage`, `interrupted`, `mainAgent.state`. `prompt`, `toolInput` and `lastAssistantMessage` contain user content: don't persist them.

`paneKey` matches the `ORCA_PANE_KEY` env var in the agent's process, so a process can be joined to its agent row.

`orca terminal list --worktree <sel> --json` adds `handle`, `ptyId`, `agentIdentity`, `connected`. It has no PIDs either.

## Process tree (observed)

```
Orca Helper (pty host)  →  /usr/bin/login  →  -zsh (login)  →  claude  →  /bin/zsh -c … (tool shell, login)
```

The agent command is typed into a login shell that Orca spawns. Env set on the PTY reaches everything below it.

## Recommendation for R3 (#20)

1. **Primary:** read `ORCA_WORKTREE_ID` from the process environment. Shims get it directly. For the observer, `ps eww -o command= -p <pid>` exposes another same-user process's env without sudo (verified on a different agent's `claude` PID).
2. **Fallback:** match a process's cwd (`lsof -a -p <pid> -d cwd`) against `worktrees[].path`, longest prefix wins.
3. Use `worktree ps` for **state only**: worktree list, `status`, agent `state`, last activity. Poll it at the daemon's collection interval. Cost: one CLI call returning ~15 KB for 7 worktrees.
4. Containers and VMs are not in the process tree (they run inside the Docker VM or as Tart processes). They get attributed at launch time by the shim, which labels them with the worktree id. Ungated ones fall back to the R4 "unattributed" bucket.
