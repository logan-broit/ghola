# pg_ghola v0.3 — Contradiction Detection

## Context

The Bayesian confidence system in pg_ghola has the *mechanism* for
handling contradictions. When `bayesian_update(prior, 0.10)` is called, confidence
collapses rapidly — from 0.8 to 0.32 on the first contradiction, to 0.078 on the
second. The evidence constants are well-defined:

```
0.05  user rejected
0.10  contradiction detected
0.50  neutral (no information)
0.65  co-activation reinforcement
0.95  user confirmed
```

What was missing before v0.3 was the *detection*. Nothing in the system identified
when a new memory contradicted an existing one. The Bayesian machinery sat idle
until someone manually called `update_confidence(mneme_id, 0.10)`. v0.3 closes
that gap.

## The Problem

Contradiction detection in a SQL extension — without an LLM in the loop — faces
a fundamental limitation: **semantic similarity does not imply contradiction**.

Two memories about "Python version" with high cosine similarity could be:
- **Reinforcing:** "Python 3.12 is widely adopted" and "Python 3.12 has great performance"
- **Contradicting:** "Python 3.8 is the latest" and "Python 3.12 is the latest"

Vector distance alone cannot distinguish these cases. The cosine similarity
between both pairs would be high — they share the same topic and similar language.

## Design: Detection + Flagging

Given this limitation, pg_ghola takes a **detection + flagging** approach rather
than fully automatic resolution:

1. **Detect candidates** — find existing mnemes that are semantically close to a
   new mneme (high vector similarity, same workspace)
2. **Flag for review** — insert candidates into a `contradiction_candidates` table
   with similarity scores and metadata
3. **Let the caller decide** — the upstream system (e.g., Chapterhouse) reviews
   candidates, possibly using an LLM or user input to confirm actual contradictions
4. **Apply the mechanism** — confirmed contradictions trigger
   `bayesian_update(confidence, 0.10)` on the newer memory, using the existing
   Bayesian machinery

pg_ghola surfaces *what might contradict*, not *what does contradict*. The
judgment call stays with the caller.

## Detection Strategy

### Candidate Criteria

A contradiction candidate is a pair of mnemes in the same workspace where:

1. **High semantic similarity** — cosine similarity >= configurable threshold
   (default 0.85). This catches memories about the same topic.
2. **Different content** — the `content` fields are not identical (trivially,
   exact duplicates are not contradictions).
3. **Both active** — only `state = 'active'` mnemes are candidates. Dormant and
   archived memories are excluded.
4. **Concept overlap** (optional signal) — same `concept` field value
   strengthens the contradiction signal. Two memories labeled "python version"
   are more likely to contradict than two memories that happen to share embedding
   space.

### When Detection Runs

Detection runs at two points:

1. **On insert** — an `AFTER INSERT` trigger on `mnemes` calls
   `flag_contradictions()` automatically. This is the primary path.
2. **On demand** — `scan_workspace_contradictions()` checks all active mnemes in
   a workspace against each other. This is for bulk review or initial setup.

## Schema

### Table: `contradiction_candidates`

```sql
CREATE TABLE contradiction_candidates (
    id              bigserial PRIMARY KEY,
    workspace_id    uuid NOT NULL,
    mneme_a         uuid NOT NULL REFERENCES mnemes(id) ON DELETE CASCADE,
    mneme_b         uuid NOT NULL REFERENCES mnemes(id) ON DELETE CASCADE,
    similarity      double precision NOT NULL,
    concept_overlap boolean NOT NULL DEFAULT false,
    status          text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'confirmed', 'dismissed')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    resolved_at     timestamptz,
    UNIQUE (mneme_a, mneme_b)
);

CREATE INDEX contradiction_candidates_workspace_idx
    ON contradiction_candidates (workspace_id, status);
```

**Design choices:**

- `mneme_a` is always the newer mneme (the one that triggered detection).
  `mneme_b` is the existing mneme it potentially contradicts.
- `UNIQUE (mneme_a, mneme_b)` prevents duplicate flagging of the same pair.
- `ON DELETE CASCADE` cleans up candidates when either mneme is deleted.
- `status` tracks the lifecycle: `pending` → `confirmed` or `dismissed`.
- `concept_overlap` is true when both mnemes share the same `concept` value,
  providing an additional signal for the reviewer.

### Composite Types

```sql
CREATE TYPE contradiction_candidate_result AS (
    candidate_id    bigint,
    mneme_a         uuid,
    mneme_b         uuid,
    similarity      float8,
    concept_overlap boolean
);

CREATE TYPE contradiction_detail AS (
    candidate_id    bigint,
    similarity      float8,
    concept_overlap boolean,
    concept_a       text,
    content_a       text,
    confidence_a    float8,
    concept_b       text,
    content_b       text,
    confidence_b    float8,
    created_at      timestamptz
);
```

## Functions

### `check_contradictions(mneme_id uuid, similarity_threshold float8 DEFAULT 0.85)`

Returns `SETOF contradiction_candidate_result`.

Checks a specific mneme against all other active mnemes in the same workspace.
Returns candidate pairs above the similarity threshold. Read-only — does NOT
insert into the candidates table.

### `flag_contradictions(mneme_id uuid, similarity_threshold float8 DEFAULT 0.85)`

Returns `bigint` (number of candidates flagged).

Same detection logic as `check_contradictions`, but inserts results into
`contradiction_candidates`. Skips pairs already flagged (respects UNIQUE
constraint). This is what the insert trigger calls.

### `resolve_contradiction(candidate_id bigint, resolution text)`

Returns `text` (`'ok'`).

Resolves a contradiction candidate. `resolution` must be `'confirmed'` or
`'dismissed'`.

