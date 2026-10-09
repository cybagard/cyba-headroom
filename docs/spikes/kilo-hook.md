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

This spike answers whether Kilo can be hooked. How the plugin should ask headroom is #41's design. These are the facts it starts from, and the questions its plan has to answer.

**Facts:**
- An allowed `headroom check` with a worktree **takes a lease**, and `check` never releases it. A plugin that pre-checks through today's `check` would reserve each gated call twice: once for the hook, once for the shim.
- `check` has no parser. It joins its arguments, defaults to `--kind container` and asks about any command, so under pressure it would deny `echo ok`. It also sends the raw command, while the shim sends only `shim.Call.Command`, without flags that may hold secrets.
- `check` answers once. Its exit codes are 0 allow, 75 deny, 1 daemon unreachable or broken, and 2 usage. Unlike the shim, it does not wait under `BUDGET_WAIT`.
- `--worktree` takes a worktree ID, not a path, so `ctx.worktree` cannot be passed as it is. Without it, `check` identifies the caller from its own environment, cwd and ancestors. Those are the Kilo process's, not the tool shell's.
- The hook is awaited on every tool call, including `ls`, and an unexpected throw blocks the call.

**Questions for #41's plan:**
1. **A check that takes no lease.** Should there be a dry-run mode of `check`, or a daemon op, so that only the shim reserves? A timed-out or late answer must not leave a lease behind either.
2. **One request builder.** The hook should ask with exactly the request the shim would build, through the same Go function as `gate()`: the parse, the cost from `-m`, the kind, the tart VM fields, the remote-engine skip, and `Command` without secrets. How is a shell string turned into that? It might mean the first simple command only, or full shell lexing that skips quoted text, and `cd x && docker run …` has to be handled.
3. **Identity.** Which environment and cwd should the hook's check use, so that it names the same worktree the shim will? Especially if #31 or `shell.env` set `HEADROOM_WORKTREE` only in tool shells.
4. **Waiting.** Under `BUDGET_WAIT`, should the hook let a retryable deny through to the shim, which waits, rather than throw?
5. **Bounds and cost.**
   - Where does the time bound live: in `check`, from `daemon_timeout`, or as a generous outer cap in the plugin?
   - Kill the child on timeout.
   - Is a cheap TypeScript prefilter on the words docker, podman and tart acceptable, so that `ls` spawns nothing?
6. **Safety.**
   - Catch every error and throw only on a deliberate deny.
   - Never run the model's text through a shell (`exec`, `sh -c`).
   - Pass it as an argv element after `--`, or in a request field.
7. **The interactive TUI.** Confirm everything above in the TUI that Orca agents run, not only in `kilo run`.
