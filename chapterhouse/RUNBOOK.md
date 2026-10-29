# Chapterhouse Runbook

Operating chapterhouse in the docker compose stack under
`deploy/docker-compose/`. All commands below run from that directory
unless noted.

The homelab cluster deployment is managed from the separate
`homelab-k3s` repo, not from here.

## Images

Compose builds two images from the repo root on first `up`:

| Image | Dockerfile | Services |
|---|---|---|
| `ghola/ghola:dev` | `Dockerfile.golang` | `ch-init`, `chapterhouse` (ch-api), `chapterhouse-worker`, `ghola`; binary picked via `entrypoint:`. Also carries `ch-admin` |
| `ghola/py:dev` | `Dockerfile.python` | `mentat`, `truthsayer`; app picked via `command:` |

Rebuild after code changes:

```bash
docker compose build
docker compose up -d
```

Postgres is `pgvector/pgvector:pg17`.

## Start and stop

```bash
make dev-up                       # from repo root: core stack, no ghola daemon
scripts/dev-up.sh --with-ghola    # from repo root: + ghola, waits for /health
make dev-down                     # stop, keep volumes
docker compose down -v            # stop and wipe postgres + sietch volumes
```

Without a CUDA GPU, `guild` and `truthsayer` won't start. The smoke
overlay replaces `guild` with a hash-stub embedder; start only the
services you need (truthsayer is optional for ghola, rerank falls back
to RRF):

```bash
docker compose -f docker-compose.yml -f docker-compose.smoke.yml \
  up -d postgres guild mentat ch-init chapterhouse chapterhouse-worker ghola
```

For Apple Silicon see [`docs/deploy-mac.md`](../docs/deploy-mac.md).

## Migrations

`ch-init` runs once per `up` and exits. It:

1. Pings Postgres.
2. Checks for the `ghola` extension. Compose sets
   `GHOLA_EXTENSION_OPTIONAL=true`, so a missing extension only logs a
   warning.
3. Applies `chapterhouse/internal/repository/migrations/*.sql`
   (embedded in the binary) in order, recording each in
   `_migrations.applied`. Already-applied files are skipped. Every
   `${EMBEDDING_DIM}` token is replaced with the `EMBEDDING_DIM` env var,
   which is required.

`chapterhouse` and `chapterhouse-worker` wait for `ch-init` to
complete. To rerun it by hand:

```bash
docker compose run --rm ch-init
```

To add a migration, add the next-numbered file to
`chapterhouse/internal/repository/migrations/` and rebuild the image.
`EMBEDDING_DIM` is baked into vector columns on first apply. Changing it
later needs a fresh database (`docker compose down -v`).

`seed.sql` runs only when the postgres volume is first created. It
enables `pgcrypto` and `vector` and stubs the `semantic` schema.

## Users and API keys

`ch-init` applies the admin schema (`users`, `api_keys`, `audit_log`)
from `chapterhouse/db/migrations/` after the episodic migrations,
recorded in `_migrations` as `admin/<file>`. A database where it was
applied by hand is detected (`api_keys` exists, nothing recorded) and
only marked applied.

`ch-admin` manages users and keys. Every key is `ch_k1_` + 64 hex
chars and is shown once. Create the first admin and its key straight
in Postgres (rerun it to mint a replacement if the key is lost):

```bash
docker compose run --rm --entrypoint /usr/local/bin/ch-admin ch-init \
  bootstrap --username admin
```

Everything else goes through ch-api with a key. From the host:

```bash
go install ./chapterhouse/cmd/ch-admin
export CHAPTERHOUSE_API_KEY=ch_k1_...      # from bootstrap
ch-admin me
ch-admin users create --username alice --email alice@example.com
ch-admin keys create --user <alice-id> --name laptop --expires 90d
ch-admin keys list --all
ch-admin keys revoke <key-id> --admin
ch-admin audit --all limit=20
```

Output is the API's JSON. Give ghola a key as `CHAPTERHOUSE_API_KEY`
and set `CHAPTERHOUSE_REQUIRE_KEY=true`; on ch-api, drop
`AUTH_DISABLED` so `/v1` checks it.

## Backfill over-long sessions

`ch-backfill-sessions` splits one episodic session into segments at
idle gaps (default 4h). It dry-runs by default. `--execute` writes a
JSON backup (`backfill-backup-<session-id>.json` in the working
directory) and then applies the change in one transaction. Run it from
the repo root against the published postgres port:

```bash
export DATABASE_URL='postgres://memory_api:dev@localhost:5432/memories?sslmode=disable'
go run ./chapterhouse/cmd/ch-backfill-sessions --session-id <uuid>            # dry run
go run ./chapterhouse/cmd/ch-backfill-sessions --session-id <uuid> --execute
```

Flags: `--gap-hours`, `--backup <file>`, `--orphan-session <uuid>`
(close a second open session in the same run).

## Consolidation

`chapterhouse-worker` drains the co-activation queue every
`WORKER_TICK_SECONDS`. If `CONSOLIDATE_WORKSPACES` and `MENTAT_URL` are
set, it also runs the episodic→semantic batch nightly at
`CONSOLIDATE_HOUR`. Manual trigger, knobs and reset:
[`docs/consolidation.md`](../docs/consolidation.md).

## Verification

```bash
curl -fsS localhost:8080/health
curl -fsS localhost:8080/ready      # includes a database check
docker compose ps
```

## Backup and restore

```bash
docker compose exec -T postgres pg_dump -U memory_api memories > backup-$(date +%Y%m%d-%H%M%S).sql
docker compose exec -T postgres psql -U memory_api -d memories < backup-<ts>.sql
```

## Troubleshooting

**Logs.** `docker compose logs -f --tail=100 ch-init chapterhouse chapterhouse-worker`

**ch-init exits non-zero.** Check its log. If `EMBEDDING_DIM` is missing
the migration runner fails fast. If the `ghola` extension is missing,
set `GHOLA_EXTENSION_OPTIONAL=true`.

**Semantic recall always returns 0 hits.** `MENTAT_URL` is unset on
`chapterhouse`, or no consolidation has run for the workspace yet.

**Vector dimension mismatch on consolidate.** ch-api reads
`EMBEDDING_DIMENSIONS` and ch-init reads `EMBEDDING_DIM`. Both must
match the embedder (1024 for Qwen3-Embedding-0.6B). Compose sets both
from `EMBEDDING_DIM`.

**ch-admin returns 401.** The key is missing, revoked, expired, or
belongs to a deactivated user. `/v1/admin/*` returns 403 for a key
whose user isn't an admin.
