# Chapterhouse Security Statement

Security posture of the chapterhouse API (`chapterhouse/cmd/ch-api`)
and its admin CLI (`chapterhouse/cmd/ch-admin`). Chapterhouse stores agents'
conversation history (episodic tier) and distilled memories (semantic
tier), so confidentiality between users and workspaces is the main
concern.

Chapterhouse has no MCP endpoint. Agents reach it through the ghola
daemon, and `ghola-mcp` is a stdio bridge to that local daemon
(`internal/mcp/server.go`), so no network MCP sessions exist to
protect.

---

## Threat Model

1. **Cross-user access**: a user reads or deletes another user's events
2. **API key compromise**: a stolen key grants a user's access
3. **Credential exposure**: database or embedder credentials leak
4. **Injection**: SQL injection or malformed input corrupts data or leaks
   internals

---

## 1. Authentication

### API Keys

- **Format**: `ch_k1_` + 32 random bytes, hex-encoded (64 characters)
- **Storage**: SHA-256 hash only; plaintext is shown once at creation
- **Validation**: Prefix, length and hex are checked before any database
  lookup
- **Source**: `chapterhouse/internal/auth/apikey_provider.go`

SHA-256 is fine for 256-bit random keys; they aren't open to
dictionary attacks.

### Authentication

API keys are the only credential. There are no passwords or cookie
sessions.

- `/v1/episodic/*` and `/v1/semantic/*` (the memory API ghola calls)
  require a key. With `AUTH_DISABLED=true` (what the compose stack sets
  for local dev) ch-api skips the check and serves every request as
  `AUTH_DEFAULT_USER`. ch-api refuses to start with `AUTH_DISABLED=true`
  and `ENVIRONMENT=production`.
- `/v1/me`, `/v1/keys`, `/v1/audit` always require a key and act on the
  key's own user.
- `/v1/admin/*` always requires a key whose user has `is_admin`.
- `/health` and `/ready` need no auth.

The first admin key comes from `ch-admin bootstrap`, which writes to
Postgres directly and so needs database credentials, not a key.

**Source**: `chapterhouse/cmd/ch-api/main.go`,
`chapterhouse/internal/middleware/middleware.go` (`RequireAdmin`),
`chapterhouse/internal/config/config.go`,
`chapterhouse/cmd/ch-admin/main.go`

---

## 2. Data Access Control

- **Episodic tier**: rows carry `user_id`. Query, ingest and share
  handlers require the request's `user_id` to match
  the authenticated caller (400 or 403 otherwise). Forget deletes only rows
  `WHERE user_id = caller`. Queries are also scoped by `workspace_id`
  through `episodic.session_workspaces`. The isolation invariant is
  pinned by `chapterhouse/internal/repository/workspace_isolation_test.go`.
- **Shares**: only the owner (`owner_user_id` = caller) can share.
- **Semantic tier**: mnemes are shared per workspace by design.
  `/v1/semantic/query` and `/v1/semantic/consolidate` require an
  authenticated caller and a `workspace_id`. They don't check per-user
  membership in that workspace.

**Source**: `chapterhouse/internal/handler/episodic.go`,
`chapterhouse/internal/handler/semantic.go`,
`chapterhouse/internal/repository/`

---

## 3. API Security

### Rate Limiting

None. `/v1` has no limiter.

### Input Validation

- Admin and user routes decode JSON with `DisallowUnknownFields()`
  (`handler.DecodeJSON`). `/v1` handlers use a plain decoder and
  validate required fields and UUIDs explicitly.
- Admin pagination is capped at 1000.
- Server read and write timeouts are 30 seconds by default
  (`SERVER_READ_TIMEOUT`, `SERVER_WRITE_TIMEOUT`).

### CORS

Router-level middleware with an origin allowlist (`CORS_ORIGINS`). Empty
by default, which allows no cross-origin requests.

### Admin Authorization

Stats, audit logs and user and key management (`/v1/admin/*`) need a
key whose user is an admin (`RequireAdmin`). Other users get only
`/v1/me`, `/v1/keys` and `/v1/audit` for their own account.

---

## 4. Database Security

- **Driver**: pgx
- **SSL mode**: `prefer` by default. Set `DATABASE_SSL_MODE=require` or
  `verify-full` outside local dev. The compose stack uses `disable` on
  its private network.
- **Pool**: 5–25 connections, 1h lifetime, 30m idle (`DATABASE_*`
  overrides)
- **Queries**: parameterized (`$1`, `$2`, ...) throughout, via hand-written
  pgx SQL in `repository` and sqlc-generated code for the admin schema
- **Source**: `chapterhouse/internal/config/config.go`,
  `chapterhouse/internal/repository/`

---

## 5. Audit Logging

Admin actions go to the `audit_log` table (IP, user agent): user
create/update/deactivate/reactivate and API key create/revoke,
including keys minted by `ch-admin bootstrap`. `/v1` memory operations aren't audited. Every HTTP request
is logged as structured JSON (request ID, method, path, status,
duration, response size, user ID, remote address).

**Source**: `chapterhouse/internal/handler/admin.go`,
`chapterhouse/internal/middleware/middleware.go`,
`chapterhouse/db/migrations/001_initial_schema.sql`

---

## 6. Deployment

The repo ships a local docker compose stack (`deploy/docker-compose/`).
It isn't production-shaped: no TLS, default `POSTGRES_PASSWORD=dev`,
`AUTH_DISABLED=true`, and secrets come from env vars. Published
ports bind to all interfaces in `docker-compose.yml` and to `127.0.0.1`
in `docker-compose.mac.yml`. The `ghola/ghola:dev` runtime image is
`alpine:3.20` and runs as root.

---

## Known Limitations

- **Dev stack runs without auth**: compose sets `AUTH_DISABLED=true`.
- **No `/v1` request body limit or rate limit**: the API trusts its
  caller (the local ghola daemon).
- **Semantic tier has no per-user ACL**: any authenticated caller can
  query or consolidate any workspace ID it knows.
- **No rate limit on failed key lookups**: keys are 256-bit random, so
  guessing isn't practical, but failures aren't throttled.

---

## Reporting Security Issues

See the top-level [`SECURITY.md`](../SECURITY.md).
