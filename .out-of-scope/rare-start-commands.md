# Gating rarely used commands that start work

The shim gates the commands agents use to start containers, pods and VMs: `run`, `create`, `start`, `restart`, `compose`, Podman's `pod`, `kube`, `play`, `machine` and `container clone`, and `tart run`. It does not gate these other commands that also start work:

- `podman container restore`: starts a container from a checkpoint.
- `podman container runlabel`: runs the command an image's label names, usually a `podman run`. Podman's remote client, the one macOS has, refuses it, so here it starts nothing.
- `docker buildx create` with the `docker-container` driver: makes a builder whose BuildKit container starts on the first build (or at once with `--bootstrap`).
- `docker model run`: runs a model in Docker Model Runner, on the host.

## Why this is out of scope

Nothing here uses them. On the dev Mac (2026-10-14), 1,883 agent transcripts hold no call to any of them, and the daemon log's 915 ungated warnings are all ordinary containers, compose projects and machines. The only buildx builders are the `docker`-driver ones Docker Desktop makes. Docker Model Runner was not running on 2026-10-14; on 2026-10-17 its backend ran with no model loaded.

`container restore` and a BuildKit builder still start containers, which the daemon reports as ungated when they appear. A wrong allow is detected, not prevented, as for any other call that bypasses the shim (SPEC, "Not a security boundary").

Gating each one costs a parser, a cost estimate, a lease key, and its flags checked against the tool's `--help`, kept up to date at every upgrade: a standing cost for calls that are not made.

`docker model run` starts no container, so the ungated check never sees it. Its cost is a model's memory on the host, which is the model budget's concern (next to LM Studio and Ollama), not the shim's.

## When to revisit

- An agent here runs one of these commands: gate that one in the shim, with its own cost and lease key.
- A model is loaded in Docker Model Runner (`docker model ps` lists one): add Model Runner to the model budget as a source, rather than gating `docker model run` in the shim.

## Prior requests

- #279: gate `buildx create`, `restore`, `runlabel` and `model run` (part 3 of #278). It names `podman restore`, which Podman 6.1.3 does not have.
