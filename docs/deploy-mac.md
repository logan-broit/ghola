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

## Start without models (Podman)

Use this path when model downloads are unavailable. It starts Postgres, the
migration runner, chapterhouse, its worker, mentat, and ghola. No Hugging Face
access, embedder, reranker, or model weights are needed. Container images and
normal Go/Python packages must still be downloadable. With a running Podman VM
and `podman-compose` installed, run from the repository root:

```bash
podman compose -p ghola \
  -f deploy/docker-compose/docker-compose.mac.yml \
  -f deploy/docker-compose/docker-compose.no-models.yml up -d --build
GHOLA_NO_MODELS=1 scripts/mac/smoke.sh
```

If a corporate proxy inspects package-download HTTPS, supply an existing,
approved CA bundle as a build secret. For example, this laptop's bundle is at
`~/.config/certs/combined-ca.pem`:

```bash
podman compose \
  --podman-build-args="--secret=id=corporate_ca,src=$HOME/.config/certs/combined-ca.pem" \
  -p ghola \
  -f deploy/docker-compose/docker-compose.mac.yml \
  -f deploy/docker-compose/docker-compose.no-models.yml up -d --build
```

The bundle is mounted only during build steps, not checked into Git or copied
into the images. TLS verification remains enabled. This does not make blocked
model hosts accessible; no model downloads are attempted in this mode.

Ghola listens at `http://127.0.0.1:7421`; chapterhouse at `:8080` and mentat at
`:8084`. Published ports remain loopback-only. An explicitly empty
`EMBEDDING_URL` disables embedding requests; an unset variable still uses its
normal default. `TRUTHSAYER_URL` is empty too, so reranking is disabled.

Recording and current-session keyword recall work. Recall reports
`degraded: ["embed"]`, which is expected in this mode. Unembedded text is kept
in the persistent `ghola-sessions` SQLite volume, **not yet consolidated to
Postgres**. Idle session expiry is disabled so cwd-scoped keyword recall keeps
finding that text. Explicitly ending a session still removes it from implicit
current-session recall; pass its session ID to search it until embeddings are
available. Semantic search and semantic consolidation are not useful yet.

To stop without deleting recorded data, run from the repository root:

```bash
podman compose -p ghola \
  -f deploy/docker-compose/docker-compose.mac.yml \
  -f deploy/docker-compose/docker-compose.no-models.yml down
```

To add models later, start and verify the native services described below
(using approved model sources), then omit the no-models overlay while keeping
project name `ghola`. The startup encoding worker will backfill embeddings and
drain buffered events into Postgres. Keep the model at 1024 dimensions. Do not
use `down -v`: that deletes both the buffered text and the database.

```bash
# From the repository root, after native :8082 and :8085 are ready:
podman compose -p ghola -f deploy/docker-compose/docker-compose.mac.yml up -d
scripts/mac/smoke.sh
```

## Non-root receiver and existing-volume upgrades

The receiver image runs as UID/GID `10001:10001`. Both Compose manifests mount
`ghola-sessions` at `/var/lib/ghola/sessions`, selected by
`GHOLA_SESSIONS_DIR`. Under project `ghola`, the existing volume remains
`ghola_ghola-sessions`; changing its container destination does not replace it.
Rootless Podman alone does not make the application a non-root container user.

Before upgrading a legacy root-owned volume, preserve the old image and
configuration, stop only the receiver, and export a cold backup. Validate the
archive, SQLite integrity, and record counts before changing ownership. Apply
UID/GID `10001:10001` only to that verified volume, through `podman unshare` in
the owning Podman VM/user namespace. Do not recursively chown the storage
parent, use `down -v`, or replace the volume. Recreate only `ghola` with
`--no-deps --no-build`, then check its UID, volume identity, loopback port,
existing data, and other containers' IDs/start times.

Inspect native container dependencies **before any recovery restart**. During
the 2026-09-17 migration, restarting the old receiver after a backup-reader
failure also restarted the completed `ch-init` container. It exited zero and
the migration ledger confirmed no new migrations. The later `--no-deps`
receiver recreation did not restart other services; the new receiver has no
native Podman dependencies. Do not assume that starting one legacy container
has no dependency side effects.

The host Apple SQLite 3.51 CLI rejected a plain read-only open of the cold
WAL-mode backup. An immutable URI read passed integrity checks on the same
bytes, as did a writable copy. Use immutable reads only for verified cold,
checkpointed copies without a nonempty WAL—not for live databases.

After building the receiver image, this isolated check creates and removes
only its own temporary container/volume. It checks non-root writes, replay,
restart persistence, and complete Unicode text without touching deployed data:

```bash
node scripts/mac/check-receiver-image.mjs localhost/ghola/ghola:dev
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
# expect: SMOKE OK: marker retrievable; degraded=[]
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

Attribution: Pi (no-model startup, non-root receiver migration, and smoke checks).
