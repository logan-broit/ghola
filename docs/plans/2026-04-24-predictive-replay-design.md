# Predictive Replay — Design

**Date:** 2026-04-24
**Status:** Approved (brainstorming complete; implementation plan to follow)
**Supersedes:** the LLM-distillation replay pipeline currently in
`_chapterhouse/ch-server/internal/replay/`

## Background

Ghola's v0.2 replay pipeline distills episodic events into semantic mnemes
by clustering on entity-pair co-occurrence and asking a chat LLM ("Mentat")
to summarize the cluster as a `{concept, content}` text mneme. The resulting
mneme is embedded with the same model used for events, deduped via HNSW, and
written to `semantic.mnemes`.

Two problems with this design surfaced during dogfooding:

1. **Generative summarization is the wrong frame for the task.** Per Yann
   LeCun's position paper (*A Path Towards Autonomous Machine Intelligence*,
   2022) and the JEPA family of follow-ups, predictive world models that
   eliminate irrelevant detail in *representation space* outperform
   generative ones that try to reconstruct surface form. We were asking
   Mentat for "a summary of episodic embeddings" when the right question is
   "given past embeddings, what future embeddings are predictable?"

2. **The tier hierarchy was a naming convention, not earned by
   architecture.** Working / episodic / semantic were three SQL tables with
   different retention policies, but the same flat-cosine retrieval
   model. JEPA-family approaches (I-JEPA, V-JEPA 2, DINOv2, LeJEPA)
   suggest tiers correspond to learned encoders operating at different
   time scales / levels of abstraction.

This design replaces the LLM-distillation pipeline with a JEPA-style
predictive architecture, scoped to v1a as proof-of-viability.

## Reference Papers

| arXiv | Paper | Relevance |
|---|---|---|
| 2304.07193 | DINOv2 | "Frozen foundation + trainable downstream head" pattern |
| 2301.08243 | I-JEPA | Direct architectural template (mask + predict in repr space) |
| 2506.09985 | V-JEPA 2 | Two-stage SSL pretrain → downstream task pattern |
| 2511.08544 | LeJEPA | Heuristics-free training via SIGReg; primary loss path |
| 2105.04906 | VICReg | Three-term non-contrastive loss; fallback if LeJEPA underperforms |
| 2103.03230 | Barlow Twins | Alternative non-contrastive regularizer |
| 2002.05709 | SimCLR | Contrastive baseline (the method we explicitly avoid) |
| 2104.14294 | DINO | Teacher-student baseline; LeJEPA's claimed improvement target |
| 2602.02603 | EchoJEPA | Domain-specific JEPA proof point |

## Settled Decisions

| Axis | Choice | Rationale |
|---|---|---|
| Scope of pivot | Full replacement, v1a-blocking | Mentat-LLM path was never run in production; "preserving" it is fiction |
| L0 encoder | Qwen3-Embedding-0.6B (frozen) | Foundation-model-scale work already done; we add temporal/hierarchical structure on top |
| Embedding dim | 1024 (unchanged) | Standard in SSL research range (1024–2048); upgrade later if validation hits a representational ceiling |
| Levels for v1a | Two: L0 event, L1 session | Smallest coherent proof; matches LeCun H-JEPA Figure 15 minimum |
| Pooler architecture | Type-weighted mean (Stage 1) → single-query attention (Stage 2) | Stage 1: zero learnable params, deterministic, isolation of variables. Stage 2 only after Stage 1 baseline validated |
| Pretraining objective | Event-mask within-session (I-JEPA-style) | Solves data-scarcity by yielding many training pairs per session; standard JEPA pattern |
| Regularizer | LeJEPA / SIGReg primary, VICReg fallback | LeJEPA simpler (no EMA, no projector, ~50 LOC) but newer; VICReg battle-tested |
| Validation | Automated metrics + behavioral A/B + dogfooding (Q6c) | Automated gates merges; A/B shapes iteration; dogfooding is v1b steering signal |
| Service architecture | New `mentat` Python+PyTorch service in compose | Mirrors melange pattern; specialized service over HTTP |
| Bootstrap data | Multi-source: claude-code + openclaw + pi + augment + codex-cli + hermes + cline + opencode | ~225K events from this machine, plus what user pulls from other machines; ~1,290 sessions |
| Mneme schema shape | `semantic.mnemes` with `level` column, unified 1024-dim across levels, `member_ids` polymorphic | Sets up cleanly for L2/L3 expansion without schema churn |
| Tier semantics | Hierarchy in storage + abstraction; continuous in embedding space; tiers shade in recall | Storage tree, geometric continuity, retrieval merges by score |

