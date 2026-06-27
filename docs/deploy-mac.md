# Deploying ghola on a Mac (Apple Silicon)

A full, independent ghola stack on a Mac laptop. The CPU services run in Docker
Desktop; the two accelerated services run **natively** on macOS because Docker
Desktop has no Metal/GPU passthrough. Same models as the workstation
(Qwen3-Embedding-0.6B, 1024-d; bge-reranker-v2-m3), so recall behavior is
comparable to prod.

```
native (Metal)                     docker compose (CPU)
┌────────────────────────┐         ┌────────────────────────────────────┐
│ llama.cpp  :8082        │◄────────│ chapterhouse, ghola daemon          │
│ truthsayer :8085 (mps)  │  host.  │ postgres, ch-init, worker, mentat   │
└────────────────────────┘ docker. └────────────────────────────────────┘
                           internal
```

## 1. Prereqs (one-time)

```bash
# Docker Desktop for Mac must be running.
brew install llama.cpp uv
brew install huggingface-cli      # or: pipx install huggingface_hub[cli]

# Fetch a GGUF of the embedder into ~/.ghola-models/
mkdir -p ~/.ghola-models
huggingface-cli download <repo>/Qwen3-Embedding-0.6B-GGUF <file>.gguf --local-dir ~/.ghola-models
#   The launch script defaults to ~/.ghola-models/Qwen3-Embedding-0.6B-Q8_0.gguf;
#   override with GHOLA_EMBED_GGUF=/abs/path/to/file.gguf if the name differs.

# Reranker deps (native venv)
cd /path/to/ghola/truthsayer && uv sync
```

## 2. Start the native services first (each in its own terminal / tmux window)

```bash
scripts/mac/run-embedder.sh      # llama.cpp, Qwen3-Embedding-0.6B, :8082
scripts/mac/run-truthsayer.sh    # truthsayer on MPS, bge-reranker-v2-m3, :8085
```

Verify they are up:

```bash
curl -fsS http://localhost:8082/health && echo                 # embedder
curl -fsS http://localhost:8085/v1/health && echo              # reranker
```

## 3. Pooling-correctness gate (one-time — do NOT skip)

A wrong pooling mode yields embeddings with no semantic structure and recall
silently degrades. Confirm related strings score far higher than unrelated:

```bash
python3 - <<'PY'
import json, urllib.request
def embed(text):
    req = urllib.request.Request(
        "http://localhost:8082/v1/embeddings",
        data=json.dumps({"model": "qwen3-embedding", "input": [text]}).encode(),
        headers={"content-type": "application/json"})
    return json.load(urllib.request.urlopen(req))["data"][0]["embedding"]
def cos(a, b):
    import math
    d = sum(x*y for x, y in zip(a, b))
    na = math.sqrt(sum(x*x for x in a)); nb = math.sqrt(sum(y*y for y in b))
    return d/(na*nb)
a = embed("the database listens on port 9931")
b = embed("which port does the database use")          # related
c = embed("a recipe for sourdough bread")              # unrelated
print("related  :", round(cos(a, b), 3))
print("unrelated:", round(cos(a, c), 3))
assert cos(a, b) > cos(a, c) + 0.1, "pooling looks wrong — related not clearly closer"
print("POOLING OK")
PY
```

## 4. Bring up the stack

```bash
cd deploy/docker-compose
docker compose -f docker-compose.mac.yml up -d
```

First run builds the `chapterhouse`, `ghola`, and `mentat` images for arm64.
Wait for chapterhouse to go healthy:

```bash
docker compose -f docker-compose.mac.yml ps
```

## 5. Smoke test

```bash
scripts/mac/smoke.sh
# expect: SMOKE OK: non-degraded, marker retrievable
```

## 6. The user id

The stack uses a fixed default user. Set it once and reuse the same value as
pi-mono's `GHOLA_USER_ID`:

```bash
export DEFAULT_USER_UUID=00000000-0000-0000-0000-000000000001   # or your own uuid
# (re-`up` the stack if you change it; chapterhouse + ghola read it at start)
```

## 7. Teardown / reset

```bash
docker compose -f docker-compose.mac.yml down       # stop containers, keep data
docker compose -f docker-compose.mac.yml down -v    # also wipe the store (postgres + sietch)
# stop the native services with Ctrl-C in their terminals.
```

## Troubleshooting

- **chapterhouse/ghola logs show "connection refused" to the embedder/reranker** —
  the containers can't reach the native host services. Re-run them bound to all
  interfaces: `GHOLA_EMBED_HOST=0.0.0.0 scripts/mac/run-embedder.sh` and
  `TRUTHSAYER_HOST=0.0.0.0 scripts/mac/run-truthsayer.sh`.
- **`degraded` non-empty in recall** — a tier failed. `embed` means the embedder is
  unreachable or erroring; check `localhost:8082`. Rerank failure is non-fatal
  (falls back to RRF) and does not appear in `degraded`.
- **First recall slow / reranker downloading** — bge-reranker-v2-m3 (~600 MB) and
  the GGUF download on first use; subsequent runs are cached.
- **arm64 build issues** — the compose stack builds from `Dockerfile.chapterhouse`
  and `Dockerfile.ghola` (multi-arch). The `--platform=linux/amd64`-locked
  `_chapterhouse/ch-server/Dockerfile` is NOT used by this compose file.
