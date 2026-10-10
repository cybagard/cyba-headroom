# headroom

[![CI](https://github.com/cybagard/cyba-headroom/actions/workflows/ci.yml/badge.svg)](https://github.com/cybagard/cyba-headroom/actions/workflows/ci.yml)

Admission control for a fleet of parallel coding agents on one Mac.

**Status:** v0.1.0 is a pre-release for the observe baseline run, [#63](https://github.com/cybagard/cyba-headroom/issues/63). That run sets the threshold defaults. All `v0.*` releases are pre-releases.

## Table of contents

- [Background](#background)
- [Install](#install)
- [Usage](#usage)
  - [Observe](#observe)
  - [Gate](#gate)
  - [Configuration](#configuration)
- [Security](#security)
- [Contributing](#contributing)
- [License](#license)

## Background

headroom shows what each coding agent costs in Docker, Tart, LM Studio and Ollama. It also lets agents ask before they start more containers or VMs through `docker`, `podman` or `tart`. The display alone is not the product. The product is the gate.

This README uses these terms with one meaning each:

- **headroom figure:** host memory minus reserved memory. The name headroom alone is the tool.
- **worktree:** a git worktree in which agents work. headroom shows one row for each worktree. It charges each gated call to one worktree.
- **daemon:** the headroom process that runs in the background and records samples.
- **shim:** the headroom binary when it runs as `docker`, `podman` or `tart`.
- **lease:** a reservation for the cost of an allowed call. The lease holds that cost until the containers or VMs that the call starts show up and use it.
- **gate:** the shim and the leases together.

Release v0.1.0 has the observe view, the daemon and the gate. The gate takes effect for agents that `headroom run` launches. For more, see [Gate](#gate).

For the full design, read the [product spec](docs/SPEC.md). The spec also has these parts:

- [problem & goals](docs/01-problem-and-goals.md)
- [requirements](docs/02-requirements.md)
- [architecture](docs/03-architecture.md)
- [metrics](docs/04-metrics.md)
- [open questions](docs/05-open-questions.md)
- [phasing](docs/06-phasing.md)

## Install

headroom runs on macOS on Apple silicon. To install it, do these steps:

1. Download `headroom-vX.Y.Z-darwin-arm64.tar.gz` and `SHA256SUMS` from the [release](https://github.com/cybagard/cyba-headroom/releases).
2. Check the tarball.
3. Unpack the tarball.
4. Run `./headroom install`.

These commands do the steps:

```sh
curl -fLO https://github.com/cybagard/cyba-headroom/releases/download/vX.Y.Z/headroom-vX.Y.Z-darwin-arm64.tar.gz
curl -fLO https://github.com/cybagard/cyba-headroom/releases/download/vX.Y.Z/SHA256SUMS
shasum -a 256 -c SHA256SUMS
tar -xzf headroom-vX.Y.Z-darwin-arm64.tar.gz   # headroom, LICENSE, README.md
./headroom install
```

`install` copies the binary that it runs from to `~/.local/bin/headroom`. Thus `install` works from the directory where you unpacked it. `install` also starts the daemon. For more, see [Observe](#observe).

To run `headroom` by name, put `~/.local/bin` on your PATH.

To upgrade, run `install` again.

`headroom uninstall` removes the LaunchAgent, the binary and the shim links. It keeps the config, the samples and the logs.

## Usage

### Observe

`headroom install` runs the daemon as a LaunchAgent. The LaunchAgent starts at login, and it restarts the daemon if the daemon crashes. The daemon writes its log to `~/Library/Logs/headroom/daemon.log`. The log rotates at 5 MB. The view and `status` read from the daemon.

```sh
headroom install              # daemon under launchd (or: headroom daemon, in a terminal)
headroom logs [-f]            # its log
headroom                      # observe view, printed once
headroom --watch              # redraws in place; Ctrl-C to quit
headroom --all                # also worktrees with no agent and nothing running
headroom status --json        # the full snapshot
headroom uninstall
```

The view shows the headroom figure and memory pressure first. Below them, it shows one row for each worktree. Each row shows the agents, containers, Tart VMs and CPU of the worktree. It also shows the memory that its agents use themselves. After the rows, the view shows each item that matches no worktree.

The view uses these marks:

- `⚑` marks a worktree that holds containers or VMs while none of its agents works.
- `⚠` marks a worktree that holds an ungated container or VM.
- `?` means unknown. It never means 0.
- `≤` before the headroom figure means that a source has not reported yet.

An ungated container or VM is one that did not start through the gate. A container or VM is ungated when it starts in one of these ways:

- through the Docker socket, or through an SDK such as Testcontainers;
- from the login shell of a script;
- from an agent that `headroom run` did not launch;
- while the daemon was down.

The footer of the view names each ungated item. The daemon log shows a warning when each one first appears.

To tell gated items from ungated items, headroom gives each check an exact key for what the call starts:

- **`docker` or `podman` `run` or `create`:** the shim adds the lease ID to each container that the call makes. It adds the ID as the label `dev.headroom.lease`.
- **`start`:** the key is the container ID that Docker reports at check time.
- **`compose up`:** the key is the project name. The shim gets the name from `-p`. If there is no `-p`, the shim uses the name that `docker compose config` gives. If that command fails, the shim uses the default name that Compose gives a project. Compose makes this default name from `COMPOSE_PROJECT_NAME`, from a `.env` file, or from the name of the project directory.
- **`tart run`:** the key is the VM.

The default Compose name is a guess. Thus a lease with that key binds only containers in its own worktree. The shim also sends the working directory that Compose labels the project with. This directory tells two stacks with the same project name apart.

A container or VM binds only the lease whose key it matches.

headroom does not flag these containers and VMs:

- containers and VMs that were running before the daemon started;
- containers that Docker restarts within 30 s after its engine comes back.

A container that comes back within the lease timeout keeps the mark that it had. If the container crashed, it has at least 2 minutes to come back.

The view uses color only on a terminal. It never uses color when `NO_COLOR` has a value.

### Gate

`headroom install` also links `docker`, `podman` and `tart` in `~/.config/headroom/shims` to the headroom binary. These links are the shims. When this directory is first on PATH, each container or VM that an agent starts goes through the gate. The gate gives one of these verdicts:

- **Allowed:** the call runs as usual.
- **Denied:** the call exits 75. It gives the agent a message with the headroom figure and the cost. The message also tells what the worktree of the agent can reuse or stop.
- **Waiting:** with `BUDGET_WAIT=1`, the call waits for room or for a macOS VM slot. It waits only when the wait can help. It waits up to `policy.wait_timeout`. The default is 10 minutes.

A call that starts nothing goes directly to the real tool. A call to a remote Docker or Podman engine also goes directly to the real tool. If the daemon is down, each call runs and shows a one-line warning.

`headroom run -- <agent>` launches an agent with the shims first on PATH. The tool shells of Claude and Kilo keep the PATH that their agent started with. The [tool shell spike](docs/spikes/tool-shell-path.md) shows this. Thus the gate also checks calls from the scripts and tools that these shells run.

A login shell, such as `zsh -l` or `bash -l`, is an exception. It rebuilds PATH through `path_helper`, and it puts the real `docker` first. The view shows what such calls start as ungated, with `⚠`. `headroom doctor` tells you whether a login shell does this.

**Gate the agents that Orca launches.** Do these steps:

1. In Orca, open Settings → Agents.
2. Set the command of each agent to run through headroom.

`headroom install` prints these lines with the full path. It prints one line for each agent that it finds on PATH:

```text
  claude: ~/.local/bin/headroom run -- claude
  kilo: ~/.local/bin/headroom run -- kilo
```

An agent that already runs keeps its old PATH until you restart it.

headroom gets the worktree from the `ORCA_WORKTREE_ID` variable that Orca sets. For agents that you launch outside Orca, set `HEADROOM_WORKTREE` to override it. `HEADROOM_SHIM_DEBUG=1` shows the verdict for each call.

**Check a shell with `headroom doctor`.** Run `headroom doctor` in a worktree terminal. You can also tell an agent to run it in its tool shell. It checks these items:

- the config;
- the shim links;
- that `docker`, `podman` and `tart` resolve to the shims on this PATH;
- which real binary each shim runs;
- that the daemon is up;
- the worktree that headroom charges the calls to, and how headroom found it;
- whether a login shell would put the real tools first.

Each line of the output tells you what to fix. If a check fails, `headroom doctor` exits 1.

### Configuration

The config file is `~/.config/headroom/config.toml`. To change its directory, set `HEADROOM_CONFIG_DIR` or `XDG_CONFIG_HOME`. An unknown key is an error. `headroom config` prints each setting that is in effect. GB means GiB.

`min_headroom_gb`, `per_worktree_cap_gb` and `host_baseline_gb` have the default 0 until the observe baseline run sets them. That run is [#63](https://github.com/cybagard/cyba-headroom/issues/63). A value of 0 means no margin, no cap and no baseline. The defaults for the Docker, LM Studio and Ollama overheads come from the spike measurements.

The example below sets example values for the three thresholds. These values are not defaults.

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

The daemon records each tick to `samples/YYYY-MM-DD.jsonl` in the config directory. For what the samples hold, see [Security](#security).

`headroom suggest [--since 14d] [--write]` suggests `[budget]` and `[policy]` values from the samples. `--write` merges these values into `config.toml`.

The daemon compresses each finished day with gzip. It deletes the days that are older than `retention`.

A measured sample was about 4 KB, with nine to eleven worktree records in it. At the default 5 s interval, the current day uses about 70 MB. Gzip made a day about 22 times smaller, to about 3 MB. Thus 30 days use about 160 MB.

```toml
[samples]
enabled = true
retention = "720h"   # 30 days; at least 24h
```

## Security

To report a vulnerability, and to see what headroom contacts and stores, read [SECURITY.md](SECURITY.md).

## Contributing

To build, test and release headroom, read [CONTRIBUTING.md](CONTRIBUTING.md). It also shows the development harness and the delivery loop.

## License

[AGPL-3.0-only](LICENSE)
