# Spike: does Kilo CLI have a before-tool hook that can deny?

Issue: #11 · Kilo CLI 7.8.3 · macOS · 2026-10-09

## Answer

**Yes, in headless `kilo run`.** A plugin's `tool.execute.before` hook ran before every tool call tested:
- `bash`;
- `task`, which starts a subagent;
- the subagent's own `bash` call.

If the hook throws, the call does not run, and the model receives the thrown message as the tool's error. Kilo waits for an async hook, so the hook can ask headroom first. A plugin in the global config dir worked in a project that has no `.kilo/` dir. So Kilo can likely get the same early warning as Claude Code's PreToolUse hook. #41 must confirm it in the interactive TUI that Orca agents run, which was not tested. The shim stays authoritative.

Constraints for #41:

- **A hook that throws by accident also blocks the call.** The plugin must catch its own errors and throw only for a deliberate deny, or it breaks R7 (fail open).
- **The hook must not hang.** Kilo waits for it, and no Kilo-side timeout was seen or tested. The plugin's check needs its own bound, and must allow on timeout. Set the bound above `policy.daemon_timeout` (500 ms by default, and configurable) plus the time to start `headroom`. `check` already uses `daemon_timeout` for its own wait, so a bound equal to it would cut off answers that arrive near the limit.
- **`kilo --pure` loads no plugins.** The hook is advisory. That is fine, because the shim still gates the call.

**Not tested:**
- the interactive TUI, as opposed to `kilo run`;
- MCP tools;
- a subagent's calls with the plugin in the global dir (only with the project's own plugin);
- whether a project's `.kilo` config can turn global plugins off;
- Kilo's default global plugin path when `XDG_CONFIG_HOME` is unset;
- whether the hook runs before or after Kilo's permission prompt.

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

- **Plugin context** (once per load): `client`, `project`, `worktree`, `directory`, `experimental_workspace`, `serverUrl` and `$`. `directory` and `worktree` are both the project root, a path. headroom's worktree ID comes from the environment instead (see "For #41").
- **Per call:** `input = { tool, sessionID, callID }`, plus `output.args`, the tool's arguments. In every `bash` call observed, the args were `{ command, description }`. OpenCode-style bash tools also accept an optional `workdir`, which no call used. The plugin should read it when it is present. A `cd` inside the command is only text to the hook.
- **`tool.execute.after`** received `{ title, output }` for `echo ok`. In the one run that logged it, it did not fire for the denied `docker run`. #41 should not rely on that.

The binary also names a `permission.ask` hook and a `shell.env` hook. They were not tested. `shell.env` may be a way to set `PATH` and `HEADROOM_WORKTREE` for Kilo's tool shells, as #31 does through Orca's launch env.

## For #41

- **Install the plugin once, in Kilo's global plugin dir.** The tested path was `$XDG_CONFIG_HOME/kilo/plugins/`. Kilo reads its global config from `~/.config/kilo/`, so with `XDG_CONFIG_HOME` unset the dir is presumably `~/.config/kilo/plugins/`. #41 should confirm that.
- **First, give `headroom check` a mode that parses a command string.** Today `check` has no parser. It joins its arguments, defaults to `--kind container` and asks the daemon about any command, so under pressure it would deny `echo ok`. #41 needs a mode that splits the string, runs `shim.Parse`, allows what the shim would not gate, and takes the kind from the parse (`container`, `compose` or `tart`). That way one parser, whose flag tables are tested against the CLIs, decides. Do not re-implement it in TypeScript.
- **Use `headroom check`, not the socket.** Its exit codes, caller identification and timeout are the tested Go path. A TypeScript client of the daemon protocol would duplicate them and drift.
- **Give `check` the call's place.** `--worktree` takes a worktree ID (`HEADROOM_WORKTREE`, else `ORCA_WORKTREE_ID`), not a path, so do not pass `ctx.worktree`. Leaving it out makes `check` identify the caller from its own environment, cwd and ancestors. Those are the Kilo process's, not the tool shell's. So spawn `check` with Kilo's environment, and with the call's `workdir` (else `ctx.directory`) as its cwd.
- **Throw only on a deny.** `headroom check` exits 75 on a deny, 1 when the daemon is unreachable or fails, and 2 on a usage error. Throw only on 75, with headroom's message. Let everything else through: other exit codes, a timeout, and any error in the plugin.
- **No shell.** The command is the model's text. Run `check` with `execFile` or `spawn` and an argv array, passing the command after `--`, never inside an `exec` or `sh -c` string, which would run it.
- **Leases stay with the shim.** A denied call never reaches the shim, so it takes no lease. An allowed call is checked again by the shim, which takes the lease as today.
