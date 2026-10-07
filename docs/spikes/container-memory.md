# Spike: container memory reporting vs. VM host footprint (Docker Desktop)

Issue: #9 · Docker Desktop 4.93.0 (240920), engine 29.8.1, LinuxKit kernel 7.0.14 · macOS, 64 GB · 2026-10-07

**Podman is not installed on this machine and was not measured.** Re-run this spike if Podman gets used (its VM is a separate `vfkit`/`krunkit` process, and the same method applies).

## Answer

- **Container stats undercount host cost by a fixed ~1.6 GB plus a high-water mark.** The VM's host footprint ≈ 1.6 GB overhead + the *peak* guest memory since the VM started, not the current container sum.
- **The VM never gives memory back while it runs.** With every container stopped, the footprint stayed at 5.3 GB for 7+ minutes. Freed guest pages are reused by later containers, but not returned to macOS.
- **Docker Desktop's Resource Saver does release it all.** About 5 minutes after the last container activity the VM process exits (footprint → 0). Read-only API calls (`/containers/json`, `/info`) are answered by the backend in ~1 ms **without waking the VM**, so a polling collector won't keep it alive.
- **The configured VM limit (`MemoryMiB` 32512 → `MemTotal` 31 GiB) is a ceiling, not a cost.** The VM only takes host memory as the guest touches it.

So for R2: **used** = VM process `phys_footprint` (what the host actually pays); **container sum** = attribution only; **reserved** ≠ the 31 GiB limit.

## Where the memory lives

| Process | Role | Host memory |
|---|---|---|
| `com.apple.Virtualization.VirtualMachine` (XPC service, ppid 1) | **The Linux VM** | the number that matters |
| `com.docker.backend` ×3 | API proxy, services | 29–67 MB footprint each, flat throughout |
| `Docker Desktop` + helpers | UI | ~0.5 GB, flat |

The VM process exists only while the VM is up. Find it with `pgrep -f com.apple.Virtualization.VirtualMachine`. The name is generic: Tart VMs use the same XPC service. Both are XPC services with ppid 1, so the parent PID **cannot** tell them apart. #15 must find another discriminator, e.g. the responsible process (`launchctl procinfo`, may need sudo), VM bundle paths in `lsof`, or matching footprint/start time against `tart list` / Docker's VM state.

### Measuring it without sudo

| Tool | Works without sudo | Meaningful |
|---|---|---|
| `footprint <pid>` → `phys_footprint` | ✅ | ✅ This is what Activity Monitor and `top` MEM show |
| `top -l 1 -stats pid,mem` | ✅ | ✅ same figure, all processes in one call |
| `ps -o rss` | ✅ | ⚠ ~325 MB higher (counts shared framework pages); tracks footprint, but use footprint |

From Go: `proc_pid_rusage(pid, RUSAGE_INFO_V4)` → `ri_phys_footprint` (libproc, no sudo) gives the same number without forking `footprint`.

## Measurement

Containers: `alpine`, each holding N bytes in `/dev/shm` (counted in the container cgroup as `shmem`). Container figure = `docker stats` MemUsage (= cgroup `usage − inactive_file`). Snapshot 10 s after each step.

| Step | Σ containers (`docker stats`) | VM `phys_footprint` | VM `rss` | Footprint − Σ containers |
|---|---|---|---|---|
| Docker idle, VM stopped (Resource Saver) | 0 | — (no process) | — | — |
| 1 GB container (VM just booted) | 1.00 GiB | 2602 MB | 2922 MB | ~1.6 GB |
| …stopped, +5 s / +30 s / +60 s | 0 | 2616 / 2616 / 2618 MB | ~2.94 GB | 2.6 GB (none returned) |
| 0.5 GB container | 0.50 GiB | 2630 MB (+14) | 2953 MB | reused freed guest pages |
| + 1 GB container (Σ 1.5 GB) | 1.50 GiB | 3240 MB (+610) | 3563 MB | above previous peak only |
| + 2 GB container (Σ 3.5 GB) | 3.50 GiB | 5295 MB (+2055) | 5618 MB | ~1.7 GB |
| all stopped, +10 s / +50 s / +120 s | 0 | 5320 MB (flat) | 5645 MB | 5.3 GB held |
| …t+450 s | 0 | 5320 MB | | still held |
| ~t+480 s (≈5 min after last `docker` CLI call) | 0 | **VM process exits** | | 0 |

## Docker API (what the Go collector will use)

Socket: `unix:///Users/promptcritical/.docker/run/docker.sock` (from `docker context inspect`). That's 45 bytes, well under the macOS 104-byte `sun_path` limit.

| Call | Latency |
|---|---|
| `GET /containers/json` | 19 ms (VM up), 1 ms (VM stopped, answered by backend) |
| `GET /containers/{id}/stats?stream=false` | **1–2 s**: waits for a second sample to compute CPU % |
| `GET /containers/{id}/stats?stream=false&one-shot=true` | **5 ms** |
| `GET /info` → `MemTotal` | ~1 ms; returns the 31 GiB VM limit |

`memory_stats` gives `usage`, `limit`, and cgroup v2 `stats` (`anon`, `file`, `shmem`, `inactive_file`, …). Compute container memory as `usage − stats.inactive_file`, which is what `docker stats` shows.

## Recommendation

**R1 Docker collector (#14)**

- Poll `/containers/json`, then `stats?stream=false&one-shot=true` per running container (5 ms each). Never use the default `stream=false` alone: 1–2 s per container. CPU % needs two samples, so compute it from consecutive polls.
- Container memory = `usage − inactive_file`. Attribute via the shim's labels (and `com.docker.compose.project.working_dir` as a fallback).
- Host cost = `phys_footprint` of the Docker VM process (`proc_pid_rusage` from Go), found via `pgrep`-equivalent on `com.apple.Virtualization.VirtualMachine` whose ancestry is Docker, not Tart.
- VM absent → Docker host cost is 0 (Resource Saver), even though `/info` still answers.

**R2 budget model (#19)**

- **Used (Docker)** = VM `phys_footprint`, not Σ containers. Show the gap as "Docker VM overhead + retained" so the observe view explains why stopping containers doesn't free memory.
- **Reserved (Docker)** must not be `MemTotal` (31 GiB). That would leave almost no headroom on a 64 GB machine. Use: `max(VM footprint, Σ running containers + ~1.6 GB overhead) + Σ active leases`. Treat the VM's retained high-water mark as already spent, since it won't come back until Resource Saver stops the VM.
- A new container fits "for free" up to the retained high-water mark (the 0.5 GB step cost +14 MB). The policy engine (#24) can count retained-but-unused guest memory as headroom *for Docker requests only*: `retained = footprint − overhead − Σ containers`.
- Revisit the ~1.6 GB overhead in the two-week baseline (#23). It includes guest kernel, dockerd/containerd and page cache from image pulls, so it varies with workload.
