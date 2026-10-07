# Success metrics

## Success metrics

Measure a two-week baseline with observe mode only, then two weeks with the gate on. Targets below are starting hypotheses to revise after the baseline.

| Metric | Type | Target | How measured |
| --- | --- | --- | --- |
| Minutes per day in red memory pressure | Leading | Down 80% vs. baseline | Daemon samples pressure every interval |
| Swap used during multi-agent runs | Leading | Under 2 GB peak | Daemon swap samples |
| Attributed share of reserved memory | Leading | Over 95% | Reserved GB under worktrees ÷ total reserved |
| Agent recovery after a deny | Leading | Over 70% reuse, stop or wait instead of failing | Audit log + next command from that worktree |
| Tart "too many VMs" failures | Leading | Zero | Audit log + Tart errors |
| Orphaned resources older than 1 hour | Lagging | Zero at any time | Unattributed row age |
| Gate false denies | Lagging | Under 1 per day | Denies where actual peak later fit in headroom |
| Parallel agents sustained without thrash | Lagging | Up from baseline, ideally 4+ | Max concurrent working agents with pressure not red |