## Architecture Overview

```
                   ┌─────────────────────────┐
                   │   ghola daemon (Go)     │
                   │   API unchanged         │
                   └────────────┬────────────┘
                                │ HTTP
                   ┌────────────▼────────────┐
                   │  chapterhouse (Go)      │
                   │  internal/semantic/     │ ── replaces internal/replay/
                   │  internal/mentat/       │ ── new mentat HTTP client
                   └──────┬──────────┬──────┘
                          │          │
                     Postgres      HTTP
                          │          │
                   ┌──────▼───┐      │
                   │ postgres │      │
                   │ episodic │      │
                   │  .events │      │
                   │  .sessions      │
                   │  (+l1_emb)│     │
                   │ semantic │      │
                   │  .mnemes │      │
                   │  (new    │      │
                   │   shape) │      │
                   └──────────┘      │
                              ┌──────▼─────────────┐
                              │  mentat (Python)   │
                              │  FastAPI+PyTorch   │
                              │  /v1/pool          │
                              │  /v1/predict       │
                              │  /v1/train         │
                              │  /v1/cluster       │
                              │  /v1/health        │
                              │  weights on        │
                              │  mentat-weights    │
                              │  named volume      │
                              └────────┬───────────┘
                                       │ HTTP at training time
                              ┌────────▼─────────┐
                              │ melange (vLLM,   │
                              │ Qwen3-Embedding) │
                              │ unchanged        │
                              └──────────────────┘
```

**What's unchanged:** ghola daemon HTTP API, ghola-mcp tools, encoding worker
basic mechanics, melange, all `/v1/episodic/*` endpoint shapes, Phase 11 e2e
gates 1/2/4/6.

**What's new:** `mentat` compose service, `semantic.mnemes` schema (text
columns dropped, level column added), `episodic.sessions.l1_embedding`
column, multi-source import tool.

**What's deleted:** `_chapterhouse/ch-server/internal/replay/` (17 unit tests
plus the entire pipeline).

## Data Model

### `semantic.mnemes` (pg_ghola v0.3)

```sql
CREATE TABLE semantic.mnemes (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id          uuid NOT NULL,
    level                 integer NOT NULL DEFAULT 1,
    embedding             vector(${EMBEDDING_DIM}) NOT NULL,
    confidence            double precision NOT NULL DEFAULT 0.5,
    access_count          integer NOT NULL DEFAULT 0,
    member_ids            uuid[] NOT NULL DEFAULT '{}',
    contributor_user_ids  uuid[] NOT NULL DEFAULT '{}',
    created_at            timestamptz NOT NULL DEFAULT now(),
    last_reinforced_at    timestamptz NOT NULL DEFAULT now(),
    last_access           timestamptz NOT NULL DEFAULT now(),
    state                 text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active','archived'))
);

CREATE INDEX mnemes_by_level        ON semantic.mnemes (workspace_id, level);
CREATE INDEX mnemes_embedding_hnsw  ON semantic.mnemes USING hnsw (embedding vector_cosine_ops);
CREATE INDEX mnemes_member_ids_gin  ON semantic.mnemes USING gin (member_ids);
CREATE INDEX mnemes_last_reinforced ON semantic.mnemes (last_reinforced_at DESC) WHERE state = 'active';
```

Levels: `level=1` → session-cluster; future `level=2+` → cluster-of-clusters.
Same table, same index, same query patterns.

### `episodic.sessions` (additive change)

