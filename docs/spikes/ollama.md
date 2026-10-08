# Spike: Ollama footprint, idle and with a model loaded

Issue: #59 · Ollama 0.40.1 (Homebrew) · macOS 27.0.1, M4 Max, 64 GB · 2026-10-08

**Partial.** The outbound firewall blocks `ollama pull` (`dial tcp …:443: i/o timeout` to `registry.ollama.ai`), and this Mac has no GGUF file to `ollama create` from (LM Studio holds MLX models only). The loaded-model measurement below is still open.

## Answer so far

- **The idle server costs about 12–13 MB** (`phys_footprint` of `ollama serve`: 12 MB at start, 13.2 MB after a few `/api/ps` reads). The `ollama_idle_gb` default of 0.1 covers that with room for the Ollama.app menu-bar process, which this install doesn't have.
- **`/api/ps` with nothing loaded** returns `{"models":[]}`. headroom reads only this endpoint, and only while a server process runs. With the server stopped, the daemon reports `installed: true, running: false`, and nothing listens on 11434.

## Processes

| Process | comm | Args | Parent |
|---|---|---|---|
| Server | `ollama` | `…/bin/ollama serve` | launching shell, or launchd for `brew services` |
| Runner, one per loaded model | `ollama` | `…/bin/ollama runner --model … --port …` | server (per Ollama's source; not observed here) |
| CLI client (`ollama run`, `ollama ps`) | `ollama` | `ollama run …` | a terminal; **not** part of the server's cost |

The kernel's `p_comm` is `ollama` for all three, so the source picks out the server by `argv[1] == "serve"` and measures its whole process tree.

## To finish

Once `ollama` can reach the registry (a LuLu rule for `/opt/homebrew/Cellar/ollama/*/bin/ollama`):

```sh
ollama serve &
ollama pull llama3.2:1b
curl -s localhost:11434/api/generate -d '{"model":"llama3.2:1b","prompt":"hi","stream":false,"options":{"num_predict":1}}' >/dev/null
curl -s localhost:11434/api/ps                       # size, size_vram, context_length, expires_at
pgrep -x ollama | xargs -n1 footprint | grep phys_footprint:
```

Record `size` against the server tree's footprint here, and replace `internal/source/ollama/testdata/ps-one.json` with the real response (scrub the digest). A `keep_alive: -1` load should also confirm the far-future `expires_at` (expected in 2318) that the source treats as "never".
