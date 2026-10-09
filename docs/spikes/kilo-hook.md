# Spike: does Kilo CLI have a before-tool hook that can deny?

Issue: #11 · Kilo CLI 7.8.3 · macOS · 2026-10-09

## Answer

**Yes.** In headless `kilo run`, a plugin's `tool.execute.before` hook ran before each tool call tested: `bash`, and `task`, which starts subagents. If the hook throws, the call does not run, and the model receives the thrown message as the tool's error. Kilo waits for an async hook, so the hook can ask headroom first. A plugin in the global config dir worked in a project that has no `.kilo/` dir, and it also saw a subagent's calls. For #41, Kilo can get the same early warning as Claude Code's PreToolUse hook. The shim stays authoritative.

Constraints for #41:

- **A hook that throws by accident also blocks the call.** The plugin must catch its own errors and throw only for a deliberate deny, or it breaks R7 (fail open).
- **The hook must not hang.** Kilo waits for it, and no Kilo-side timeout was seen or tested. The plugin's check needs its own bound, the shim's 500 ms (`policy.daemon_timeout`), and must allow on timeout.
- **`kilo --pure` loads no plugins.** The hook is advisory. That is fine, because the shim still gates the call.

**Not tested:**
- the interactive TUI, as opposed to `kilo run`;
- MCP tools;
- whether a project's `.kilo` config can turn global plugins off;
- Kilo's default global plugin path when `XDG_CONFIG_HOME` is unset.

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
| Subagent (task tool) runs `docker run …` | ✅, with the subagent's own `sessionID` | **no** | The subagent saw the message, and the parent saw it in the task result |
| `docker run --rm alpine true`, with the plugin in the global `$XDG_CONFIG_HOME/kilo/plugins/` and no `.kilo/` in the project | ✅ | **no** | The same deny |
| The hook throws an unexpected `TypeError` on `echo ok` | ✅ | **no** | `undefined is not an object (evaluating '(void 0).boom')`, which it took for a harness failure |
| `kilo run --pure` | ❌ plugin not loaded | yes | Normal output |

## What the hook gets

- **Plugin context** (once per load): `client`, `project`, `worktree`, `directory`, `experimental_workspace`, `serverUrl` and `$`. `directory` and `worktree` are both the project root, a path. headroom's worktree ID comes from the environment instead (see "For #41").
- **Per call:** `input = { tool, sessionID, callID }`, plus `output.args`, the tool's arguments. In every `bash` call observed, the args were `{ command, description }`. OpenCode-style bash tools also accept an optional `workdir`, which no call used. The plugin should read it when it is present. A `cd` inside the command is only text to the hook.
- **`tool.execute.after`** received `{ title, output }` for `echo ok`. In the first run it did not fire for the denied `docker run`.

The binary also names a `permission.ask` hook and a `shell.env` hook. They were not tested. `shell.env` may be a way to set `PATH` and `HEADROOM_WORKTREE` for Kilo's tool shells, as #31 does through Orca's launch env.

## For #41

- **Install the plugin once, in Kilo's global plugin dir.** The tested path was `$XDG_CONFIG_HOME/kilo/plugins/`. Kilo reads its global config from `~/.config/kilo/`, so with `XDG_CONFIG_HOME` unset the dir is presumably `~/.config/kilo/plugins/`. #41 should confirm that.
- **Do not re-implement the shim's parser in TypeScript.** The hook sees a command string, while `shim.Parse` works on argv against flag tables that are tested against the CLIs. Hand the string to headroom, for example a check mode that splits it and runs `shim.Parse`, so that one parser decides what is gated and what kind it is (`container`, `compose` or `tart`).
- **Let headroom identify the worktree.** `--worktree` takes a worktree ID (`HEADROOM_WORKTREE`, else `ORCA_WORKTREE_ID`), not a path. Leave it out, so `check` identifies the caller as the shim does, or pass `HEADROOM_WORKTREE` from the environment. Do not pass `ctx.worktree`, which is a path.
- **Throw only on a deny.** `headroom check` exits 75 on a deny, 1 when the daemon is unreachable or fails, and 2 on a usage error. Throw only on 75, with headroom's message. Let everything else through: other exit codes, a timeout, and any error in the plugin.
- **No shell.** Never splice the model's command into a shell string to run the check (`exec`, `sh -c`). The hook may run before Kilo's own permission prompt, so an injection there would run a model-chosen command unapproved. Talk to the daemon's socket, or use `execFile`/`spawn` with an argv array.
- **Leases stay with the shim.** A denied call never reaches the shim, so it takes no lease. An allowed call is checked again by the shim, which takes the lease as today.
