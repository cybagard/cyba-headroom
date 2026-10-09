# Spike: does Kilo CLI have a before-tool hook that can deny?

Issue: #11 · Kilo CLI 7.8.3 · macOS · 2026-10-09

## Answer

**Yes.** A Kilo plugin's `tool.execute.before` hook runs before every tool call. If it throws, the call does not run, and the model receives the error message as the tool's result. Kilo waits for an async hook, so the hook can ask `headroom check` first. A plugin in the global config dir covers every project, and it covers subagents too. For #41, Kilo can get the same early warning as Claude Code's PreToolUse hook. The shim stays authoritative.

Two constraints for #41:

- **A hook that throws by accident also blocks the call.** The plugin must catch its own errors and throw only for a deliberate deny, or it breaks R7 (fail open).
- **`kilo --pure` loads no plugins.** The hook is advisory. That is fine, because the shim still gates the call.

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
| Plugin in the global `$XDG_CONFIG_HOME/kilo/plugins/`, project with no `.kilo/` | ✅ | **no** | The same deny |
| The hook throws an unexpected `TypeError` on `echo ok` | ✅ | **no** | `undefined is not an object (evaluating '(void 0).boom')`, which it took for a harness failure |
| `kilo run --pure` | ❌ plugin not loaded | yes | Normal output |

## What the hook gets

- **Plugin context** (once per load): `client`, `project`, `worktree`, `directory`, `experimental_workspace`, `serverUrl` and `$`. `directory` and `worktree` are the project root, which identifies the worktree for #41.
- **Per call:** `input = { tool, sessionID, callID }` and `output.args`, the tool's arguments. For `bash` these are `{ command, description }`. The call has no working directory of its own, so a `cd` inside the command is not visible.
- **`tool.execute.after`** gets `{ title, output }` for calls that ran. It is not called for a denied call.

The binary also names a `permission.ask` hook and a `shell.env` hook. They were not tested. `shell.env` may be a way to set `PATH` and `HEADROOM_WORKTREE` for Kilo's tool shells, as #31 does through Orca's launch env.

## For #41

- Install the plugin once, as `~/.config/kilo/plugins/headroom.ts`, not per repo.
- Handle only `bash` calls that the shim's parser (`shim.Parse`) would gate. Ask the daemon (or `headroom check`) with `ctx.worktree`, and throw only on a deny, with the shim's own message. Wrap everything else in `try/catch` and let the call through.
- A denied call never reaches the shim, so it takes no lease. An allowed call is checked again by the shim, which takes the lease as today.
