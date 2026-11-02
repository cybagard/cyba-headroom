# headroom

[![CI](https://github.com/cybagard/cyba-headroom/actions/workflows/ci.yml/badge.svg)](https://github.com/cybagard/cyba-headroom/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/cybagard/cyba-headroom?include_prereleases&sort=semver)](https://github.com/cybagard/cyba-headroom/releases)
[![Homebrew](https://img.shields.io/badge/homebrew-cybagard%2Ftap%2Fheadroom-orange?logo=homebrew)](https://github.com/cybagard/homebrew-tap)

Admission control for a fleet of parallel coding agents on one Mac.

**Status:** v0.2.0 is a pre-release. All `v0.*` releases are pre-releases.

## Table of contents

- [Background](#background)
- [Install](#install)
- [Usage](#usage)
- [Configuration](#configuration)
- [Security](#security)
- [Contributing](#contributing)
- [License](#license)

## Background

headroom shows what each coding agent costs in Docker and Tart, and what LM Studio and Ollama reserve on the host. It shows one row for each Orca worktree in which agents work. headroom needs [Orca](https://github.com/stablyai/orca) to find each worktree.

headroom also gates the containers and VMs that an agent in an Orca worktree starts through `docker`, `podman` or `tart`. Before a call starts something, headroom checks that the call fits in the memory budget. If it does not fit, headroom stops the call and tells the agent what it can reuse or stop.

## Install

headroom runs on macOS on Apple silicon. Install it with Homebrew:

```sh
brew install cybagard/tap/headroom
brew services start headroom
```

The service starts the daemon now and at login, and puts the shims in `~/.config/headroom/shims`. In Orca, open Settings → Agents and set the command of each agent to `headroom run -- <agent>`, for example:

```sh
headroom run -- claude
```

If Orca does not find `headroom`, use the full path, `/opt/homebrew/bin/headroom run -- <agent>`.

Do not also run `headroom install`. To upgrade, run `brew upgrade cybagard/tap/headroom`, then `brew services restart headroom`. To uninstall, run `brew services stop headroom`, then `brew uninstall cybagard/tap/headroom`, then `rm -rf ~/.config/headroom/shims`.

### Install from the release tarball

If you do not use Homebrew, install from the release tarball:

```sh
curl -fLO https://github.com/cybagard/cyba-headroom/releases/download/vX.Y.Z/headroom-vX.Y.Z-darwin-arm64.tar.gz
curl -fLO https://github.com/cybagard/cyba-headroom/releases/download/vX.Y.Z/SHA256SUMS
shasum -a 256 -c SHA256SUMS &&
  tar -xzf headroom-vX.Y.Z-darwin-arm64.tar.gz &&
  ./headroom install
```

`install` copies the binary to `~/.local/bin/headroom` and starts the daemon as a LaunchAgent. Put `~/.local/bin` on your PATH. For zsh, the default shell on macOS, run this command and then open a new terminal:

```sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zprofile
```

To upgrade, run `install` again. `headroom uninstall` removes the binary, the LaunchAgent and the shims. It keeps the config, the samples and the logs.

## Usage

### Observe

```sh
headroom                      # the view, printed once
headroom --watch              # redraws in place; Ctrl-C to quit
headroom --all                # also worktrees with no agent and nothing running
headroom status --json        # the full snapshot
headroom logs [-f]            # the daemon log
```

The view shows the headroom figure (host memory minus reserved memory) and the memory pressure first, then one row for each worktree. It uses these marks:

- `⚑`: the worktree holds containers or VMs while none of its agents works.
- `⚠`: the worktree holds a container or VM that did not start through the gate.
- `?`: unknown. It never means 0.

### Gate

`brew services start headroom` or `headroom install` puts the shims (`docker`, `podman` and `tart`) in `~/.config/headroom/shims`. Launch each agent with the shims first on PATH. A call from outside an Orca worktree is not gated.

```sh
headroom run -- claude
```

In Orca, open Settings → Agents and set the command of each agent to `headroom run -- <agent>`. With the release tarball, use the line that `install` prints for each agent. An agent that already runs keeps its old PATH until you restart it.

Each gated call gets one of these verdicts:

- **Allowed:** the call runs as usual.
- **Denied:** the call exits 75 and gives the reason, and the containers and VMs that the worktree holds, if any. If waiting can help, it tells the agent to retry later or to use `BUDGET_WAIT=1`. If not, it tells the agent to reuse or stop what the worktree holds, or to ask for less memory; if the config allows no macOS VMs, it tells the agent to set `budget.max_macos_vms`.
- **Waiting:** with `BUDGET_WAIT=1`, the call waits for room, for a macOS VM slot or for the memory pressure to decrease, when waiting can help. Waiting calls are served first come first. The default limit is 10 minutes.

If the daemon is down, each call runs and shows a warning. A login shell (`zsh -l`) can put the real tools first on PATH, or replace them with an alias or function; then its calls are not gated. The `login` line of `headroom doctor` checks a login shell of your `$SHELL`, or of `/bin/zsh` if `$SHELL` is not zsh, bash, sh, ksh or dash.

To check a shell, run `headroom doctor` in it. Each line marks a check as passed (`✓`), a warning (`!`) or failed (`✗`).

## Configuration

The config file is `~/.config/headroom/config.toml`. `headroom config` prints each setting that is in effect. `headroom suggest` suggests values from the recorded samples.

`min_headroom_gb`, `per_worktree_cap_gb` and `host_baseline_gb` keep the default 0 after the baseline run ([docs/baseline.md](docs/baseline.md)): the margin and the baseline wait on [#311](https://github.com/cybagard/cyba-headroom/issues/311), and the cap the run suggests, 7 GB, would deny a single 16 GB Tart VM. A value of 0 means no margin, no cap and no baseline. The example below uses example values, not defaults:

```toml
[policy]
min_headroom_gb = 6
per_worktree_cap_gb = 12

[budget]
host_baseline_gb = 10
```

## Security

To report a vulnerability, and to see what headroom contacts and stores, read [SECURITY.md](SECURITY.md).

## Contributing

To build, test and release headroom, read [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[AGPL-3.0-only](LICENSE)
