# Retry-safe host ingestion

The Pi capture adapter needs durable acknowledgements without generating another session or event after a lost HTTP response. These opt-in contracts use the existing endpoints and Sietch databases. Clients without the new fields retain their existing behavior.

These changes are implemented in source; rebuilding/deploying the daemon and activating the Pi extension are separate steps. This remains a loopback development service, not an authenticated multi-user boundary.

## Readiness

`GET /health` includes `capabilities.session_replay: 1` and `capabilities.record_replay: 1`. A capture client must verify both before relying on this protocol. Older servers can silently ignore unknown JSON fields; HTTP 200 alone does not establish replay safety. Discovery creates no sessions or probe memories.

## Session creation

`POST /v1/session_start` accepts optional `session_id`. Supply a stable UUID allocated once per episode and durably retained before sending. UUID spellings are normalized to the canonical UUID string before accessing the session file. Omit the field to retain fresh server-generated session IDs.

Creation and its receipt commit atomically. Equivalent creation input returns the session's current state, including its original start time and any subsequent closure. Reusing the ID with different user, workspace or creation metadata returns HTTP 409 (`CONFLICT`). An existing legacy session with no creation receipt cannot be adopted by supplying its ID. Server-assigned timestamps are not part of the creation fingerprint.

## Event recording

`POST /v1/record` accepts optional `source_key`. It must be nonblank UTF-8, at most 512 bytes, without NUL. Replay keys are scoped to one explicit, existing session owned by the supplied user. They are not global deduplication keys. The `request_id` field retains its existing meaning and is not a replay key.

With `source_key`, the server assigns the event ID and embedding; do not supply `event.id` or a nonempty `event.embedding`. Supply an allowed event type. The remaining event fields, including sanitized provenance in `raw_event`, are preserved. Freeze the original request for retry rather than rebuilding it from newer repository/session metadata or echoing the returned event.

The server fingerprints normalized request semantics before generating defaults or invoking embeddings. JSON object order and whitespace are normalized without rounding large integers. Array order, numeric spelling in raw JSON, and changes to authored fields remain significant. An equivalent retry returns the existing event's current representation; conflicting input returns 409. Invalid input returns 400; a missing replay session returns 404.

A new append commits the event, receipt, count and current pointer together. Replay does not increment counts, change the last-event timestamp, or rewind the current pointer. Source-key event IDs are generated inside the append transaction, after embedding, so a slow embedding request cannot insert behind a newer source-key event's watermark. Legacy recording retains its original ID behavior.

Receipt lookup precedes embedding. Forgotten events replay as a minimal suppressed representation, without re-embedding or echoing retained raw/tool payloads. This is not secure erasure: existing soft-forget storage semantics still apply.

## Recall provenance

`POST /v1/recall` now optionally returns `hits[].provenance` for recognized `pi-ghola/1` metadata on working events. Both FTS and vector search use the same projection. Existing event/session IDs and tier attribution are unchanged. This is additive and needs no schema migration; older clients can ignore it. The deployed daemon has not been updated.

The allowlist contains producer, actor, `unverified` status, capture time, annotation flag, Pi session/entry/parent-entry/previous-episode IDs, journal-persistence flag, source timestamp, stop reason, and checkout revision/dirty state. UUIDs and entry IDs are syntax-checked; actor/entry fields have 128-character ASCII bounds, times must parse as RFC 3339 and fit 64 bytes, and revisions must be 40 or 64 hex characters. Nullable booleans distinguish unknown from false. The full `raw_event`, journal paths, provider payloads, thinking and arbitrary tool fields are never projected.

Raw metadata exceeding 16 KiB is excluded at the SQLite read boundary. Malformed shapes or unknown producers omit provenance without breaking text recall. Invalid field values are omitted rather than repaired or promoted into verification. This does not truncate or rewrite the stored text or raw metadata. These are producer assertions, not authenticated actor identities or independently checked claims; missing fields remain unknown.

Fusion can retain a working source when another event-grain tier wins the exemplar, but only for the same event **and** session IDs. Semantic mnemes and session-vector summaries do not borrow event provenance. Chapterhouse-only hits still lack this projection; PostgreSQL-backed provenance remains unimplemented and unverified. No event-detail route or direct-database client fallback was added.

## Recovery and retention

Receipts live in an additive `ingest_receipts` table in each session database. Existing databases acquire the table on open without rewriting their events. No cached plaintext HTTP response or separate retrieval database is introduced.

Receipt-bearing files are protected from automatic GC, including empty sessions with creation receipts and sessions whose events were all forgotten. Legacy sessions without receipts retain their existing GC behavior. This deliberately retains the event cache until archive-backed receipt lookup and an explicit retention/purge policy exist. Removing files/volumes manually destroys the replay guarantee.

One daemon owns a Sietch root. Concurrent requests are supported; this does not introduce coordinated multi-daemon file garbage collection. Pause/private decisions and fork/episode identity still belong to the capture adapter; retry-safe ingestion alone does not implement them.

## Checks

Run from the repository root with cached dependencies:

```sh
GOPROXY=off GOTOOLCHAIN=local go test -race -count=1 ./...
GOPROXY=off GOTOOLCHAIN=local go vet ./...
```

`internal/core/pi_ingest_test.go` tests creation/record replay, conflicts, full text, concurrency, scope, suppression, GC and append ordering using real temporary SQLite storage. `internal/sietch/store_test.go` additionally verifies legacy-schema upgrade and rollback after a forced late cursor-write failure. HTTP tests cover capability discovery and wire status codes. `internal/sietch/provenance_test.go` uses real SQLite for both search paths, unknown/malformed/oversized metadata, allowlisting, forgotten records and unchanged stored payloads. `internal/core/recall_provenance_test.go` checks identity/grain-safe fusion and reranking with real SQLite and fixture external services. The companion Pi suite verifies the real HTTP/SDK output and bounded presentation.

Attribution: Pi.
