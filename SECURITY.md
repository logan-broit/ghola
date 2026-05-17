# Security policy

## Reporting a vulnerability

Email `logan.broit@gmail.com` or open a
[private security advisory](https://github.com/logan-broit/ghola/security/advisories/new).
**Do not file a public issue** for security reports.

You will get an acknowledgement within a week. Coordinated disclosure
is welcome — let me know if you have a timeline.

## In scope

- The ghola daemon (`cmd/ghola`, `cmd/ghola-mcp`, `internal/`)
- The chapterhouse server (`_chapterhouse/ch-server/`) and admin
  console (`_chapterhouse/ch-web/`)
- The Python sidecars (`mentat/`, `truthsayer/`)
- The published Docker images and Helm charts in the repo

## Out of scope

- Upstream dependencies (pgvector, vLLM, FastAPI, etc.) — report those
  to their respective projects.
- The dormant Rust extension (`extension/`) — kept as algorithm
  reference; not built or shipped.

## Surface notes

- ghola defaults to **loopback-only** (`GHOLA_LOOPBACK_ONLY=true`).
  Disabling it without an auth layer in front exposes the recall
  pipeline to the local network.
- chapterhouse requires `Authorization: Bearer <api-key>` on every
  request; the local dev stack ships a default `POSTGRES_PASSWORD=dev`
  which is documented as dev-only.
- Bearer tokens flow through `internal/chapterhouse/client.go` and are
  never logged (verified during the consistency-sweep audit).
- The Python sidecars (mentat, truthsayer) have no auth layer; they
  assume trusted intra-cluster network. Don't expose them publicly.
