# Chapterhouse Security Statement

This document describes the security posture, design decisions, and implementation
details of Chapterhouse. Chapterhouse stores persistent memories for AI assistants
-- personal facts, organizational knowledge, and session context -- so protecting
the confidentiality and integrity of that data is central to its design.

---

## Threat Model

Chapterhouse assumes that API clients are authenticated but potentially
compromised. The primary threats are:

1. **Unauthorized memory access** -- a user reads or modifies another user's
   personal memories
2. **API key compromise** -- a stolen key grants access to a user's memories
3. **Credential theft** -- database or embedding service credentials are exposed
4. **Data exfiltration** -- bulk export of memories via API abuse
5. **Injection attacks** -- SQL injection or malformed input corrupts data or
   leaks internal details

Each layer below addresses one or more of these threats.

---

## 1. Authentication

### API Keys

- **Format**: Prefixed (`ch_k1_`) followed by 32 bytes of cryptographic randomness
  encoded as hex (64 characters)
- **Storage**: Keys are hashed with **SHA-256** before storage -- plaintext keys
  are never persisted
- **Validation**: Format is checked (prefix, length, hex encoding) before any
  database lookup
- **Last-used tracking**: Updated asynchronously to avoid adding latency to
  authenticated requests
- **Source**: `ch-server/internal/auth/apikey_provider.go`

SHA-256 is appropriate for high-entropy random keys (256 bits) that are not
vulnerable to dictionary attacks.

### Passwords

- **Algorithm**: bcrypt with Go's default cost (10)
- **Access**: Password hashes are only loaded via a dedicated
  `GetUserByUsernameForAuth()` query -- they are excluded from general user
  listing queries
- **Source**: `ch-server/internal/handler/admin.go`

### Sessions

- **Token generation**: 32 bytes from `crypto/rand` (256 bits of entropy),
  stored as SHA-256 hash
- **Duration**: 8 hours (configurable)
- **Cookie attributes**:
  - `HttpOnly: true` -- prevents JavaScript access (XSS mitigation)
  - `Secure: true` in production (HTTPS-only)
  - `SameSite: Lax` -- mitigates CSRF for cross-origin POST requests
- **Source**: `ch-server/internal/auth/session_provider.go`

### JWT (OpenID Connect)

- **Validation**: RS256/RS384/RS512 with JWKS endpoint key rotation
- **Claims**: `sub` (user ID), `preferred_username`, `email`, `realm_access.roles`
- **Key caching**: JWKS keys cached with configurable TTL (default 15 minutes),
  double-checked locking for thread safety
- **Source**: `ch-server/internal/auth/provider.go`

### Authentication Chain

The composite provider tries providers in order: **API Key** (Authorization
header) -> **JWT/Default** (fallback). MCP endpoints only accept API key
authentication with no fallback, ensuring programmatic access always requires
explicit credentials.

A startup warning is emitted if the default (no-auth) provider is active in
a production environment.

---

## 2. Memory Access Control

Chapterhouse enforces two-level memory isolation: **personal** (visible only to
the creator) and **org** (visible to all users in the same organization).

### Database-Level Enforcement

All memory queries filter by ownership using pg_recall's `workspace_id`:

- Personal memories use `user_id` as `workspace_id`
- Org-scoped memories use `org_id` as `workspace_id`
- Recall queries search both workspaces and merge results

The `mneme.Store` enforces this pattern consistently across all operations:
`Remember`, `Recall`, `Forget`, `ChangeScope`, `List`, `Export`.

### Session-Scoped Queries

Session lifecycle tools (`list_sessions`, `session_summary`, `session_context`)
filter by user ownership. There is no org-scope access for session data -- a
user can only see sessions where they created memories.

### Scope Enforcement

- `scope` values are constrained to `personal` or `org`
- The `share_memory` MCP tool allows users to change scope, but only the
  memory creator can do so (ownership verified before workspace migration)

**Source**: `ch-server/internal/mneme/store.go`

---

## 3. API Security

### Rate Limiting

Two rate limiters protect against abuse:

| Limiter | Rate | Scope | Purpose |
|---------|------|-------|---------|
| Login | 5 req/min | Per IP | Brute-force password protection |
| MCP | 10 req/sec, burst 50 | Per user | API abuse prevention |

