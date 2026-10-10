# Spike: the Docker VM's clock, as an events replay needs it

Issue: #170 · Docker Desktop 29.8.1 (API 1.56) · Apple Silicon Mac, macOS 27 · 2026-10-10

## Answer

**`/info` `SystemTime` is the events' clock, to the nanosecond, and costs a few ms to read, so one reading dates a replay to within a few ms. A bounded replay (`since` and `until`) ends as soon as it has sent its events, `until` included. No probe gave a stop result.**

| Probe | Question | Result | Verdict |
|---|---|---|---|
| P1 | `/info` `SystemTime`: precision and clock | RFC 3339 with 9 fraction digits (`2026-10-10T19:35:00.141843009Z`). A container's start and die `timeNano` came 62 ms and 18 ms before the next `SystemTime`. Host midpoint − `SystemTime` was 1.7 to 2.4 ms over 3 reads. | confirms |
| P2 | `/info` RTT, idle and under 20 parallel runs | Idle (60 reads): p50 2.96 ms, p95 3.57 ms, max 8.4 ms. Under 20 parallel `docker run --rm alpine sleep 2` (128 reads over 4 s): p50 4.09 ms, p95 9.74 ms, max 138 ms. | confirms |
| P3 | Does `since&until` end after the buffered events? Is `until` inclusive? | With `until` = a `SystemTime` just read, the stream sent its 6 events and closed in 1 ms. `until` = an event's own `timeNano` includes that event; `until` = it − 1 ns leaves it out: **inclusive**. With `until` 2 s in the VM's future, the stream stays open until then (2.008 s). | confirms |
| P4 | `until < since` | `400 Bad Request`, empty body, in 1 ms. | confirms |
| P5 | `/_ping` `Date` | `Sat, 10 Oct 2026 19:35:00 GMT`: whole seconds only. | as expected |
| P6 | The largest stamp-to-publish inversion, under 50 parallel `run -d` / `kill` / `rm` | 150 events per run (start, kill, die × 50). Run 1: no inversion. Run 2: 1 inversion, 0.327 ms. | confirms |

What the design takes from it:

- **The offset:** one `/info` read dates the VM's clock to within half its RTT. Even the slowest read under load (138 ms) is within the 250 ms bound, and the best of 3 reads is a few ms.
- **`U` must be in the VM's past.** A `until` ahead of the VM's clock holds the stream open until the VM reaches it (P3). `U` is the `SystemTime` just read, so the bounded call ends at once.
- **`until` is inclusive (P3).** An event stamped `U` comes in the bounded call. The live stream asks from `max(U, newest) − 1 s`, so it may bring it again, and the dedupe drops it.
- **A `400` on a bounded call is also what `until < since` gets (P4).** `Events` reads any 400 with a `since` as `ErrBadSince`. `followEvents` checks for a step-back (`U < S`) before the bounded call, so the call is never made with `U < S`. A 400 there falls back to #146's single call, which handles `ErrBadSince` as before.
- **`/_ping` `Date` is too coarse (P5)** to date a replay: it has whole seconds, and a replay's order matters at well under a second.
- **Docker publishes events nearly in time order (P6).** The worst inversion seen was 0.3 ms, far inside the 1 s a replay reaches back.

## Method

- Every request went to the Docker Engine API on `~/.docker/run/docker.sock`, over a Unix-socket HTTP client in Python (`http.client`); it makes the same requests as `curl -s --unix-socket`. Host time was `time.time_ns()`, which reads the clock that `perl -MTime::HiRes=time` reads, but at full nanosecond precision.
- Containers were started with the Docker CLI by its full path, not through the headroom shim, so the installed agent saw none of them. The image was `alpine:3`.
- Events were filtered as headroom filters them: `type=container` and `event` in start, stop, kill, die.
- P1 read `SystemTime` 3 times, ran `docker run --rm alpine:3 true`, read `SystemTime` again, and fetched the run's events from the first reading to the second.
- P2 timed `/info` 60 times in a row idle. It then started 20 `docker run --rm alpine:3 sh -c 'sleep 2'` at once, and timed `/info` every 20 ms for 4 s.
- P3 ran 3 containers between two `SystemTime` reads, then fetched `since`..`until` with `until` set to: the second reading, the last event's `timeNano`, that − 1 ns, and the VM's time + 2 s.
- P4 sent `since` = now and `until` = now − 1 s.
- P6 followed a live stream while 50 threads each ran `docker run -d alpine:3 sleep 60`, then `docker kill` and `docker rm -f` on it, and recorded the arrival order of `timeNano`. An inversion is an event stamped before one that arrived earlier; its size is how much earlier. It was run twice.

## Smoke test of the replay

The branch build ran as a second daemon, with `HEADROOM_CONFIG_DIR` set to a short `/tmp` dir and `[docker] socket` set to a small Unix-socket proxy to Docker's socket. A shim dir with `docker → bin/headroom` came first on PATH. A one-service compose project (`alpine:3`, `sleep 600`) was brought up with the proxy running. The proxy was then killed, the project stopped and brought up again through the shim, and the proxy restarted:

```text
22:00:52.188 up (proxy alive)
22:00:55.596 kill proxy
22:00:56.616 stop
22:00:59.761 up
22:01:00.017 up done
22:01:01.041 proxy restarted
```

The daemon's log (container ID shortened):

```text
22:00:56.601 INFO "docker events replayed as before" reason="… connect: connection refused"
22:00:58.602 INFO "docker events replayed as before" reason="… connect: connection refused"
22:01:02.622 INFO "docker events replay" offset=2.235385ms since=22:00:51.567 until=22:01:02.611
22:01:02.624 INFO "docker event replayed" action=kill  id=670c46fe07ab at=22:00:57.603
22:01:02.624 INFO "docker event replayed" action=kill  id=670c46fe07ab at=22:00:59.679
22:01:02.624 INFO "docker event replayed" action=stop  id=670c46fe07ab at=22:00:59.742
22:01:02.624 INFO "docker event replayed" action=die   id=670c46fe07ab at=22:00:59.744
22:01:02.624 INFO "docker event replayed" action=start id=670c46fe07ab at=22:00:59.999
```

Each replayed event is dated inside the gap, in the order the stop and the up ran, and not at the reconnect (22:01:02.6), as #146 dated it. While Docker could not be reached, each reconnect fell back as before and said why.
