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

Config lives in `~/.config/headroom/config.toml` (override with `HEADROOM_CONFIG_DIR` or `XDG_CONFIG_HOME`). Unknown keys are an error. Thresholds default to 0 (unset) until the observe baseline (#23).

```toml
[policy]
min_headroom_gb = 6
per_worktree_cap_gb = 12
lease_timeout = "2m"

[budget]
host_baseline_gb = 10
```

License: AGPL-3.0-only
