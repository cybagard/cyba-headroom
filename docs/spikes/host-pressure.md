# Host memory pressure: signals and real-load check

Issue: #18 · macOS 27 (host, 64 GB) and macOS 15.7.7 (Tart VM, 4 GB) · 2026-10-07

## Signals (sysctl, no sudo, no cgo)

| Signal | sysctl | macOS 15 | macOS 27 |
|---|---|---|---|
| Pressure level (1 normal, 2 warn, 4 critical) | `kern.memorystatus_vm_pressure_level` | ✅ | ✅ |
| Free % | `kern.memorystatus_level` | ✅ | ✅ |
| Swap used/total | `vm.swapusage` (`struct xsw_usage`) | ✅ | ✅ |
| Swap-in/out totals | `vm.compressor.swapper.swap{ins,outs}_total` | ❌ absent | ✅ |
| Memory used (#19): anonymous − purgeable + wired pages, + compressor | `vm.page_pageable_internal_count`, `vm.page_purgeable_count`, `vm.page_wired_count`, `hw.pagesize`, `vm.compressor_bytes_used` | ❌ no `vm.page_wired_count` | ✅ |
| Compressor contents at full size (#19), = `vm_stat` "Pages stored in compressor" | `vm.compressor.pages_compressed_incore` × `hw.pagesize` | ❌ absent | ✅ |

On macOS 15 the swap totals and the wired page count exist only through `host_statistics64`, which `vm_stat` uses and which needs cgo. headroom leaves the swap rates and memory used out there; swap used and the pressure level still work. One collection takes ~0.13 ms. The page counters mix 32- and 64-bit widths (`vm.page_purgeable_count` is 64-bit on macOS 27, the others 32), so headroom reads either width.

## Real-load check (Tart VM, 4 GB)

`sudo memory_pressure -l warn` allocates memory until the kernel reaches warn. With `headroom daemon` running (5 s interval):

| Time | Pressure | Free % |
|---|---|---|
| baseline | normal | 74 |
| +10 s … +40 s | normal | 59 → 42 |
| +50 s | **warn** | 40 |
| released | normal | 68 (trend keeps the warn episode for 5 min) |

- The kernel switched to warn at about **40% free** in this VM. #23 should record where the switch happens on the 64 GB host, under real agent load.
- `memory_pressure -S` (simulate) sends only a brief notification. Simulated warn never showed in the level sysctl; simulated critical showed for a single sample. Use real allocation (no `-S`) to test pressure handling.

## Compressed memory in footprints (#19)

The question was whether `phys_footprint` counts compressed pages at their full size or at their compressed size. In the Tart VM (macOS 15, 4 GB), a Perl process held 1.5 GiB of a repeating pattern (a 3075 MiB footprint), and `sudo memory_pressure -l critical` then forced it out of RAM:

| | Process footprint | Process RSS | Compressor holds | Compressor occupies |
|---|---|---|---|---|
| before | 3075 MiB | 641 MiB | 3656 MiB | 852 MiB |
| under pressure, 40 s | 3075 MiB | 0 MiB | 8705 MiB | 1969 MiB |

`phys_footprint` counts compressed pages at **full size**, while "Memory Used" counts the compressor at its compressed size. So `used − Σ footprints` reads low by every byte the compression saves. `Budget.UnaccountedBytes` therefore replaces the compressor's occupied bytes with the full size it holds before subtracting footprints. Pages swapped out to disk are still not covered (#23 checks that under swap).
