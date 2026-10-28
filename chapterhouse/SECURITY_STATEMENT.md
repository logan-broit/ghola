# Chapterhouse Security Statement

Security posture of the chapterhouse API (`chapterhouse/cmd/ch-api`)
and admin console (`cmd/ch-web`). Chapterhouse stores agents'
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

### Passwords

- bcrypt, Go default cost (10)
- Hashes load only through `GetUserByUsernameForAuth()`, not the general
  user queries
- **Source**: `chapterhouse/internal/handler/admin.go`

### Sessions (ch-web)

- 32 bytes from `crypto/rand`, stored as a SHA-256 hash
- 8-hour lifetime
- Cookies: `HttpOnly`, `SameSite=Lax`, `Secure` unless `ENVIRONMENT` is
  `local` or `development` (unknown values count as production)
- **Source**: `chapterhouse/internal/auth/session_provider.go`,
  `chapterhouse/cmd/ch-api/main.go`

### JWT (OpenID Connect)

- RS256/RS384/RS512 against a JWKS endpoint, keys cached
  (`JWKS_CACHE_TTL`, default 15 minutes)
- Claims: `sub`, `preferred_username`, `email`, `realm_access.roles`
- **Source**: `chapterhouse/internal/auth/provider.go`

### Authentication Chain for `/v1`

A composite provider tries the **API key** first, then the provider
selected by `AUTH_PROVIDER`:

- `jwt`: a valid JWT is required.
- `default` (the default, and what the compose stack uses): every
  request without a valid API key is authenticated as
  `AUTH_DEFAULT_USER`. This is single-user mode. Run it only on
  loopback or a trusted network. ch-api logs a warning when
  `ENVIRONMENT=production` and `AUTH_PROVIDER=default`.

`/health` and `/ready` need no auth. `/api/v1/*` uses session cookies
only.

**Source**: `chapterhouse/internal/auth/composite_provider.go`,
`chapterhouse/internal/config/config.go`

---

## 2. Data Access Control

- **Episodic tier**: rows carry `user_id`. Query, ingest, share and
  session-workspace handlers require the request's `user_id` to match
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

Only admin login is rate-limited: 5 requests/minute per client IP
(token bucket). `/v1` has no limiter.

**Source**: `chapterhouse/internal/middleware/ratelimit.go`,
`chapterhouse/cmd/ch-api/main.go`

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

Stats, audit logs and user and key management need a session plus the
admin role (`RequireAdmin`). Other users get only self-service routes
for their own keys, password and audit history.

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

Admin actions go to the `audit_log` table: login and logout (IP, user
agent), user create/update/deactivate/reactivate, and API key
create/revoke. `/v1` memory operations aren't audited. Every HTTP request
is logged as structured JSON (request ID, method, path, status,
duration, response size, user ID, remote address).

**Source**: `chapterhouse/internal/handler/admin.go`,
`chapterhouse/internal/middleware/middleware.go`,
`chapterhouse/db/migrations/001_initial_schema.sql`

---

## 6. Deployment

The repo ships a local docker compose stack (`deploy/docker-compose/`).
It isn't production-shaped: no TLS, default `POSTGRES_PASSWORD=dev`,
`AUTH_PROVIDER=default`, and secrets come from env vars. Published
ports bind to all interfaces in `docker-compose.yml` and to `127.0.0.1`
in `docker-compose.mac.yml`. The `ghola/ghola:dev` runtime image is
`alpine:3.20` and runs as root.

---

## 7. Admin Console (ch-web)

A Go binary that serves embedded static files and proxies `/api` to
ch-api. No npm dependencies or build toolchain.

### Security Headers

- `Content-Security-Policy: default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'`
- `X-Frame-Options: DENY`
- `X-Content-Type-Options: nosniff`
- `Referrer-Policy: strict-origin-when-cross-origin`

### Proxy Hardening

- Proxied URLs are parsed and checked to stay on the API host
- Forwards only the `Content-Type`, `Accept`, `Cookie` and
  `Authorization` headers
- Shared HTTP client with a 30-second timeout

**Source**: `cmd/ch-web/main.go`

---

## Known Limitations

- **Single-user default**: `AUTH_PROVIDER=default` turns off
  authentication for requests without a key.
- **No `/v1` request body limit or rate limit**: the API trusts its
  caller (the local ghola daemon).
- **Semantic tier has no per-user ACL**: any authenticated caller can
  query or consolidate any workspace ID it knows.
- **In-memory login limiter**: per instance only.
- **No account lockout**: failed logins are IP-rate-limited only.

---

## Reporting Security Issues

See the top-level [`SECURITY.md`](../SECURITY.md).