```sql
ALTER TABLE episodic.sessions
    ADD COLUMN l1_embedding vector(${EMBEDDING_DIM});

CREATE INDEX episodic_sessions_l1_hnsw
    ON episodic.sessions USING hnsw (l1_embedding vector_cosine_ops)
    WHERE l1_embedding IS NOT NULL;
```

### `mentat-weights` volume layout

```
/weights/
  current -> v42                        # symlink to active version
  v40/ { pooler.pt, event_predictor.pt, session_predictor.pt, metadata.json }
  v41/ ...
  v42/ ...
```

Atomic symlink flip on successful training; rollback = manual flip.

## The Mentat Service

### HTTP API

```
POST /v1/pool      events + types → L1 embedding
POST /v1/predict   past L1 sequence → predicted next L1
POST /v1/train     kick off training run (async); body: {stages: [...]}
POST /v1/cluster   recluster a workspace's L1 embeddings → upsert mnemes
GET  /v1/training/{job_id}
GET  /v1/health
```

### Internal modules (PyTorch)

- **TypeWeightedMeanPool** (Stage 1, 0 params): weights `{user: 1.0,
  assistant: 0.5, tool_result: 0.1, system: 0.0}`.
- **AttentionPool** (Stage 2, 1024 params): single learned query vector,
  softmax attention over event embeddings.
- **EventMaskPredictor** (~1.5M params): MLP `1024 → 512 → 1024` with
  residual; predicts masked-event embedding from pooled visible context.
- **SessionPredictor** (~1.5M params): same shape as EventMaskPredictor;
  predicts L1_{t+1} from L1_t (or short L1 history).
- **SIGReg / VICReg**: regularizer applied to per-batch pooler output to
  enforce isotropic-Gaussian / decorrelated structure. LeJEPA primary;
  VICReg fallback.

### Training stages

**Stage A — event-mask pretraining.** Sample minibatches of sessions from
episodic. Mask 40% of events per session (mix of contiguous and scattered).
Forward: `Pred(pool(visible_events))` against actual masked-event
embeddings. Loss = cosine-invariance + λ·SIGReg(pooler_batch). Trains until
held-out mask cosine plateaus.

**Stage B — session-level predictor.** Pooler frozen from Stage A. Form
`(L1_t, L1_{t+1})` pairs from consecutive sessions. Same loss shape on L1
outputs. Tighter data than Stage A (thousands of pairs, not tens of
thousands of masks).

**Stage C — clustering.** HDBSCAN on the workspace's
`episodic.sessions.l1_embedding`. For each cluster with `member_count ≥ 3`,
upsert a `level=1` mneme. Existing mnemes whose membership shifts get
`last_reinforced_at = now()` and updated centroid + confidence.

### Data augmentation

Cheap-and-principled, partially compensates for raw-data scarcity:
- Random mask seeds per epoch (multiplies effective training pairs).
- Mixed contiguous + scattered masking strategies.
- Type-weighted mask bias (preferentially mask high-value events).
- Session-level dropout (10% events dropped from visible context).

### Error handling

| Failure | Behavior |
|---|---|
| Mentat unreachable / 5xx | Semantic tier returns 0 hits; working + episodic still answer. Agent sees fewer hits but no error. |
| Training run fails | Old weights stay active; failure logged; next cron retries. Inference uninterrupted. |
| Never-trained (cold start) | Pooler in Stage-1 mode (deterministic type-weighted mean); predictor returns identity (history[-1]). System works immediately, gets smarter after first training run. |

## Runtime Data Flows

### Write path

`agent → ghola-mcp → ghola → /v1/record` triggers sietch append + melange
embed (unchanged). On session close (session_end fired or idle past
`TerminalBranchIdleThreshold`), encoding worker POSTs the session's events
to `mentat:/v1/pool`, writes returned vector to
`episodic.sessions.l1_embedding`. **One HTTP hop at session close, not per
event.**

### Read path (recall)

