# headroom

Admission control for a fleet of parallel coding agents on one Mac. `headroom` shows what each agent costs across Docker/Podman, Tart and LM Studio, and lets agents ask before they spawn more. Display alone is not the product; the gate is.

**Status:** Phase 1 spikes done ([docs/spikes](docs/spikes)); Phase 2 scaffold in place. Nothing is enforced yet.

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

Config lives in `~/.config/headroom/config.toml` (override with `HEADROOM_CONFIG_DIR` or `XDG_CONFIG_HOME`). Unknown keys are an error. Thresholds and the host baseline default to 0 (unset) until the observe baseline (#23); the Docker and LM Studio overheads default to the spike measurements. GB means GiB.

```toml
[policy]
min_headroom_gb = 6
per_worktree_cap_gb = 12
lease_timeout = "2m"

[budget]
host_baseline_gb = 10      # macOS, Orca, agents
docker_overhead_gb = 1.6   # Docker VM beyond its containers
lmstudio_idle_gb = 0.6     # LM Studio with no model loaded
```

The daemon records each tick to `samples/YYYY-MM-DD.jsonl` in the config directory (mode 0600). `headroom suggest` (#23) learns thresholds from these samples. Finished days are gzipped, and days past `retention` are deleted. A sample is about 2.7 KB with eight worktrees (measured). At the default 5 s interval that is about 47 MB for the current day and about 3 MB for each gzipped day, so 30 days take about 150 MB at most. Samples hold memory figures, worktree names and paths, container names and images, and bind-mount paths. They never hold container labels, environment or command lines.

```toml
[samples]
enabled = true
retention = "720h"   # 30 days; at least 24h
```

License: AGPL-3.0-only
