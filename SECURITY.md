# Security policy

## Reporting a vulnerability

Email `logan.broit@gmail.com` or open a
[private security advisory](https://github.com/logan-broit/ghola/security/advisories/new).
**Do not file a public issue** for security reports.

You will get an acknowledgement within a week. Coordinated disclosure
is welcome — let me know if you have a timeline.

## In scope

- The ghola daemon (`cmd/ghola`, `cmd/ghola-mcp`, `internal/`)
- The chapterhouse server (`chapterhouse/`) and admin
  console (`cmd/ch-web/`)
- The Python sidecars (`mentat/`, `truthsayer/`)
- The Dockerfiles under `deploy/`

## Out of scope

- Upstream dependencies (pgvector, vLLM, FastAPI, etc.) — report those
  to their respective projects.
- The retired Rust extension (`attic/extension/`) — kept as algorithm
  reference; not built or shipped.

## Surface notes

- ghola defaults to **loopback-only** (`GHOLA_LOOPBACK_ONLY=true`).
  Disabling it without an auth layer in front exposes the recall
  pipeline to the local network.
- chapterhouse's `/v1` routes accept `Authorization: Bearer ch_k1_...`
  API keys, but under `AUTH_PROVIDER=default` (the default, and what
  the compose stack uses) any request without a valid key is served as
  `AUTH_DEFAULT_USER`. Keep it on loopback or a trusted network, or run
  `AUTH_PROVIDER=jwt`. The dev stack's `POSTGRES_PASSWORD=dev` is
  dev-only.
- Bearer tokens flow through `internal/chapterhouse/client.go` and are
  never logged (verified during the consistency-sweep audit).
- The Python sidecars (mentat, truthsayer) have no auth layer; they
  assume trusted intra-cluster network. Don't expose them publicly.