`agent → ghola-mcp → ghola → /v1/recall` fans out across:
- **sietch / working tier**: per-event vector + FTS lookup (unchanged)
- **chapterhouse episodic tier**: per-event Qwen-space cosine (unchanged)
- **chapterhouse semantic tier**: pools query+recent context via
  `mentat:/v1/pool` to query_L1, runs cosine against `semantic.mnemes`,
  returns top-K by score.

Mentat down → semantic tier returns empty; other tiers still answer. No
recall-path failure mode that breaks all tiers.

### Training path

Scheduler in chapterhouse (02:00 cron, same slot the v0.2 stub used) POSTs
`mentat:/v1/train` with all three stages. Mentat reads training data
directly from postgres (read-only DSN), writes weights to volume, on
success atomically flips `current` symlink + reloads in-memory models;
on failure logs and retains old weights. Stage C clustering is a final
phase of the same job and writes `semantic.mnemes` rows directly.

## Multi-Source Bootstrap

### Tool

```
cmd/import-logs/main.go \
  --source=jsonl-family --path=...   \
  --source=augment       --path=...  \
  --source=codex-cli     --path=...  \
  --source=hermes        --path=...  \
  --source=cline         --path=...  \
  --source=opencode      --path=...  \
  --workspace=<uuid> --user=<uuid>   \
  --dry-run --resume --batch-size=32
```

### Adapter contract (Go)

```go
type Adapter interface {
    Name() string
    Walk(root string) iter.Seq[SessionFile]
    Parse(sf SessionFile) (*NormalizedSession, error)
}

type NormalizedSession struct {
    SourceTool, SourceMachine string
    SessionID                 uuid.UUID  // content-hash-derived, stable
    UserID                    uuid.UUID
    StartedAt                 time.Time
    EndedAt                   *time.Time
    Cwd, GitBranch            *string
    AgentKind                 string
    Events                    []NormalizedEvent
}
```

### Per-source adapters

| Source | Format | Effort |
|---|---|---|
| jsonl-family (claude-code + openclaw + pi + downloaded claude-code) | JSONL with `type: session` header + event stream; pi-mono shared format | 1–2 days unified |
| augment | `sessions/*.json` with `chatHistory[].exchange.{request,response}` (skip checkpoint-documents/) | 1 day |
| codex-cli | JSONL with `session_meta` + event stream | 0.5 day |
| hermes | Single JSON per session, messages array | 0.5 day |
| cline | JSON array, Anthropic-messages-API shape | 0.5 day |
| opencode | SQLite (`session ─ message ─ part` join) via modernc.org/sqlite | 0.5 day |

### Idempotency

Session ID = `uuid.NewSHA1(NameSpaceOID, sourceTool || sha256(rawBytes))`.
With `--resume`, importer checks `episodic.sessions.id = sessionID` before
re-ingesting. Re-runs are cheap and safe.

### Privacy posture

**No `--redact` flag in v1a.** Per the threat-model analysis (see
`docs/security.md`):

- The leak path that matters (model-provider exposure of secrets in
  tool_results) is **upstream** of ghola — the secret flowed to the
  inference API before ghola saw it. Ghola redaction does not address
  this.
- Secrets in ghola's storage are co-located on disk with already-plaintext
  Claude Code logs, shell history, etc. Adding a redaction at ghola's
  ingress is marginal.
- The right primary defense is **tool-use patterns that keep secrets in
  FDs** rather than tool_results. Documented in
  `~/ai/identity/rules/secret-handling.md` and loaded into every Claude
  Code session.
- Redaction at *export boundaries* (multi-machine sync, backups) is a
  meaningful future feature, deferred until those features exist.

## Validation

### Automated metrics (gates merges)

| Metric | What it measures | Threshold |
|---|---|---|
| 1: event-mask cosine accuracy | Reconstruction cosine vs random-event baseline on held-out 20% | reconstruction > random by ≥ 2σ on 100+ sessions |
| 2: next-session cosine accuracy | L1_{t+1} prediction cosine vs random-session baseline | predicted > random by ≥ 2σ |
| 3: cluster coherence | Per-cluster median intra-cosine vs nearest-other-cluster median inter-cosine | intra > inter for ≥ 3 largest clusters |

