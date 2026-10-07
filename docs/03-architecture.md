# Architecture

## Architecture

One daemon reads every source and owns the budget; all clients are thin and ask the same daemon.

```mermaid
flowchart LR
  subgraph Sources
    D[Docker / Podman API]
    T[Tart CLI]
    L[LM Studio API]
    O[Orca CLI + status hooks]
    H[Host memory pressure]
  end
  subgraph Daemon[headroom daemon]
    X[Collect all sources<br/>Attribute by worktree<br/>Budget: reserved, used, free<br/>Policy: allow, wait, deny]
  end
  subgraph Clients
    W[headroom --watch<br/>observe view, human]
    S[Gate shims<br/>docker, podman, tart]
    A[Agent hooks<br/>P1, early warning]
    V[Advise<br/>P1, orca terminal send]
  end
  D & T & L & O & H --> X
  X --> W & S & A & V
```

Only the gate shims change what happens: they exec the real binary on allow, or hand the agent a reason on wait or deny. If the daemon is unreachable, they exec straight through.

### Enforcement layers

Defense in depth: one authoritative gate; the other layers set up, warn or detect.

1. **Orca launch environment (setup).** Puts the shims first on PATH and sets `HEADROOM_WORKTREE` for every agent (R9). Not a gate, but it fixes the shim's two weak spots: PATH order and identity.
2. **Gate shim (enforce).** The only authoritative decision, at the exec level, for every agent and however deeply nested the call (R5, R10).
3. **Agent hooks (warn, P1).** Deny obvious cases before the agent spends effort, with the reason fed to the model, and add session-level attribution. Advisory, since they only see the command string.
4. **Observe mode (detect).** Flags anything that got around the first three, such as direct Docker socket use, as ungated (R4).

