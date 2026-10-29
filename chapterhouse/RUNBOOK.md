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
| `ghola/ghola:dev` | `Dockerfile.golang` | `ch-init`, `chapterhouse` (ch-api), `chapterhouse-worker`, `ghola`, `ch-web`; binary picked via `entrypoint:` |
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
docker compose --profile web up -d ch-web   # admin UI on :8090
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

## Admin console and API keys

ch-web and API keys use the admin schema (`users`, `api_keys`,
`admin_sessions`, `audit_log`) from `chapterhouse/db/migrations/`.
`ch-init` applies it after the episodic migrations, recorded in
`_migrations` as `admin/<file>`. A database where it was applied by hand
is detected (`api_keys` exists, nothing recorded) and only marked
applied. There is no default admin user, so create one:

```bash
docker compose exec -T postgres psql -U memory_api -d memories -c "
  INSERT INTO users (id, username, email, display_name, password_hash, is_admin)
  VALUES ('00000000-0000-0000-0000-000000000001', 'admin', 'admin@localhost',
          'Administrator', crypt('CHANGE_ME', gen_salt('bf', 10)), true)
  ON CONFLICT (username) DO UPDATE SET
    password_hash = EXCLUDED.password_hash, is_admin = true;"
```

`chapterhouse/db/seed_admin.sql` does the same with a fixed dev
password (`admin123`). Don't use it outside local dev.

Then log into ch-web (`http://localhost:8090`) and create API keys
(`ch_k1_` + 64 hex chars, shown once). Pass one to ghola as
`CHAPTERHOUSE_API_KEY`.

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

**ch-web login returns 401 or 500.** No admin user exists (see above),
or the session cookie was dropped. Cookies are `Secure`
unless `ENVIRONMENT` is `local` or `development`, so a browser discards
them over plain HTTP or a self-signed cert. Compose sets `local`.

**Clipboard copy fails in ch-web.** `navigator.clipboard` needs a
secure context. Same cause as the cookie issue.