### Behavioral A/B (shapes iteration)

`--compare-pipelines` flag in recall path: ch-server runs both old
episodic-cosine path and new L1-predictive path, returns merged response
tagged `(old)` and `(new)`. Logs to local jsonl for weekly review. Not
automated; does not gate merges.

### Dogfooding (continuous, v1b steering signal)

Tagged events during real use: `recall_surprise_positive`,
`recall_miss_obvious`, `recall_useful_unprompted`, `recall_noisy`. After 2–4
weeks, review the tags. Used to steer v1b, not gate v1a ship.

### v1a ship criteria

1. Metrics 1 + 2 both ≥ 2σ over baseline on held-out data.
2. Metric 3 holds for ≥ 3 largest clusters.
3. Phase 11 e2e gates 1, 2, 4, 6 still pass.
4. Full bootstrap of v1a corpus completes without drops/errors.

### Non-goals

- Beating prior art on any benchmark.
- Production-scale quality.
- Dogfooding-based merge gating.

## Rollout — PR Sequence

| PR | Scope | Effort |
|---|---|---|
| 1 | Vertical slice: pg_ghola v0.3, mentat scaffold (stubbed endpoints), ch-server `internal/semantic/` + `internal/mentat/`, encoding-worker write hook, semantic recall read path, replay/ deleted | 2–3 days |
| 2 | Import tool + jsonl-family adapter | 1–2 days |
| 3 | Remaining adapters (augment, codex-cli, hermes, cline, opencode) | 2–3 days (split possible) |
| 4 | Real clustering (Stage C in mentat); first mnemes produced | 1 day |
| 5 | Stage A training (event-mask + LeJEPA SIGReg); Metric 1 in tests | 3–5 days |
| 6 | Stage B training (session predictor); Metric 2 in tests | 2–3 days |
| 7 | Validation harness + v1a ship gate; docs | 1–2 days |
| 8 (post-v1a) | Attention-pool upgrade, ship as v1a.1 if metrics improve | 2 days |

**Realistic v1a timeline: 3–4 weeks wall-clock.** Add 2 days for VICReg
fallback if LeJEPA underperforms.

### Rollback story

Each PR preserves a working system. PR-5 weights regression → `current`
symlink stays on pre-training state, system uses identity predictor +
type-weighted mean pool. PR-4 clustering issue → semantic table
truncate-and-recluster, episodic untouched. Deletion of `internal/replay/`
in PR 1 is irreversible-via-git but the pipeline never ran in production
so there's nothing to lose.

## Open Questions / Deferred

- **Multi-machine ghola.** Logs sync from other machines via rsync as
  a one-shot for bootstrap; ongoing multi-machine recording is
  out-of-scope for v1a. Future design topic: per-device session
  namespacing, conflict resolution, Tailscale-mediated ingress.
- **L2/L3 expansion.** Schema supports it (level column), training does
  not. Trigger: when L1 cluster coherence is consistently strong AND
  L1-only recall is missing patterns that span sessions in different
  ways. v1b at earliest.
- **Learned higher-level encoders (option (c) from Q2).** Frozen Qwen +
  small adapters at L1/L2/L3 instead of pure pooling. v1b/v1c when data
  scale + signal warrants.
- **Pre-training on public corpora.** ShareGPT etc. Domain-shift risk
  high; deferred until live dogfooding shows pretrained-on-yours alone
  underperforms.

## References

- LeCun, *A Path Towards Autonomous Machine Intelligence* (2022) — the
  position paper that motivated this redesign.
- See **Reference Papers** table above for arXiv IDs of all SSL / JEPA
  works cited.
- `~/ai/identity/rules/secret-handling.md` — secret-flow patterns the
  agents are expected to follow upstream of ghola.
- `docs/2026-04-19-greenfield-tiered-memory-design.md` — original v0.2
  tiered-memory design, partially superseded.
- `docs/2026-04-20-jsonl-native-event-shape.md` — event shape contract
  (unchanged by this design).
