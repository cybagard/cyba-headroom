# Baseline run: the dev Mac

Issue: #63 · headroom `v0.1.0-202-g2494055` (the installed LaunchAgent) · macOS 27.0.1, M4 Max, 64 GB · samples 2026-10-11 to 2026-11-02 · read 2026-11-02

The LaunchAgent has recorded a sample every 5 s since 2026-10-07. Schema v2 samples start on 2026-10-11 at 19:26; `suggest` skips the 54,024 v1 samples before that, so both windows below start on 2026-10-11. This page holds what `headroom suggest` proposed, which of it became `config.Defaults`, and why the rest did not.

## Answer

| key | default | from the run |
|---|---|---|
| `[budget] lmstudio_idle_gb` | 0.6 → **0.7** | p95 of LM Studio's footprint with no model loaded, 160,532 samples (p50 0.6, max 1.4) |
| `[budget] docker_overhead_gb` | stays **1.6** | `suggest` says 21.1: the Docker VM's high-water mark, which the budget already reserves through the footprint (#310) |
| `[budget] host_baseline_gb` | stays **unset (0)** | `suggest` says 23, learned from unaccounted memory, which moves with the compressor (#311) |
| `[policy] min_headroom_gb` | stays **unset (0)** | the replay at warn onsets gives −12.8 GB (#311); `suggest` fell back to its 1 GB margin |
| `[policy] per_worktree_cap_gb` | stays **unset (0, no cap)** | `suggest` says 7 (p95 4.9 × 1.25), but one worktree peaked at 38.9 GB with two 16 GB Tart VMs: a 7 GB cap denies a single 16 GB VM |
| `[budget] ollama_idle_gb` | stays 0.1 | no data: Ollama never ran idle while agents worked |
| `[policy] pressure_guard` | stays **critical** | critical was never observed, so it would never have denied |
| `[policy] pressure_guard_rising` | stays **true** (open) | it never preceded critical, but critical never happened, so the run cannot show whether it predicts it; it would have denied for about 7 h; see [Pressure guard](#pressure-guard) | see [Pressure guard](#pressure-guard) |
| `[policy] idle_grace` | stays **2m** | a worktree's CPU stays at 5% or more after its agents leave working for p99 5.5 s, max 75 s |
| `[policy] default_container_gb` | stays **1** | p75 of containers' peaks is 1.0 GB; see [Default costs](#default-costs) | see [Default costs](#default-costs) |
| `[policy] default_tart_gb` | stays **4** | the size of every running Tart VM was known, so the default never applied |
| `steady_slope` (`internal/source/host`) | stays **1 %/min** | 60 of 65 warn episodes read ≤ −1 %/min over 5 min; 2.3% of normal-pressure samples do |
| `[daemon] trend_window` | stays **5m** | 2 min is too noisy at the threshold, 10 min too slow; see [Trend](#trend) |

- **Warn turned on at a median of 47% free (206 onsets); critical was never observed in 30 days.** The 47% is not the point at which this Mac runs short. Every warn day but one had two 16 GB Tart VMs and a 21 GB Docker VM running, with 22–29 GB in the compressor: the kernel warns as the compressor fills with idle VM pages, not as free pages run out. On 2026-10-29 LM Studio held a 29–55 GB model and free memory went down to 5%, still at warn.
- **Swap-rate unit (#18): open.** The samples point to pages (about 16 KiB per event, against `vm.swapusage`), but the rates were never compared with `vm_stat`, so the field docs still say "unverified"; see [Swap-rate unit](#swap-rate-unit-18).

## `suggest` output

`headroom suggest --since 14d` (4.3 s):

```text
headroom suggest: the last 14 days of samples

DAY          HOURS   WORKING  PEAK AGENTS  WARN MIN  CRIT MIN  PEAK SWAP
2026-10-19    14.6      13.2           11       0.0       0.0     1.0 GB
2026-10-20    21.8      21.4           11       0.0       0.0     1.0 GB
2026-10-21     4.8       2.8           11       0.0       0.0     0.0 GB
2026-10-22     0.5       0.3            1       0.0       0.0     0.0 GB
2026-10-23     2.8       2.8            1       0.0       0.0     0.0 GB
2026-10-24     3.3       3.0           10       0.0       0.0     4.1 GB
2026-10-25    25.0      17.6           10       0.0       0.0     0.3 GB
2026-10-26    24.0      18.0           13       0.0       0.0     0.2 GB
2026-10-27    11.6       7.8            6       0.0       0.0     0.2 GB
2026-10-28    24.0      14.6            9       0.0       0.0     0.2 GB
2026-10-29    14.6       0.7            4       0.6       0.0     4.5 GB
2026-10-31     3.7       2.8           10       0.0       0.0     0.0 GB
2026-11-01    24.0      20.4           11       0.0       0.0     0.0 GB
2026-11-02     8.4       0.6            5       0.0       0.0     0.0 GB

read 130874 samples (0 broken lines skipped, 0 v1 samples skipped, 0 duplicates, 0 unreadable files); left out of the budget maths: 2112 with stale inputs, 0 during swap-outs

# Suggested settings. GB means GiB. Each value says how it was found.

[budget]
host_baseline_gb = 23  # p95 of memory in use outside every component while agents worked (87733 samples; 0 left out during swap-outs)
docker_overhead_gb = 21.1  # p95 of Docker VM footprint − containers while agents worked (78107 samples; p50 19.1, p95 21.1, max 21.1 GB)
lmstudio_idle_gb = 0.7  # p95 of LM Studio's footprint with no model loaded (87733 samples; p50 0.6, max 1.1 GB)
# ollama_idle_gb: not enough data: Ollama ran with no model loaded for 0s of working time, need 10m0s

[policy]
# min_headroom_gb: not enough data: pressure turned warn 0 time(s) while agents worked, need 3; no evidence for a floor yet
per_worktree_cap_gb = 7  # p95 of daily per-worktree peaks (containers + Tart VMs) × 1.25 (582 worktree-days; p95 4.9, max 6.3 GB)

# Advice (headroom changes nothing outside its own config)
- Up to 13 agents worked in parallel at normal pressure.
```

`headroom suggest --since 30d`:

```text
headroom suggest: the last 30 days of samples

DAY          HOURS   WORKING  PEAK AGENTS  WARN MIN  CRIT MIN  PEAK SWAP
2026-10-11     4.6       4.4            6      23.7       0.0     1.2 GB
2026-10-12    24.0      17.3           12     221.5       0.0     0.9 GB
2026-10-13    21.9      21.9            7      62.9       0.0     0.2 GB
2026-10-14     5.2       5.2           18      60.8       0.0     2.1 GB
2026-10-15    18.8      18.8           12      30.6       0.0     2.0 GB
2026-10-16     3.5       3.5            1       0.0       0.0     1.2 GB
2026-10-17     5.6       5.6           16      34.4       0.0     2.0 GB
2026-10-18    24.0      24.0           10     101.1       0.0     1.8 GB
2026-10-19    24.0      17.8           11       0.0       0.0     1.5 GB
2026-10-20    21.8      21.4           11       0.0       0.0     1.0 GB
2026-10-21     4.8       2.8           11       0.0       0.0     0.0 GB
2026-10-22     0.5       0.3            1       0.0       0.0     0.0 GB
2026-10-23     2.8       2.8            1       0.0       0.0     0.0 GB
2026-10-24     3.3       3.0           10       0.0       0.0     4.1 GB
2026-10-25    25.0      17.6           10       0.0       0.0     0.3 GB
2026-10-26    24.0      18.0           13       0.0       0.0     0.2 GB
2026-10-27    11.6       7.8            6       0.0       0.0     0.2 GB
2026-10-28    24.0      14.6            9       0.0       0.0     0.2 GB
2026-10-29    14.6       0.7            4       0.6       0.0     4.5 GB
2026-10-31     3.7       2.8           10       0.0       0.0     0.0 GB
2026-11-01    24.0      20.4           11       0.0       0.0     0.0 GB
2026-11-02     8.4       0.6            5       0.0       0.0     0.0 GB

read 214886 samples (0 broken lines skipped, 54024 v1 samples skipped, 0 duplicates, 0 unreadable files); left out of the budget maths: 5018 with stale inputs, 22 during swap-outs

# Suggested settings. GB means GiB. Each value says how it was found.

[budget]
host_baseline_gb = 23  # p95 of memory in use outside every component while agents worked (160475 samples; 22 left out during swap-outs)
docker_overhead_gb = 21.1  # p95 of Docker VM footprint − containers while agents worked (116213 samples; p50 19.3, p95 21.1, max 21.1 GB)
lmstudio_idle_gb = 0.7  # p95 of LM Studio's footprint with no model loaded (160532 samples; p50 0.6, max 1.4 GB)
# ollama_idle_gb: not enough data: Ollama ran with no model loaded for 0s of working time, need 10m0s

[policy]
min_headroom_gb = 1  # p90 headroom at 143 warn onsets, replayed with the suggested budget (-12.8 GB), + 1 GB margin
per_worktree_cap_gb = 7  # p95 of daily per-worktree peaks (containers + Tart VMs) × 1.25 (759 worktree-days; p95 4.9, max 38.9 GB)

# Advice (headroom changes nothing outside its own config)
- Memory pressure turned warn at a median of 47% free (143 times).
- Up to 18 agents worked in parallel at normal pressure.
- Pressure turned warn with a median of 3 agents working.
```

`suggest` counts 143 warn onsets: those with an agent working. The tables below count all 206.

## Per day

`suggest`'s table above, with what ran. Tart is the share of samples with at least one, and two, Tart VMs running.

| day | samples | Tart ≥ 1 | Tart 2 | Docker VM max GB | compressor max GB | min free % | warn onsets |
|---|---|---|---|---|---|---|---|
| 10-11 | 3,287 | 40% | 17% | 21.1 | 29.5 | 42 | 12 |
| 10-12 | 17,259 | 49% | 30% | 21.1 | 31.6 | 36 | 98 |
| 10-13 | 15,734 | 10% | 6% | 21.1 | 31.6 | 37 | 18 |
| 10-14 | 3,725 | 53% | 28% | 21.1 | 32.9 | 36 | 19 |
| 10-15 | 13,527 | 7% | 4% | 21.1 | 30.7 | 40 | 9 |
| 10-16 | 2,471 | 0 | 0 | 1.9 | 0.9 | 87 | 0 |
| 10-17 | 3,989 | 30% | 14% | 21.1 | 31.8 | 36 | 13 |
| 10-18 | 17,279 | 21% | 13% | 21.1 | 31.7 | 37 | 32 |
| 10-19 | 17,280 | 0 | 0 | 21.1 | 5.9 | 81 | 0 |
| 10-20 | 15,695 | 0 | 0 | 21.1 | 6.2 | 83 | 0 |
| 10-21 | 3,458 | 0 | 0 | 15.4 | 0.6 | 88 | 0 |
| 10-22 | 326 | 0 | 0 | 0 | 0.4 | 89 | 0 |
| 10-23 | 1,230 | 0 | 0 | 0 | 0.4 | 93 | 0 |
| 10-24 | 2,233 | 0 | 0 | 16.5 | 2.4 | 88 | 0 |
| 10-25 | 18,000 | 0 | 0 | 21.1 | 8.0 | 77 | 0 |
| 10-26 | 17,280 | 0 | 0 | 21.1 | 3.5 | 83 | 0 |
| 10-27 | 8,352 | 0 | 0 | 21.1 | 4.3 | 85 | 0 |
| 10-28 | 17,280 | 0 | 0 | 21.1 | 2.1 | 85 | 0 |
| 10-29 | 10,516 | 0 | 0 | 19.0 | 14.8 | 5 | 5 |
| 10-31 | 2,664 | 0 | 0 | 21.1 | 3.4 | 87 | 0 |
| 11-01 | 17,280 | 0 | 0 | 21.1 | 2.5 | 85 | 0 |
| 11-02 | 6,032 | 0 | 0 | 10.3 | 1.8 | 88 | 0 |

## Warn onsets

An onset is a sample at warn after one at normal. An episode is an onset with no warn in the 10 minutes before it; the rest are re-crossings, as free memory hovers at the threshold. 206 onsets, 65 episodes.

| day | onsets | free % | compressor GB | used GB | Docker VM GB | containers GB | Tart reserved GB (VMs) | LM Studio GB | unaccounted GB | agents working |
|---|---|---|---|---|---|---|---|---|---|---|
| 10-11 | 12 | 46–56 | 22–28 | 48–50 | 21.1 | 0–2.5 | 32 (2) | 0.6 | 10–12 | 1–5 |
| 10-12 | 98 | 43–49 | 24–29 | 47–52 | 21.0–21.1 | 0–8.5 | 32 (2) | 0.6 | 10–18 | 1–9 |
| 10-13 | 18 | 44–49 | 25–28 | 48–50 | 18.4–21.1 | 0–5.1 | 32 (2) | 0.6 | 12–16 | 1–6 |
| 10-14 | 19 | 43–49 | 26–29 | 49–51 | 21.1 | 0.1–4.3 | 32–33 (2) | 0.6 | 14–19 | 10–16 |
| 10-15 | 9 | 44–54 | 24–28 | 49–50 | 21.1 | 0.2–5.9 | 32 (2) | 0.6 | 11–14 | 2–11 |
| 10-17 | 13 | 45–48 | 26–28 | 49–52 | 21.1 | 0–3.2 | 32 (2) | 0.6 | 12–16 | 1–12 |
| 10-18 | 32 | 44–51 | 25–27 | 48–51 | 21.1 | 0–8.8 | 32 (2) | 0.6 | 12–19 | 2–6 |
| 10-29 | 5 | 5–15 | 5–9 | 57–60 | 0–1.6 | 0 | 0 | 28.7–54.6 (one model) | 8–33 | 0 |

On every warn day but 10-29 the budget reserved about 54 GB (Docker VM 21.1 + two Tart VMs 32 + LM Studio 0.6) and counted another 10–19 GB as unaccounted: 64–73 GB on a 64 GB host, while the kernel had about half its memory free. That is #311.

An episode looks the same each time: free memory sits near 90%, the two Tart VMs boot (63 of the 65 episodes began within 10 minutes of the second VM starting; the other two are 10-29's), and within about 2 minutes the compressor grows from 2–3 GB to 25–27 GB and free memory falls to about 50%. The kernel turns warn there and flips between warn and normal while free memory hovers at 46–50%. Free memory then recovers: in the 30 minutes after each episode's onset it never went below 37%.

## Reading

### What drove each value

- **`lmstudio_idle_gb` 0.7:** LM Studio with no model loaded, p50 0.6, p95 0.7, max 1.4 GB. The same in both windows.
- **`docker_overhead_gb` 21.1 (not taken):** the Docker VM's limit is 20.5 GiB, and it sat at 21.1 GB footprint on most days whatever ran in it: Docker Desktop keeps the VM's high-water mark until Resource Saver stops it. The rule "footprint − containers" learns that mark. The budget reserves `max(overhead + containers, footprint)` already, so the spike value stays (#310).
- **`host_baseline_gb` 23 and `min_headroom_gb` 1 (not taken):** both come from unaccounted memory, which at warn included 25–29 GB of compressor. With the suggested budget, headroom at warn onsets replays to −12.8 GB, so the floor `suggest` reports is just its margin (#311).
- **`per_worktree_cap_gb` 7 (not taken):** p95 of per-worktree daily peaks is 4.9 GB, but a worktree running two 16 GB Tart VMs peaked at 38.9 GB. The 14-day window, with no Tart VMs at all, tops out at 6.3 GB.

### Odd things

- **2026-10-30 is missing.** The last sample on 10-29 is at 21:53 and the next on 10-31 at 11:10, a minute after the Mac booted (`kern.boottime`): the Mac was down, not the daemon.
- **Short days** (10-14, 10-16, 10-17, 10-21 to 10-24, 10-27, 10-31) are the Mac asleep, not a stalled daemon: their samples spread from just after midnight to just before it, in short bursts with gaps of about 20 minutes between them (one of 17.5 hours on 10-24), as the Mac wakes for maintenance. 10-31 starts at the boot. On 10-23 every sample marks `lmstudio` and `orca` stale. 10-25 has 25 hours: the clocks went back.
- **Warn stopped after 10-18** because the Tart VMs did. No Tart VM ran after 10-18; with only the Docker VM, LM Studio and agents, the compressor peaked at 8 GB and free memory stayed at 77% or above.
- **The 10-29 onsets had no agents working.** LM Studio had one model loaded, its footprint 29–55 GB, while the Docker VM was stopped or small and no Tart VM ran. Free memory went down to 5% and swap to 4.5 GB, and pressure stayed at warn: the only onsets where free pages, not compressed VMs, ran short.
- **Critical was never observed.** Not even at 5% free on 10-29.
- **Warn's median free % (47%) is an artefact of compressed VMs.** It records where the compressor filled with two 16 GB Tart VMs' idle pages, not a level at which this Mac runs out of memory. Use it as the warn threshold of this Mac with VMs running, not as a floor for the budget.

## Pressure guard

`pressure_guard = "critical"` stays: critical never happened, so it would never have denied, and it remains the backstop for a Mac that does run out.

`pressure_guard_rising` (deny at warn while the trend is rising) stays true, because the run cannot settle it. #24 asked how often warn plus rising preceded critical. The answer is never, but critical never happened at all, so there is no case to show whether it predicts critical or not. What the run does show is its cost on this Mac:

- 65 warn episodes, none followed by critical; in the 30 minutes after each onset free memory stayed at 37% or above, and on 10-29 it reached 5% and stayed at warn.
- At warn, 79% of samples read rising, because the 5-minute window still holds the drop that caused warn. Free memory then rose a median of 19 points over the next 5 minutes (11 points for the warn samples that did not read rising).
- With the gate on, it would have denied for about 7 hours of the 535 minutes at warn, nearly all of it during the compressed-VM warn of #311; only 32 of the 6,420 warn samples saw swap-outs.

Revisit it with #311, when the budget sees compression and warn means something closer to running short.

## Default costs

`default_container_gb`: 9,855 containers ran (186 images, most of them short-lived). Each container's peak memory: p50 0.19, p75 1.0, p90 1.7, p95 2.8, max 11.6 GB. Per image, the p95 of its containers' peaks has a median of 1.7 GB. It stays 1: that covers three containers in four, and a higher default adds wrong denies on top of #311's overcount, while a container runs inside the Docker VM, whose limit caps what all of them together can take from the host. The run cannot settle a single figure for all images; #36 learns one per image.

`default_tart_gb` stays 4 (tart's own default VM size). It is the cost of a Tart VM whose size the shim cannot read; across 30,371 Tart VM samples the size was always known (560 VMs of 16 GB, one of 4 GB), so the data never exercised the default. It cannot settle it.

## Idle grace

`idle_grace` stays 2m. It covers Orca marking a worktree's agents done or waiting while their tool shells still run. In 2,441 transitions out of working, the worktree's CPU fell below 5% within one tick in 90% of them; p95 5.1 s, p99 5.5 s, max 75 s. 2 minutes covers the longest lag seen. (Agents that leave working and come back do so within 1 minute in 26% of 1,549 cases and within 2 minutes in 40%; those are new turns, not lag.)

## Trend

The free % slope (least squares, as `internal/source/host` computes it) over the window ending at each onset, in %/min:

| window | episodes (65): p50 | ≤ −1 | re-crossings (141): p50 | normal pressure: share ≤ −1 | normal: share within ±1 |
|---|---|---|---|---|---|
| 2 min | −19.4 | 64 | −8.7 | 2.8% | 92.1% |
| 5 min | −7.4 | 60 | +0.6 | 2.3% | 91.9% |
| 10 min | −1.8 | 43 | +0.2 | 2.9% | 92.0% |

- **`steady_slope` stays 1 %/min.** At normal pressure the 5-minute slope's p5 is −0.26 and p95 +1.6, so 1 sits above the noise, and 60 of 65 episodes read beyond it. A threshold of 2 would catch 55, and 0.5 the same 60.
- **`trend_window` stays 5m.** 2 minutes catches every episode but reads re-crossings, which are noise at the threshold, as rising too (108 of 141); 10 minutes catches only 43 of 65, because the drop takes about 2 minutes and the window dilutes it.

## Swap-rate unit (#18)

Open. The host has not swapped since it booted on 2026-10-31 (`vm_stat` Swapins and Swapouts, and `vm.compressor.swapper.swapins_total` / `swapouts_total`, are all 0), and macOS 15 in the Tart VM lacks the swapper totals. No memory was allocated to force swapping, so the daemon's rates could not be compared with `vm_stat` deltas over one window.

The samples from before the reboot still say what the unit probably is. 4,675 samples have a swap rate. Multiplying a sample's rate by its interval gives events; dividing the change in `vm.swapusage` used bytes over the same interval by that gives bytes per event:

| | samples | events | bytes per event, median | p10–p90 |
|---|---|---|---|---|
| swap-outs (swap used grows) | 11 | 79,830 | 16,439 | 12,743–24,476 |
| swap-ins (swap used shrinks) | 107 | 192,151 | 16,251 | 10,224–25,418 |

(Samples with more than 500 events and no event the other way.) That is one event per 16 KiB page, the host's page size (`hw.pagesize` 16384). Swap used shrinks in 8 MB steps, which explains the spread. The field docs (`internal/protocol` `SwapinsPerSec`, `internal/samples`) keep "unverified" until a swap on the host is compared with `vm_stat`.

## Method

Samples were read with `python3 -I` scripts kept outside the samples directory, from the gzipped JSONL day files (`samples/YYYY-MM-DD.jsonl.gz`), v2 samples only. An onset is a v2 sample at `warn` whose predecessor is at `normal`. Slopes are the least-squares slope of `free_percent` against time over the samples in the window, including the onset, skipped when they span less than a minute (as `minTrendSpan`). Container peaks are per container name; Tart sizes are `tart_vms[].memory_bytes`. The idle-grace lag is the time from a worktree's first sample out of `working` to its first with `cpu_percent` below 5, on runs of samples at most 30 s apart.
