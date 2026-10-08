# Spike: Ollama footprint, idle and with a model loaded

Issue: #59 · Ollama 0.40.1 (Homebrew) · macOS 27.0.1, M4 Max, 64 GB · 2026-10-08

## Answer

- **The idle server costs about 12–45 MB** (`phys_footprint` of `ollama serve`). It was 12 MB at start and 45 MB after a model had been loaded once. The `ollama_idle_gb` default of 0.1 covers this.
- **A loaded model's `size` overstates what it uses, so reserving `size` is conservative.** For `llama3.2:1b` (Q8_0, 1.2B parameters) with Ollama's default 131072-token context, `/api/ps` reported `size` 6,474,380,083 B (6.0 GiB), all of it `size_vram`. The server tree's footprint was 4.1–4.4 GiB: the server at 45 MB plus its `llama-server` runner at 4,180 MB. `size` counts the context's whole KV cache, and a short prompt touches only part of it. The budget reserves `max(idle + Σ size, footprint)`, which is 6.1 GiB here.
- **`keep_alive: -1` reports `expires_at` about 292 years ahead** (`2319-01-18…`), not a zero time. The source treats an expiry more than 100 years away as "never".
- **`/api/ps` with nothing loaded** returns `{"models":[]}`. headroom reads only this endpoint, and only while a server process runs. With the server stopped, the daemon reports `installed: true, running: false`, and nothing listens on 11434.

## Processes

| Process | comm | Args | Parent |
|---|---|---|---|
| Server | `ollama` | `…/bin/ollama serve` | launching shell, or launchd for `brew services` |
| Runner, one per loaded model | `llama-server` | `…/libexec/lib/ollama/llama-server --model … --port …` | server |
| CLI client (`ollama run`, `ollama ps`) | `ollama` | `ollama run …` | a terminal; **not** part of the server's cost |

The source finds the server by `comm == "ollama"` and `argv[1] == "serve"`, then measures its whole process tree, runners included.

## Method

```sh
ollama serve &
ollama pull llama3.2:1b
curl -s localhost:11434/api/generate -d '{"model":"llama3.2:1b","prompt":"hi","stream":false,"options":{"num_predict":1}}' >/dev/null
curl -s localhost:11434/api/ps
ps -axo pid,ppid,ucomm,command | grep -i ollama
footprint <pid>                                       # per process: phys_footprint
curl -s localhost:11434/api/generate -d '{"model":"llama3.2:1b","keep_alive":-1}'   # expiry for "never"
```

`internal/source/ollama/testdata/ps-one.json` is the response captured here, with the digest zeroed.

Note: the first attempt to pull timed out (`dial tcp …:443: i/o timeout`) until the outbound firewall allowed `ollama`.