- **confirmed**: applies `bayesian_update(confidence, 0.10)` to the *newer*
  mneme (`mneme_a`), reducing its confidence. Established memories with
  accumulated evidence are not overturned by a single contradicting insertion —
  the newer memory bears the burden of proof. If it is genuinely correct, it
  will recover confidence through use and confirmation. Additionally, weakens
  any existing Hebbian association between the pair (weight *= 0.1) and creates
  a directed `contradicts` association from `mneme_a` to `mneme_b` (weight 1.0).
  This association feeds into recall scoring as a negative boost (-0.5x),
  actively demoting contradicted mnemes in results. Sets `status = 'confirmed'`
  and `resolved_at = now()`.
- **dismissed**: sets `status = 'dismissed'` and `resolved_at = now()`. No
  confidence change, no association modification.

### `get_pending_contradictions(workspace_id uuid, limit_n int DEFAULT 50)`

Returns `SETOF contradiction_detail`.

Returns pending contradiction candidates with full mneme details for review.
Ordered by similarity descending (highest similarity = most likely contradiction).

### `scan_workspace_contradictions(workspace_id uuid, similarity_threshold float8 DEFAULT 0.85)`

Returns `bigint` (number of new candidates flagged).

Bulk scan: checks all active mnemes in a workspace against each other. Useful
for initial setup or periodic review. This is an expensive operation (O(n²)
comparisons, though HNSW indexing amortizes the vector search).

## Trigger

```sql
CREATE OR REPLACE FUNCTION contradiction_check_trigger()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_ghola.flag_contradictions(NEW.id, 0.85);
    RETURN NEW;
END;
$$;

CREATE TRIGGER mneme_contradiction_check
    AFTER INSERT ON mnemes
    FOR EACH ROW
    EXECUTE FUNCTION contradiction_check_trigger();
```

The trigger fires after every insert. The similarity threshold is hardcoded at
0.85 in the trigger but can be overridden by calling `flag_contradictions`
directly.

**Performance consideration:** The trigger runs an HNSW nearest-neighbor search
for every insert. For typical workloads (tens of inserts per minute), this is
negligible. For bulk imports, disable the trigger temporarily:

```sql
ALTER TABLE pg_ghola.mnemes DISABLE TRIGGER mneme_contradiction_check;
-- ... bulk inserts ...
ALTER TABLE pg_ghola.mnemes ENABLE TRIGGER mneme_contradiction_check;
SELECT pg_ghola.scan_workspace_contradictions('workspace-id'::uuid, 0.85);
```

## System Integration

### Bayesian Confidence Flow

```
New mneme inserted
    │
    ├── trigger fires flag_contradictions()
    │       │
    │       ├── HNSW search for similar active mnemes
    │       │
    │       ├── candidates above threshold → INSERT into contradiction_candidates
    │       │
    │       └── (no automatic confidence change)
    │
    ▼
Caller reviews pending contradictions
    │
    ├── resolve_contradiction(id, 'confirmed')
    │       │
    │       ├── bayesian_update(mneme_a.confidence, 0.10)
    │       │   newer mneme's confidence drops (burden of proof)
    │       │
    │       ├── weaken hebbian association between pair (weight *= 0.1)
    │       │
    │       └── create 'contradicts' association (mneme_a → mneme_b, weight 1.0)
    │           feeds into recall scoring as negative boost (-0.5x)
    │
    └── resolve_contradiction(id, 'dismissed')
            │
            └── no confidence change, candidate closed
```

### Hebbian Interaction

When a contradiction is confirmed, the association between the pair is weakened
(weight *= 0.1) rather than deleted. This preserves the link as a signal that the
two mnemes are related, while drastically reducing the Hebbian boost either would
receive from the other's presence in recall results.

### Worker Integration

The background worker (v0.2) does not process contradiction candidates. Detection
happens synchronously via the insert trigger. Resolution is always caller-driven.

A future enhancement could have the worker periodically run
`scan_workspace_contradictions` for workspaces that haven't been scanned recently.

## Implementation

| File | Contents |
|------|----------|
| `src/contradiction.rs` | All detection logic, flagging, resolution, trigger definition, 16 unit tests |
| `src/schema.rs` | `contradiction_candidates` table, composite types, index |
| `src/integration_tests.rs` | 3 end-to-end tests (trigger, lifecycle, workspace scan) |

## Design Decisions

1. **Threshold.** 0.85 cosine similarity as the default, exposed as a parameter
   on all functions. Tunable per deployment without code changes.

2. **Concept matching.** Exact match only for `concept_overlap` in v0.3. Fuzzy
   or embedding-based concept similarity deferred to a future version.

3. **Confidence penalty direction.** The *newer* mneme (`mneme_a`) is penalized
   on confirmed contradiction. Established memories with accumulated evidence
   should not be overturned by a single insertion — the new memory bears the
   burden of proof. If it is genuinely correct, it will recover confidence
   through use and confirmation. This sets a high bar and functions as a quality
   gate.

4. **Association weakening.** On confirmed contradiction, the Hebbian association
   between the pair is weakened (weight *= 0.1), not deleted. This preserves
   the relational signal while drastically reducing co-activation boost. A
   separate `contradicts` typed association is also created, which feeds into
   recall scoring as a negative boost (see typed-memory-system.md).

## Future Considerations

- **Adaptive thresholds.** Per-workspace or per-concept threshold tuning based
  on observed false-positive rates.
- **Fuzzy concept matching.** Use text similarity or concept embedding distance
  instead of exact match for `concept_overlap`.
- **Confidence-aware penalty scaling.** If the older mneme already has low
  confidence (< 0.3), the penalty on the newer mneme could be reduced — a weak
  prior doesn't deserve strong defense.
