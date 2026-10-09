# Spike: does Kilo CLI have a before-tool hook that can deny?

Issue: #11 · Kilo CLI 7.8.3 · macOS · 2026-10-09

## Answer

**Yes, in headless `kilo run`.** A plugin's `tool.execute.before` hook ran before every tool call tested:
- `bash`;
- `task`, which starts a subagent;
- the subagent's own `bash` call.

If the hook throws, the call does not run, and the model receives the thrown message as the tool's error. Kilo waits for an async hook, so the hook can ask headroom first. A plugin in the global config dir worked in a project that has no `.kilo/` dir. So Kilo can likely get the same early warning as Claude Code's PreToolUse hook. #41 must confirm it in the interactive TUI that Orca agents run, which was not tested. The shim stays authoritative.

Two more results matter for #41 (see the questions at the end):
- **An unexpected throw in the hook also blocks the call.**
- **`kilo run --pure` loads no plugins**, so the hook can only ever be advisory. The shim still gates the call.

No Kilo-side timeout on the hook was seen or tested.

**Not tested:**
- the interactive TUI, as opposed to `kilo run`;
- MCP tools;
- a subagent's calls with the plugin in the global dir (only with the project's own plugin);
- whether a project's `.kilo` config can turn global plugins off;
- Kilo's default global plugin path when `XDG_CONFIG_HOME` is unset;
- whether the hook runs before or after Kilo's permission prompt;
- `--pure` in the interactive TUI.

## Method

A plugin, `.kilo/plugins/headroom.ts`, was placed in a scratch project. It logs each `tool.execute.before` call, waits 300 ms (standing in for `headroom check`), and throws `headroom: denied (test): …` for a `bash` call whose command starts with `docker run`:

```ts
export const Headroom = async (ctx) => ({
  "tool.execute.before": async (input, output) => {
    await new Promise((r) => setTimeout(r, 300)) // as if asking headroom check
    if (input.tool === "bash" && /^\s*docker\s+run\b/.test(output.args.command ?? "")) {
      throw new Error("headroom: denied (test): not enough memory for `docker run`. Retry later or ask the user.")
    }
  },
})
```

Each case ran headless against a local model, `kilo run -m <provider/model> --format json "<prompt>"`, while `docker events --filter event=create` recorded any container created.

## Results

| Case | Hook fired | Call ran | What the model saw |
|---|---|---|---|
| `docker run --rm alpine true` | ✅ waited 307 ms | **no**, no `alpine` container created | Tool error with the exact message. It reported the deny and went on. |
| `echo ok`, the same session | ✅ waited 300 ms | yes, printed `ok` | Normal output |
| Subagent (task tool) runs `docker run …`, project plugin | ✅ for the `task` call (parent's session), then for the subagent's `bash` call (its own `sessionID`) | `task` yes, `docker run` **no** | The subagent saw the message, and the parent saw it in the task result |
| `docker run --rm alpine true`, with the plugin in the global `$XDG_CONFIG_HOME/kilo/plugins/` and no `.kilo/` in the project | ✅ | **no** | The same deny |
| The hook throws an unexpected `TypeError` on `echo ok` | ✅ | **no** | `undefined is not an object (evaluating '(void 0).boom')`, which it took for a harness failure |
| `kilo run --pure` | ❌ plugin not loaded | yes | Normal output |

## What the hook gets

- **Plugin context**, logged when the plugin loaded: `client`, `project`, `worktree`, `directory`, `experimental_workspace`, `serverUrl` and `$`. In the scratch project, a git repo started at its root, `directory` and `worktree` were both that root path. Whether they differ for a session started in a subdirectory, or outside git, was not tested.
- **Per call:** `input = { tool, sessionID, callID }`, plus `output.args`, the tool's arguments. In every `bash` call observed, the args were `{ command, description }`. Whether Kilo's bash tool accepts a working-directory argument was not checked. A `cd` inside the command is only text to the hook.
- **`tool.execute.after`** received `{ title, output }` for `echo ok`. In the one run that logged it, it did not fire for the denied `docker run`. #41 should not rely on that.

The binary also names a `permission.ask` hook and a `shell.env` hook. They were not tested. `shell.env` may be a way to put the shims first on `PATH` for Kilo's tool shells, as #31's `headroom run` does for the agent.

## For #41

This spike answers whether Kilo can be hooked. How the plugin should ask headroom is #41's design. These are the facts it starts from, and the questions its plan has to answer.

**Facts** (from `internal/cli/check.go`, `internal/cli/run.go`, `internal/cli/shimgate.go`, `internal/lease/lease.go`, `internal/policy/policy.go` and the earlier spikes):
- An allowed `headroom check` takes a lease (`Book.Check` in `internal/lease/lease.go`): one that reserves its cost when a worktree is identified for it, and a zero-cost one for a manual call. That happens for any command, `echo ok` included, at the container default when no cost is given (the Tart default for `--kind tart`; see `policy.Decide`). `check` never releases the lease. How a lease lives and ends is set out in the package comment of `internal/lease/lease.go`. So a pre-check through today's `check` would reserve each gated call a second time, and reserve memory for calls that start nothing.
- `check` has no parser. It joins its arguments, defaults to `--kind container` and asks about any command, so under pressure it would deny `echo ok`. It sends the raw command, while the shim sends only `shim.Call.Command`, without flags that may hold secrets.
- `check` answers once. It exits 0 on allow and 75 on deny. It exits 1 when the config cannot be loaded or the daemon is unreachable or fails, and 2 on a usage error. Unlike the shim, it does not wait under `BUDGET_WAIT`.
- `--worktree` takes a worktree ID, not a path. Without it, `check` identifies the caller as the shim does, through `callerRequest` in `internal/cli/shimgate.go`, but from its own process, not the tool shell's. Orca sets `ORCA_WORKTREE_ID` in the agent's environment (`orca-launch-env.md`), and Kilo's tool shells inherited it in headless `kilo run --auto` (`tool-shell-path.md`). `HEADROOM_WORKTREE` is an optional override that nothing in headroom sets: `headroom run` only puts the shims first on PATH (`internal/cli/run.go`; see #95). Wherever neither is set, outside Orca or in a tool shell that lacks them, `check` falls back to its cwd and parent processes (question 3).
- The hook was awaited on every tool call observed (`bash`, `task`). If that holds for all tools, every call pays for whatever the hook does.

**Questions for #41's plan:**
1. **Leases.** How does the hook ask without taking a lease, so that only the shim reserves? For example, a dry-run mode of `check` or a daemon op. A check that times out or answers late must not leave one behind either.
2. **One request.** How does the hook ask with exactly the request the shim would build: the parse, the cost from `-m`, the kind, the tart VM fields, the remote-engine skip, and `Command` without secrets? How is a shell string turned into that: the first simple command, or full shell lexing that skips quoted text? What about `cd x && docker run …`?
3. **Identity.** Does the hook's check name the same worktree as the shim in every launch path #41 supports? This is at risk when neither variable is set and the caller is found by cwd and parent processes, which differ between the Kilo process and its tool shell.
4. **Waiting.** Under `BUDGET_WAIT`, should the hook let a retryable deny through to the shim, which waits?
5. **Time and cost.** Where does the time bound live: in `check`, from `daemon_timeout`, or in the plugin? What happens to a check still running when the bound passes? Can a cheap prefilter on the words docker, podman and tart spare `ls` the spawn?
6. **Failure.** How does the plugin make sure that only a deliberate deny throws, so that its own errors fail open (R7)? How does it pass the model's text to `check` without a shell, which would run it?
7. **The TUI.** Do these results hold in the interactive TUI that Orca agents run?
