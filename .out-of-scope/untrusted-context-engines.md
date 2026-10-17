# Keeping an untrusted engine's last reading

The Docker source reads every Docker-API socket it can find. Docker Desktop's socket and `DOCKER_HOST` are trusted; a socket reached only through a Docker context is not, since a context is a file anything may write. The untrusted sockets are read last, together, within one second. One that errors or runs out of time counts as an engine that is not running, with its error, for that tick: it reserves nothing, and its previous reading is dropped. The source as a whole is not marked stale.

headroom does not keep that engine's last good reading for a few ticks instead.

## Why this is out of scope

The current rule is a deliberate design (#227, amendments 3 and 4): one slow or broken context must not hide the other engines or hold up the reading. `TestASlowContextIsNotRunning` and `TestAContextThatNeverAnswersIsNotRunning` pin it. Keeping an old reading would trade a one-tick wrong allow for an engine whose figures may be stale, and add per-socket state to the source.

The engines this would help are not used here. On the dev Mac (2026-10-17), both Docker contexts lead to Docker Desktop's socket (`default` through the `/var/run/docker.sock` link), so no engine is untrusted; there is no OrbStack, Rancher Desktop or Colima, and `DOCKER_HOST` and `DOCKER_CONTEXT` are unset. The samples have only ever recorded two sockets, Docker Desktop's and the Podman machine's, both trusted. (The daemon log cannot show such a failure: an untrusted engine's error is kept only on the engine, in `headroom status --json` under `engines[].error`, and is not logged.) Colima, OrbStack and Rancher Desktop stay out of scope until they are used (#49).

Docker Desktop is not affected: its socket is trusted, and a trusted engine whose reading fails, other than by a missing or refusing socket, fails the whole reading, so the last good one stands.

## When to revisit

Another local engine reached through a Docker context is in use here (Colima, OrbStack or Rancher Desktop; a context's TCP or SSH endpoint is a remote engine and never read), and `headroom status --json` shows it with an `error`. Then keep each untrusted socket's last good engine for a bounded number of ticks, marked with its error, without making the whole source stale.

## Prior requests

- #280: keep an untrusted context engine's last good reading for a few ticks (claim 1 of #267)