Rate limiters use a token bucket algorithm. The login limiter keys on client
IP; the MCP limiter keys on authenticated user ID.

**Source**: `ch-server/internal/middleware/ratelimit.go`

### Input Validation

- All JSON requests are parsed with `DisallowUnknownFields()` to reject
  unexpected fields (mass assignment prevention)
- Pagination is bounded: 1--100 for user/MCP routes, up to 1000 for admin routes
- Session tool `limit` parameter is bounded to 1--100
- `session_id` arguments are validated as UUID format before any database query
- Memory type and scope values are validated at the handler level
- Request timeout: 30 seconds (configurable)

### CORS

CORS is handled by a centralized router-level middleware using configured
origin allowlists. The default production configuration allows no cross-origin
requests. MCP handlers do not set their own CORS headers.

**Source**: `ch-server/internal/middleware/middleware.go`, `cmd/api/main.go`

### Admin Authorization

System statistics, audit logs, and user management endpoints require both
session authentication and the admin role (`RequireAdmin` middleware). Regular
authenticated users can only access their own memories, API keys, and audit
history via dedicated self-service routes.

---

## 4. Database Security

### Connection

- **Driver**: pgx (native Go PostgreSQL driver)
- **SSL mode**: `prefer` by default -- upgrades to TLS when the server supports
  it. Operators should set `DATABASE_SSL_MODE=require` or `verify-full` in
  production
- **Password handling**: The `DatabaseConfig.Password` field is not serialized
  into logs or config dumps
- **Connection pooling**: Min 5, max 25 connections with 1-hour lifetime and
  30-minute idle timeout

### Query Safety

All database queries use **parameterized statements** with positional
placeholders (`$1`, `$2`, ...). No string concatenation or interpolation is
used in query construction. The `mneme` package uses raw SQL with parameterized
queries; the `repository` package uses sqlc-generated code.

### Data Lifecycle

- Working memories (7-day TTL) expire via pg_recall's temporal decay
- Superseded mnemes are archived, preserving lineage while keeping active
  results current

**Source**: `ch-server/internal/config/config.go`, `ch-server/internal/mneme/`

---

## 5. Audit Logging

### What Is Captured

Admin and MCP operations are logged to the `audit_logs` table:

- **Authentication events**: login, logout (with IP address, user agent)
- **User management**: create, update, deactivate, reactivate
- **API key lifecycle**: create, revoke
- **MCP tool calls**: remember, recall, forget, list_memories, share_memory,
  export_memories, list_sessions, session_summary, session_context (with
  tool-specific details such as query text, memory type, and bounded session
  IDs)
- **Resource details**: JSONB `details` column for additional context

### Request Logging

All HTTP requests are logged via structured JSON middleware:

- Request ID, method, path, status code, duration
- Authenticated user ID
- Client IP address

### Session ID Protection

Chapterhouse distinguishes two types of session ID:

- **Transport session IDs** (MCP `Mcp-Session-Id` header) are bearer tokens
  created during the `initialize` handshake. These are always logged in
  truncated form (first 8 characters) to prevent session hijacking via log
  access.
- **Memory session IDs** (client-provided `session_id` on `remember`, or
  filter arguments on `recall`/`list_memories`/`export_memories`) are grouping
  tags within a user's own namespace. They are not authentication tokens and
  carry no authorization -- all access control is enforced by `user_id`
  filtering at the query level. These are also truncated to 8 characters in
  audit log `details` entries for consistency.

The `list_sessions` tool returns full memory session IDs to the calling user,
but these are the user's own session IDs filtered by ownership -- not a log
exposure risk.

**Source**: `ch-server/db/migrations/001_initial_schema.sql`,
`ch-server/internal/middleware/middleware.go`

---

## 6. MCP Session Security

### Session Lifecycle

MCP Streamable HTTP sessions are created during the `initialize` handshake,
which requires API key authentication. The session ID (UUID v4, 128 bits of
randomness) is returned in the `Mcp-Session-Id` response header.

### Session Binding

