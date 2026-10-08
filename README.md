# headroom

Admission control for a fleet of parallel coding agents on one Mac. `headroom` shows what each agent costs across Docker/Podman, Tart, LM Studio and Ollama, and lets agents ask before they spawn more. Display alone is not the product; the gate is.

**Status:** observe view, daemon and gate shim work; the gate takes effect for agents launched through `headroom run` (below).

## Docs

- [Product spec (full)](docs/SPEC.md), split into: [problem & goals](docs/01-problem-and-goals.md), [requirements](docs/02-requirements.md), [architecture](docs/03-architecture.md), [metrics](docs/04-metrics.md), [open questions](docs/05-open-questions.md), [phasing](docs/06-phasing.md)
- [Implementation plan](docs/plan.md) — phases and epics; tracked as [GitHub issues](https://github.com/cybagard/cyba-headroom/issues) with sub-issues

## Development

Go, one multi-call binary ([ADR 0001](docs/adr/0001-language.md)): `headroom` is the CLI and daemon; symlinked as `docker`/`podman`/`tart` it is the gate shim.

```sh
make build   # bin/headroom
make test    # go test -race ./...
make lint    # go vet + golangci-lint
bin/headroom config   # effective config and its path
```

## Observe

`headroom install` runs the daemon as a LaunchAgent: it starts at login, restarts if it crashes, and logs to `~/Library/Logs/headroom/daemon.log` (rotated at 5 MB). The install copies the binary to `~/.local/bin/headroom`; run `install` again to upgrade. `headroom uninstall` removes the agent and the binary, but keeps config, samples and logs. The other commands read from the daemon.

```sh
bin/headroom install          # daemon under launchd (or: headroom daemon, in a terminal)
headroom logs [-f]            # its log
headroom                      # observe view, printed once
headroom --watch              # redraws in place; Ctrl-C to quit
headroom --all                # also worktrees with no agent and nothing running
headroom status --json        # the full snapshot
headroom uninstall
```

The view leads with headroom and memory pressure. Below that is one row per worktree, showing its agents, containers, Tart VMs, CPU and the agents' own memory, then anything that matches no worktree. `⚑` marks a worktree that holds containers or VMs while none of its agents is working. `⚠` marks one holding a container or VM that started without going through headroom (ungated): through the Docker socket or an SDK such as Testcontainers, a script's login shell, an agent not launched through `headroom run`, or while the daemon was down. The footer names them, and the daemon log warns when each first appears. To tell them apart, the shim adds the lease's ID to each container it lets a `docker` or `podman` `run` or `create` make, as the label `dev.headroom.lease`. Those that were running before the daemon started, that Docker restarts when it comes back, or that come back within the lease timeout after a crash, are not flagged. `?` means unknown, never 0. `≤` before headroom means some source has not reported yet. Colour is used only on a terminal, and never when `NO_COLOR` is set.

Config lives in `~/.config/headroom/config.toml` (override with `HEADROOM_CONFIG_DIR` or `XDG_CONFIG_HOME`). Unknown keys are an error. Thresholds and the host baseline default to 0 (unset) until the observe baseline (#23); the Docker, LM Studio and Ollama overheads default to the spike measurements. GB means GiB.

```toml
[policy]
min_headroom_gb = 6
per_worktree_cap_gb = 12
lease_timeout = "2m"

[budget]
host_baseline_gb = 10      # macOS, Orca, agents
docker_overhead_gb = 1.6   # Docker VM beyond its containers
lmstudio_idle_gb = 0.6     # LM Studio with no model loaded
ollama_idle_gb = 0.1       # Ollama server with no model loaded

[ollama]
host = "127.0.0.1:11434"   # default: OLLAMA_HOST, then this; launchd does not see your shell's OLLAMA_HOST
```

headroom reads Ollama's `/api/ps` only while an `ollama serve` process runs, and never contacts a host that is not this Mac.

The daemon records each tick to `samples/YYYY-MM-DD.jsonl` in the config directory (mode 0600). `headroom suggest` (#23) learns thresholds from these samples. Finished days are gzipped, and days past `retention` are deleted. A sample is about 2.7 KB with eight worktrees (measured). At the default 5 s interval that is about 47 MB for the current day and about 3 MB for each gzipped day, so 30 days take about 150 MB at most. Samples hold memory figures, worktree names and paths, container names and images, and bind-mount paths. They never hold container labels, environment or command lines.

```toml
[samples]
enabled = true
retention = "720h"   # 30 days; at least 24h
```

## Gate

`headroom install` also links `docker`, `podman` and `tart` in `~/.config/headroom/shims` to the headroom binary. With that directory first on PATH, every container or VM an agent starts goes through the gate:
- **Allowed:** the call runs as usual.
- **Denied:** it exits 75, with a message for the agent naming the headroom, the cost, and what its worktree could reuse or stop.
- **Waiting:** with `BUDGET_WAIT=1` the call waits for room instead.

Calls that start nothing pass straight through. If the daemon is down, every call runs, with a one-line warning.

`headroom run -- <agent>` launches an agent with the shims first on PATH. Claude's and Kilo's tool shells keep the PATH their agent started with ([spike](docs/spikes/tool-shell-path.md)), so calls from scripts and tools they run are gated too. One exception is a login shell (`zsh -l`, `bash -l`), which rebuilds PATH through `path_helper` and puts the real `docker` first. Detecting those calls is #33.

**Gate the agents Orca launches.** In Orca → Settings → Agents, set each agent's command to run through headroom. `headroom install` prints these lines with the full path, for the agents it finds on PATH:

```
claude: ~/.local/bin/headroom run -- claude
kilo:   ~/.local/bin/headroom run -- kilo
```

Agents that are already running keep their old PATH until they are restarted. The worktree is known from Orca's `ORCA_WORKTREE_ID`; set `HEADROOM_WORKTREE` to override it for agents launched outside Orca. `HEADROOM_SHIM_DEBUG=1` shows each call's verdict.

**Check a shell with `headroom doctor`.** Run it in a worktree terminal, or have an agent run it in its tool shell. It checks the config, the shim links, that `docker`, `podman` and `tart` resolve to the shims on this PATH (and which real binary each runs), that the daemon is up, which worktree the calls are charged to and how that was found, and whether a login shell would put the real tools first. Each line says what to fix; it exits 1 if a check fails.

License: AGPL-3.0-only
