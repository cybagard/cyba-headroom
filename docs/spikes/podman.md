# Spike: a Podman machine, as headroom would read it

Issue: #49 · Podman 6.1.3 (Homebrew), machine OS `quay.io/podman/machine-os:6.1` (Fedora 44, kernel 7.1) · Apple Silicon Mac, macOS 27, Docker Desktop 4.93.0 running beside it · 2026-10-11

## Answer

**A Podman machine is an Apple Virtualization VM, the same kind of process as Docker Desktop's, told apart by the disk image it holds open. Its engine speaks the Docker API on a socket that `podman machine inspect` names, and `/version` says "Podman Engine". headroom sees none of it today. The VM's whole footprint, about 1 GB idle for a 2 GiB machine, lands in unaccounted memory, and the shim already gates `podman run`, whose lease then never binds.**

| Probe | Question | Result |
|---|---|---|
| P1 | Machine setup | `podman machine init --cpus 2 --memory 2048` (98 s, mostly the image pull), then `podman machine start`. VM type `applehv`. Default machine name `podman-machine-default`; not rootful. |
| P2 | Where the engine socket is | `podman machine inspect` → `ConnectionInfo.PodmanSocket.Path`: `/var/folders/<…>/T/podman/podman-machine-default-api.sock`, a per-user temp dir that changes between Macs. No `DOCKER_HOST` set, and no Docker context points at it. `podman system connection list` lists only ssh URIs, not this socket. The other `inspect` fields: `Name`, `State` (`running`/`stopped`), `Rootful`, `Resources` (`CPUs` 2, `Memory` 2048 in MiB, `DiskSize` 100 in GiB), `ConfigDir.Path` and `SSHConfig`. A stopped machine still reports the same socket path. |
| P3 | How the engine tells itself apart | `/version`: `Version` 6.1.3, `Components` `["Podman Engine", "Conmon", "OCI Runtime (crun)"]`, `Platform.Name` `linux/arm64/fedora-44`. Docker Desktop: `Components` `["Engine", "containerd", "runc", "docker-init"]`, `Platform.Name` `Docker Desktop 4.93.0 (…)`. `/info`: `ID` a UUID, `OperatingSystem` `fedora`, `MemTotal` 2037100544 (the machine's memory); Docker Desktop's says `OperatingSystem` `Docker Desktop`. |
| P4 | Which processes run the VM | Three, all children of launchd (ppid 1): `libexec/podman/gvproxy` (networking, 28 MB RSS), `libexec/podman/vfkit --cpus 2 --memory 2048 …` (the VM's launcher, 34 MB RSS), and `com.apple.Virtualization.VirtualMachine` (the VM itself, the same XPC helper Docker Desktop and Tart use). No `krunkit`. |
| P5 | How to tell the VM process from Docker's and Tart's | `lsof -p <helper>` holds `~/.local/share/containers/podman/machine/applehv/podman-machine-default-arm64.raw` (the disk) and `…/applehv/efi-bl-podman-machine-default` (the EFI store). The machine's name is in both file names. Docker's helper holds files under `Docker.app` or `com.docker.docker`; Tart's, `~/.tart/vms/<name>/`. |
| P6 | VM footprint against its containers | `footprint` of the helper: 1035 MB idle, 1070 MB with one `redis:7-alpine` running. The engine's `/containers/<id>/stats` reported `memory_stats.usage` 3.2 MB and `limit` 2037100544. `podman stats`: 3.5 MB / 2.037 GB. So the VM costs about 1 GB before any container, about half its configured memory. |
| P7 | What headroom does today | `headroom status --json`: no Podman engine, no Podman VM. The VM's footprint is inside `budget.unaccounted_bytes`. The shim gated `podman run` (it refused once under memory pressure, then passed with `BUDGET_WAIT=1`) and labelled the container `dev.headroom.lease`. The daemon never sees that container, so that lease never binds and ends at its timeout. |

What the design takes from it:

- **Discovery:** the socket is only known from `podman machine inspect`, so the plan's cache, re-read when the socket stops answering, is needed. A stopped machine still reports its socket path, so a path that is known but dead is the normal stopped state. The path is per-user temp, so never hard-code it.
- **Kind:** `/version` `Components[].Name == "Podman Engine"` names Podman. Docker Desktop's `Platform.Name` names it. Anything else is plain `docker`.
- **VM owner:** a path under `/.local/share/containers/podman/machine/` in the helper's `lsof` names Podman. The machine name is the disk file's name without `-<arch>.raw`. `vfkit` and `gvproxy` are small separate processes. Reserve them with the VM, or leave them in unaccounted (about 60 MB).
- **Overhead:** about 1 GB for a 2 GiB machine at idle, so a `podman_overhead_gb` default near 1.0, or one that scales with the machine's memory (`Resources.Memory`). One machine size measured; a 4 GiB machine is worth one more reading.
- **The shim:** `podman run` is already gated, so it takes a lease that can't bind until Podman is a source. Until then, the lease holds its reservation until its timeout, on top of the VM's footprint already in unaccounted memory. That counts the same memory twice and can deny the next call. A worktree's keyed `podman run` then warns a false "never appeared". Manual, keyless and guessed leases end quietly.

## Method

- Podman was installed with Homebrew. LuLu (the Mac's per-app firewall) had to allow `podman` and `gvproxy` before `machine init` could pull its image.
- API requests went over the machine's socket with `curl -s --unix-socket`. Docker Desktop's went over `/var/run/docker.sock`.
- Footprints were read with `footprint <pid>`, as in the container-memory spike.
- The test container was `docker.io/library/redis:7-alpine`, run through the headroom shim. It was then removed, and the machine stopped. The machine is kept (stopped) for later smoke tests: `podman machine rm` removes it.