Sessions are bound to the authenticated user identity. Subsequent operations
(POST, GET, DELETE) on a session validate that the caller is the same user who
created the session. Unauthenticated session deletion is not permitted.

### Cleanup

Sessions expire after 30 minutes and are cleaned up every 5 minutes by a
background goroutine. The MCP request body is limited to 1 MiB via
`io.LimitReader`.

**Source**: `ch-server/internal/mcp/transport.go`

---

## 7. Deployment Security

### Container Security (ch-server)

| Control | Value |
|---------|-------|
| `runAsNonRoot` | `true` |
| `runAsUser` | `1000` |
| `readOnlyRootFilesystem` | `true` |
| `allowPrivilegeEscalation` | `false` |
| Capabilities | Drop `ALL` |

### Container Security (ch-web)

| Control | Value |
|---------|-------|
| Base image | `distroless/static-debian12:nonroot-amd64` |
| `runAsNonRoot` | `true` |
| `runAsUser` | `65534` (nonroot) |
| `readOnlyRootFilesystem` | `true` |
| `allowPrivilegeEscalation` | `false` |
| Capabilities | Drop `ALL` |

### Secrets Management

All credentials are sourced from Kubernetes Secrets:

- Database credentials: `secretKeyRef` from CNPG-managed secret
- Embedding API key: `secretKeyRef` from dedicated secret
- Admin bootstrap: `secretKeyRef` (optional)

No secrets are stored in ConfigMaps. The Helm chart does not support inline
credential values for production -- `existingSecret` is the expected path.

### Service Exposure

Both services use `ClusterIP` (internal only). External access is via optional
Ingress, disabled by default.

**Source**: `ch-server/charts/ch-server/values.yaml`,
`ch-web/charts/ch-web/values.yaml`

---

## 8. Admin Console Security

### Architecture

ch-web is a lightweight Go binary that serves embedded static files (HTML, CSS,
JS) and proxies API requests to ch-server. It has zero npm dependencies --
no build toolchain, no dependency supply chain risk.

### Security Headers

All responses include:

- `Content-Security-Policy: default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'`
- `X-Frame-Options: DENY`
- `X-Content-Type-Options: nosniff`
- `Referrer-Policy: strict-origin-when-cross-origin`

### Proxy Hardening

- URL validation: constructed proxy URLs are parsed and verified to stay on the
  expected API host
- Header allowlist: only `Content-Type`, `Accept`, `Cookie`, and `Authorization`
  are forwarded
- Shared HTTP client with 30-second timeout (prevents per-request allocation and
  slowloris attacks)

**Source**: `ch-web/cmd/server/main.go`

---

## Configuration Reference

| Setting | Env Var / Helm Value | Default | Description |
|---------|---------------------|---------|-------------|
| Environment | `ENVIRONMENT` / `config.environment` | `local` | `local`, `development`, `production` |
| DB SSL mode | `DATABASE_SSL_MODE` / `database.sslMode` | `prefer` | PostgreSQL SSL mode |
| Auth provider | `AUTH_PROVIDER` / `auth.provider` | `default` | `default`, `jwt` |
| CORS origins | `CORS_ORIGINS` / `cors.origins` | (empty) | Comma-separated allowed origins |
| Session duration | `SESSION_DURATION` | `8h` | Admin session lifetime |
| Secure cookies | (auto) | `true` in production | HTTPS-only session cookies |
| Login rate limit | (code) | 5 req/min per IP | Brute-force protection |
| MCP rate limit | (code) | 10 req/sec, burst 50 | Per-user API abuse protection |
| MCP body limit | (code) | 1 MiB | Maximum MCP request body size |

---

## Known Limitations

- **In-memory rate limiting**: Rate limits are per-instance. In multi-replica
  deployments, use an upstream rate limiter (e.g., Nginx, HAProxy) for
  distributed enforcement.
- **Single organization default**: All users are assigned to a shared default
  organization. Multi-organization support requires manual database updates.
- **No account lockout**: Failed login attempts are rate-limited by IP but do
  not trigger per-account lockout.

---

## Reporting Security Issues

If you discover a security vulnerability in Chapterhouse, please report it
responsibly by emailing the maintainers directly rather than opening a public
issue.
