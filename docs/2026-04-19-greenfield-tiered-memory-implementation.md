# Greenfield Tiered Memory — Implementation Plan (v1a)

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to
> implement this plan task-by-task.

**Goal:** Ship v1a of the tiered memory system — Sietch (SQLite working),
Episodic (Postgres raw), Semantic (Postgres distilled via pg_ghola v2),
Pipeline A continuous consolidation, Pipeline B nightly distillation,
Chapterhouse REST surface, Ghola local service with HTTP/JSON + MCP,
docker-compose local dev stack, and a drop-and-recreate production deploy.

**Architecture:** Three tiers, two substrates (SQLite + Postgres), one
canonical Go interface exposed twice (MCP + HTTP/JSON). Working is local
and ephemeral; episodic + semantic are centralized in Chapterhouse's
Postgres. Pipeline A (no LLM, inside local service) promotes working →
episodic; Pipeline B (LLM-assisted, inside Chapterhouse) distills episodic
→ semantic overnight. Cognitive primitives (ACT-R, Hebbian, Bayesian,
contradiction) run inside Postgres via the pg_ghola v2 Rust extension.

**Tech Stack:** Rust (pgrx 0.17, pgvector, HNSW) · Go 1.22+ (pgx,
mattn/sqlite, asg017/sqlite-vec, github.com/mark3labs/mcp-go) ·
Postgres 18 for local dev (pg16/17 support deferred to Phase 10 based
on CNPG image in prod) · SQLite with sqlite-vec + FTS5 · TypeScript
(pi-mono ext) · docker-compose · ArgoCD for production · local vLLM
(Gemma) for Mentat.

**Design doc:** `docs/2026-04-19-greenfield-tiered-memory-design.md`

> **Monorepo pivot (2026-04-19):** Phase 0 originally created three
> separate repos (`pg_ghola`, `chapterhouse`, `ghola`). Mid-plan we
> consolidated into a single `logan-broit/ghola` monorepo holding all
> components (extension/, cmd/, internal/, clients/, deploy/), with
> history for pg_ghola and chapterhouse imported via `git subtree add`.
> Chapterhouse content is staged at `_chapterhouse/` and migrates into
> the root module incrementally during Phases 2, 3, 8. All work now
> happens on `main` of the single repo; per-phase feature branches
> optional.

---

## Scope & non-scope

**In scope for v1a:**
- Repo layout decisions (Phase 0)
- pg_ghola v2 Rust extension with 5-table semantic schema (Phase 1)
- Episodic Postgres schema + migrations (Phase 2)
- Chapterhouse `/v1/episodic/*` and `/v1/semantic/*` REST endpoints
  (Phase 3)
- Ghola local service: HTTP/JSON + SQLite Sietch + core library (Phase 4)
- Pipeline A worker inside local service (Phase 5)
- MCP wrapper inside local service (Phase 6)
- Pi-mono TypeScript extension (Phase 7)
- Pipeline B worker inside Chapterhouse (Phase 8)
- docker-compose local-dev stack (Phase 9)
- Production deploy: drop-and-recreate DB, ArgoCD manifest updates
  (Phase 10)
- Acceptance-criteria integration tests (Phase 11)

**Explicit non-scope (per design doc):** procedural/prospective/spatial/
flashbulb tiers, priming, cross-device working sync, LongMemEval
benchmarking, encoding-strategy iteration, multi-tenant Chapterhouse,
federated cross-team sharing, LLM-free entity extraction upgrades.

## Acceptance gates (from design doc v1a success criteria)

Each gate is a concrete integration test in Phase 11. An implementation
task is not "done" until its relevant gate passes.

1. A pi-mono agent and a Claude Code agent both call `record` / `recall`
   against the same local service and get consistent results.
2. Pipeline A runs continuously without errors over a 1-hour session;
   episodic grows as expected; watermark advances idempotently.
3. Pipeline B runs on a cron; semantic mnemes get created from recurring
   episodic patterns; cognitive primitives fire correctly.
4. `share` exposes a session/branch to team; another user's `recall` picks
   it up from episodic.
5. Fresh `CREATE EXTENSION pg_ghola` installs cleanly on an empty DB with
   the simplified v2 schema.
6. Local service runs on workstation + laptop without Postgres credentials
   on either device — only a per-user chapterhouse API key.
7. `docker-compose up` in the ghola repo produces a working end-to-end
   stack on a developer laptop in under 30 seconds, with no external
   dependencies beyond the embedding model download.

---

## Global conventions

- **Commits:** every task ends with a commit. Conventional messages:
  `feat:`, `fix:`, `refactor:`, `test:`, `chore:`, `docs:`.
- **Branching:** single monorepo (`logan-broit/ghola`); work lands on
  `main` directly (atomic commits per task). Per-phase feature branches
  (`phase-<N>-<slug>`) are optional; use them if a phase is large or
  needs a PR for review.
- **TDD order:** write failing test → run it and confirm failure → write
  minimal code → run it and confirm pass → commit.
- **Never skip hooks:** no `--no-verify`. If a hook fails, fix the
  underlying issue.
- **Never delete identity files or workstation configs.**

---

# Phase 0 — Repo cleanup & monorepo setup — **DONE (2026-04-19)**

**What actually shipped:** Phase 0 pivoted from three separate repos to
one `logan-broit/ghola` monorepo. The resulting layout:

| Path | Contents |
|---|---|
| `/home/loganb/ghola/` | Monorepo root (single `go.mod`, `Makefile`) |
| `/home/loganb/ghola/extension/` | Rust `pg_ghola` extension (imported via `git subtree add` from the old repo's `v2-greenfield` branch; full history preserved) |
| `/home/loganb/ghola/_chapterhouse/` | Staging area for the former chapterhouse repo (imported via `git subtree add` from `v2-tiered`); migrates into `cmd/ch-server/` + `internal/handler/` + `internal/repository/` + `internal/pipeline_b/` during Phases 2, 3, 8 |
| `/home/loganb/ghola/cmd/{ghola,ghola-mcp}/` | Local service binaries (scaffolded, Phase 4 writes code) |
| `/home/loganb/ghola/internal/{core,sietch,pipeline_a,http,mcp,chapterhouse}/` | Shared Go libs (scaffolded) |
| `/home/loganb/ghola/clients/pi-mono-ext/` | TS pi-mono extension (scaffolded, Phase 7) |
| `/home/loganb/ghola/deploy/docker-compose/` | Dev stack (scaffolded, Phase 9) |
| `/home/loganb/ghola/docs/` | Design doc, simplex spec, this implementation plan, `assets/GholaArchitecture.tsx` |
| `/home/loganb/longmemeval-ghola/` | Left in place — out of v1a scope, revived for benchmarking post-v1a |

Deviations from the original Phase 0 text:
- Did **not** archive `longmemeval-ghola` (user keeping for later benchmarking).
- Did **not** create three separate repos — consolidated to one.
- Did **not** use `v2-greenfield` / `v2-tiered` feature branches going forward — monorepo `main` is the working branch. The subtree imports preserve those branch histories.
- Pre-existing clutter in the old pg_ghola clone (orphaned `worktrees/implement_scoring_primitives/`, stale analysis scripts, untracked migration SQLs) cleaned up on pg_ghola's `main` before subtree-import.

Original Phase 0 task text below is kept for historical reference only —
do not execute.

<details>
<summary>Original Phase 0 tasks (historical)</summary>
| `/home/loganb/ai/pi-mono` (upstream fork of `badlogic/pi-mono`) | TS agent | **Leave untouched. Ghola extension is a new package in the new `ghola` repo, not a pi-mono fork.** |
| *(new)* `/home/loganb/ghola` | — | **Create.** New Go monorepo for local service + pi-mono extension + docker-compose. |

### Task 0.1: Archive longmemeval-ghola

**Files:**
- Move: `/home/loganb/longmemeval-ghola/` → `/home/loganb/archived/2026-04-19-longmemeval-ghola/`

**Step 1:** Create archive directory:

```bash
mkdir -p /home/loganb/archived
```

**Step 2:** Move the folder:

```bash
mv /home/loganb/longmemeval-ghola /home/loganb/archived/2026-04-19-longmemeval-ghola
```

**Step 3:** Drop a stub README in the archived folder noting why:

```bash
cat > /home/loganb/archived/2026-04-19-longmemeval-ghola/ARCHIVED.md <<'EOF'
# Archived 2026-04-19

LongMemEval harness is out-of-scope for v1a of the greenfield tiered
memory design (see pg_ghola/docs/plans/2026-04-19-greenfield-tiered-
memory-design.md). Revive when "Encoding-strategy iteration" leaves the
deferral list.
EOF
```

**Step 4:** Verify:

```bash
ls /home/loganb/archived/2026-04-19-longmemeval-ghola/
```

Expected: contents of former `longmemeval-ghola/` plus `ARCHIVED.md`.

**Step 5:** No commit — archiving is filesystem-only; the contents keep
their own git history intact.

### Task 0.2: Open v2 branches on pg_ghola and chapterhouse

**Files:** (two existing repos)

**Step 1:** Create branch in pg_ghola:

```bash
cd /home/loganb/ghola/extension
git checkout -b v2-greenfield
```

**Step 2:** Create branch in chapterhouse:

```bash
cd /home/loganb/ghola/_chapterhouse
git checkout -b v2-tiered
```

**Step 3:** Push both branches to remotes:

```bash
cd /home/loganb/ghola/extension && git push -u origin v2-greenfield
cd /home/loganb/ghola/_chapterhouse && git push -u logan v2-tiered
```

Expected: both branches visible on GitHub, tracking set.

### Task 0.3: Create `ghola` repo

**Files:**
- Create: `/home/loganb/ghola/` (Go module root)
- Create: `/home/loganb/ghola/.gitignore`
- Create: `/home/loganb/ghola/go.mod`
- Create: `/home/loganb/ghola/README.md`
- Create: `/home/loganb/ghola/LICENSE` (MIT, mirror pg_ghola's)

**Step 1:** Create directory and init git:

```bash
mkdir -p /home/loganb/ghola
cd /home/loganb/ghola
git init -b main
```

**Step 2:** Initialize Go module:

```bash
go mod init github.com/logan-broit/ghola
```

**Step 3:** Populate `.gitignore`:

```
/ghola
/ghola-mcp
/ghola-*.exe
/.ghola/
*.db
*.db-journal
*.sqlite
*.sqlite-journal
/node_modules/
/dist/
/.env
/.env.local
/coverage/
/tmp/
```

**Step 4:** Stub `README.md` (short — one paragraph + link to design doc).

**Step 5:** Copy `LICENSE` from `/home/loganb/ghola/extension/LICENSE` (same MIT).

**Step 6:** First commit:

```bash
git add .gitignore go.mod README.md LICENSE
git commit -m "chore: init ghola repo (greenfield local service)"
```

**Step 7:** Create GitHub repo (requires manual auth if gh not cached):

```bash
gh repo create logan-broit/ghola --private --source=. --remote=origin --push
```

Expected: new repo at `github.com/logan-broit/ghola`, branch `main`
pushed, remote `origin` set.

### Task 0.4: Lay out `ghola` monorepo directories

**Files:**
- Create (empty placeholders): `/home/loganb/ghola/{cmd/ghola,cmd/ghola-mcp,internal/core,internal/sietch,internal/pipeline_a,internal/http,internal/mcp,internal/chapterhouse,clients/pi-mono-ext,deploy/docker-compose,docs,test}/.keep`

**Step 1:** Create the directory skeleton:

```bash
cd /home/loganb/ghola
mkdir -p cmd/ghola cmd/ghola-mcp
mkdir -p internal/{core,sietch,pipeline_a,http,mcp,chapterhouse}
mkdir -p clients/pi-mono-ext
mkdir -p deploy/docker-compose
mkdir -p docs test
for d in cmd/ghola cmd/ghola-mcp internal/core internal/sietch internal/pipeline_a internal/http internal/mcp internal/chapterhouse clients/pi-mono-ext deploy/docker-compose docs test; do
  touch "$d/.keep"
done
```

**Step 2:** Drop a `docs/layout.md` briefly explaining each directory
(~120 words).

**Step 3:** Commit:

```bash
git add .
git commit -m "chore: scaffold monorepo directories"
git push
```

### Task 0.5: Copy the greenfield design doc into ghola for colocation

**Files:**
- Copy: `/home/loganb/ghola/extension/docs/plans/2026-04-19-greenfield-tiered-memory-design.md`
  → `/home/loganb/ghola/docs/2026-04-19-greenfield-tiered-memory-design.md`
- Copy this plan in similarly once committed.

**Step 1:**
```bash
cp /home/loganb/ghola/extension/docs/plans/2026-04-19-greenfield-tiered-memory-design.md \
   /home/loganb/ghola/docs/
```

**Step 2:** Commit in ghola repo:

```bash
cd /home/loganb/ghola
git add docs/
git commit -m "docs: colocate greenfield design doc"
git push
```

### Gate 0 (sanity): all four repos in expected state

Run:
```bash
ls /home/loganb/{pg_ghola,chapterhouse,ghola} /home/loganb/archived/
git -C /home/loganb/ghola/extension branch --show-current
git -C /home/loganb/ghola/_chapterhouse branch --show-current
git -C /home/loganb/ghola branch --show-current
```

Expected: pg_ghola on `v2-greenfield`, chapterhouse on `v2-tiered`, ghola
on `main`, archived dir holds longmemeval-ghola. Proceed to Phase 1.

</details>

---

# Phase 1 — pg_ghola v2 Rust extension (5-table semantic schema)

**Purpose:** Strip `src/` down to v2. Remove sub_mnemes, cluster pathway,
gating columns, temporal-token infra, matched_position. Keep ACT-R,
Hebbian, Bayesian, contradiction, archival. Fresh `CREATE EXTENSION`
installs on an empty DB (Gate 5).

**Branch:** `main` (monorepo). All edits land under `extension/` in
`/home/loganb/ghola/`.

### Task 1.1: Inventory what to delete vs keep

**Files:** read-only inventory, no writes.

**Step 1:** List current src:

```bash
ls /home/loganb/ghola/extension/src/
```

Existing files: `associations.rs`, `bin/`, `consolidation_worker.rs`,
`contradiction.rs`, `contradiction_worker.rs`, `gating_worker.rs`,
`hebbian.rs`, `integration_tests.rs`, `lib.rs`, `recall.rs`, `schema.rs`,
`scoring.rs`, `types.rs`, `worker_stats.rs`.

**Step 2:** Write `docs/plans/v2-src-inventory.md` listing, per file,
one of: KEEP / SIMPLIFY / DELETE with one-line reason. Minimum:
- `lib.rs` — KEEP (re-wire exports)
- `schema.rs` — SIMPLIFY (5 tables, no sub_mnemes / clusters / gating)
- `recall.rs` — SIMPLIFY (drop matched_position, drop sub_mnemes join)
- `types.rs` — SIMPLIFY (8-col recall_result, drop sub-mneme types)
- `scoring.rs` — KEEP (ACT-R + softplus; unchanged)
- `hebbian.rs` + `associations.rs` — KEEP
- `contradiction.rs` + `contradiction_worker.rs` — KEEP
- `consolidation_worker.rs` — KEEP (decay + archival)
- `gating_worker.rs` — DELETE (gating columns gone)
- `worker_stats.rs` — KEEP
- `integration_tests.rs` — REWRITE (covers only v2 surface)
- `bin/` — inspect + decide per file

**Step 3:** Commit:

```bash
cd /home/loganb/ghola/extension
git add docs/plans/v2-src-inventory.md
git commit -m "docs: v2 src inventory (keep/simplify/delete)"
```

### Task 1.2: Write failing schema test for v2 shape

**Files:**
- Create: `/home/loganb/ghola/extension/src/tests/schema_v2.rs` (new test module)
- Modify: `/home/loganb/ghola/extension/src/lib.rs` — register test module gated
  behind `#[cfg(any(test, feature="pg_test"))]`.

**Step 1:** Write test asserting that after `CREATE EXTENSION pg_ghola`
on an empty DB, exactly these tables exist under schema `ghola`:

```rust
// src/tests/schema_v2.rs
#[cfg(any(test, feature = "pg_test"))]
mod tests {
    use pgrx::prelude::*;

    #[pg_test]
    fn v2_schema_has_five_tables_only() {
        let tables: Vec<String> = Spi::get_one(
            "SELECT array_agg(tablename ORDER BY tablename)::text \
             FROM pg_tables WHERE schemaname = 'semantic'"
        ).unwrap().unwrap();
        assert_eq!(
            tables,
            "{associations,co_activation_queue,contradiction_candidates,\
             contradiction_queue,mnemes}"
        );
    }

    #[pg_test]
    fn v2_has_no_sub_mnemes_no_clusters() {
        let count: i64 = Spi::get_one(
            "SELECT count(*) FROM pg_tables \
             WHERE tablename IN ('sub_mnemes','clusters','mneme_clusters')"
        ).unwrap().unwrap();
        assert_eq!(count, 0);
    }

    #[pg_test]
    fn mnemes_has_contributor_user_ids_column() {
        let exists: bool = Spi::get_one(
            "SELECT EXISTS (SELECT 1 FROM information_schema.columns \
             WHERE table_schema='semantic' AND table_name='mnemes' \
               AND column_name='contributor_user_ids')"
        ).unwrap().unwrap();
        assert!(exists);
    }
}
```

**Step 2:** Run and confirm fail:

```bash
cd /home/loganb/ghola/extension
cargo pgrx test pg18 2>&1 | grep -E "(FAIL|PASS|test result)"
```

Expected: the three new tests fail (current schema has 12 tables under
`ghola`, not 5 under `semantic`).

**Step 3:** Commit test-only:

```bash
git add src/tests/schema_v2.rs src/lib.rs
git commit -m "test: failing schema tests for v2 five-table shape"
```

### Task 1.3: Rewrite `schema.rs` for v2

**Files:**
- Modify: `/home/loganb/ghola/extension/src/schema.rs` — replace existing
  `extension_sql_file!` body with v2 SQL matching the design doc
  (schema `semantic`, 5 tables, indexes per design).
- Delete: any `create_sub_mnemes_table`, `create_clusters_table`,
  gating-related functions.

**Step 1:** Implement. SQL body (exact):

```sql
CREATE SCHEMA IF NOT EXISTS semantic;

CREATE TABLE semantic.mnemes (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id          uuid NOT NULL,
    concept               text NOT NULL,
    content               text NOT NULL,
    embedding             vector(${EMBEDDING_DIM}) NOT NULL,  -- dim substituted by migration runner
    search_vector         tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', concept), 'A') ||
        setweight(to_tsvector('english', content), 'B')
    ) STORED,
    confidence            double precision NOT NULL DEFAULT 0.5,
    access_count          integer NOT NULL DEFAULT 0,
    last_access           timestamptz NOT NULL DEFAULT now(),
    created_at            timestamptz NOT NULL DEFAULT now(),
    state                 text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active','archived')),
    memory_type           text NOT NULL DEFAULT 'factual'
        CHECK (memory_type IN ('factual','experiential','procedural')),
    tags                  text[] NOT NULL DEFAULT '{}',
    entities              text[] NOT NULL DEFAULT '{}',
    source_episodic_ids   uuid[] NOT NULL DEFAULT '{}',
    contributor_user_ids  uuid[] NOT NULL DEFAULT '{}'
);
CREATE INDEX mnemes_workspace      ON semantic.mnemes (workspace_id);
CREATE INDEX mnemes_embedding_hnsw ON semantic.mnemes USING hnsw (embedding vector_cosine_ops);
CREATE INDEX mnemes_search_gin     ON semantic.mnemes USING gin (search_vector);
CREATE INDEX mnemes_entities_gin   ON semantic.mnemes USING gin (entities);
CREATE INDEX mnemes_tags_gin       ON semantic.mnemes USING gin (tags);
CREATE INDEX mnemes_last_access    ON semantic.mnemes (last_access DESC)
    WHERE state = 'active';

CREATE TABLE semantic.associations (
    src_id            uuid NOT NULL REFERENCES semantic.mnemes(id) ON DELETE CASCADE,
    dst_id            uuid NOT NULL REFERENCES semantic.mnemes(id) ON DELETE CASCADE,
    association_type  text NOT NULL
        CHECK (association_type IN ('hebbian','contradicts','supersedes','supports')),
    weight            double precision NOT NULL DEFAULT 0.01,
    co_activations    integer NOT NULL DEFAULT 0,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (src_id, dst_id, association_type)
);

CREATE TABLE semantic.co_activation_queue (
    id         bigserial PRIMARY KEY,
    src_id     uuid NOT NULL,
    dst_id     uuid NOT NULL,
    enqueued_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE semantic.contradiction_queue (
    id         bigserial PRIMARY KEY,
    mneme_id   uuid NOT NULL,
    enqueued_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE semantic.contradiction_candidates (
    id           bigserial PRIMARY KEY,
    mneme_a      uuid NOT NULL REFERENCES semantic.mnemes(id) ON DELETE CASCADE,
    mneme_b      uuid NOT NULL REFERENCES semantic.mnemes(id) ON DELETE CASCADE,
    similarity   double precision NOT NULL,
    detected_at  timestamptz NOT NULL DEFAULT now(),
    status       text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','resolved','dismissed'))
);
```

**Step 2:** Run tests:

```bash
cd /home/loganb/ghola/extension
cargo pgrx test pg18
```

Expected: the three schema tests now pass. Any test that referenced the
old tables will fail; keep those failing until Task 1.5 rewires.

**Step 3:** Commit:

```bash
git add src/schema.rs
git commit -m "refactor: v2 semantic schema (5 tables)"
```

### Task 1.4: Rewrite `types.rs` — 8-column recall_result

**Files:**
- Modify: `/home/loganb/ghola/extension/src/types.rs` — drop `matched_position`
  from `recall_result`. Drop any sub-mneme types.

**Step 1:** Write failing test in
`src/tests/recall_signature.rs`:

```rust
#[pg_test]
fn recall_result_has_eight_columns() {
    let cols: i64 = Spi::get_one(
        "SELECT count(*)::bigint FROM pg_attribute a \
         JOIN pg_type t ON t.typrelid = a.attrelid \
         WHERE t.typname = 'recall_result' AND a.attnum > 0"
    ).unwrap().unwrap();
    assert_eq!(cols, 8);
}
```

**Step 2:** Run; confirm fail (currently 9).

**Step 3:** Edit `types.rs`: remove `matched_position: i16` from the
`extension_sql!` block and from the associated Rust struct.

**Step 4:** Run; confirm pass.

**Step 5:** Commit:

```bash
git add src/types.rs src/tests/recall_signature.rs
git commit -m "refactor: recall_result back to 8 columns (no sub_mnemes)"
```

### Task 1.5: Rewire `recall.rs` — no sub_mnemes join

**Files:**
- Modify: `/home/loganb/ghola/extension/src/recall.rs` — remove sub_mneme UNION/
  JOIN, all references to `matched_position`. Recall now scans only
  `semantic.mnemes`.
- Modify: `/home/loganb/ghola/extension/src/lib.rs` — drop any
  `recall_inner` signature params that existed only for sub_mnemes.

**Step 1:** Write failing test that calls `semantic.recall(...)` and
expects 8 columns:

```rust
#[pg_test]
fn recall_returns_eight_column_rows() {
    Spi::run("INSERT INTO semantic.mnemes (workspace_id, concept, content, embedding) \
              VALUES (gen_random_uuid(), 'hello', 'world', \
              (SELECT ('[' || string_agg('0.01', ',') || ']')::vector \
               FROM generate_series(1, 1024)))").unwrap();
    let cols: i64 = Spi::get_one(
        "SELECT count(*) FROM (\
         SELECT * FROM semantic.recall(gen_random_uuid(), 'hello', \
         (SELECT ('[' || string_agg('0.01', ',') || ']')::vector \
          FROM generate_series(1, 1024)), 10)) t"
    ).unwrap().unwrap();
    assert!(cols >= 0);
}
```

**Step 2:** Run; confirm fail (function still references `sub_mnemes`).

**Step 3:** Rewrite `recall.rs`:
- `recall_inner` becomes: vector/FTS over `semantic.mnemes` only, returns
  8 columns (mneme_id, score, content_match, activation, hebbian_boost,
  confidence, concept, content).
- Move schema from `ghola.*` to `semantic.*`.
- Drop `filter_session_id` and any param that only made sense for
  sub_mnemes.

**Step 4:** Run; confirm pass.

**Step 5:** Commit:

```bash
git add src/recall.rs src/lib.rs
git commit -m "refactor: recall targets semantic.mnemes only (no sub_mnemes)"
```

### Task 1.6: Delete gating infra

**Files:**
- Delete: `/home/loganb/ghola/extension/src/gating_worker.rs`
- Modify: `/home/loganb/ghola/extension/src/lib.rs` — remove its mod line and any
  BGWorker registrations tied to it.
- Modify: `/home/loganb/ghola/extension/pg_ghola.control` if references exist.

**Step 1:** Delete file:

```bash
cd /home/loganb/ghola/extension
git rm src/gating_worker.rs
```

**Step 2:** Update `lib.rs` — drop `mod gating_worker;`, drop its
`bgworker_start()` call.

**Step 3:** Run full test suite:

```bash
cargo pgrx test pg18
```

Expected: compiles and existing tests pass.

**Step 4:** Commit:

```bash
git add src/lib.rs
git commit -m "refactor: drop gating worker (no gating columns in v2)"
```

### Task 1.7: Rewire Hebbian + contradiction + consolidation workers

**Files:**
- Modify: `/home/loganb/ghola/extension/src/hebbian.rs` — if it references
  `ghola.mnemes`, change to `semantic.mnemes`.
- Modify: `/home/loganb/ghola/extension/src/associations.rs` — same.
- Modify: `/home/loganb/ghola/extension/src/contradiction.rs` + `_worker.rs` — same.
- Modify: `/home/loganb/ghola/extension/src/consolidation_worker.rs` — same.
- Modify: `/home/loganb/ghola/extension/src/scoring.rs` — same.

**Step 1:** For each file, run:

```bash
grep -n "ghola\." src/*.rs
```

Rewrite every `ghola.mnemes` → `semantic.mnemes`, same for associations
and queues. Do not touch scoring math.

**Step 2:** Run `cargo pgrx test pg18`. Fix any compile errors.

**Step 3:** Commit:

```bash
git add src/
git commit -m "refactor: rename ghola.* schema refs to semantic.*"
```

### Task 1.8: Rewrite `integration_tests.rs` for v2 surface

**Files:**
- Modify: `/home/loganb/ghola/extension/src/integration_tests.rs` — remove all
  sub_mneme tests, cluster tests, gating tests. Add tests covering:
  - insert → recall round-trip
  - Hebbian weight update fires from co_activation_queue
  - contradiction_worker flags high-cosine divergent mneme pairs
  - decay drops `last_access`-old mnemes' activation
  - archival flips `state='active'` → `state='archived'` at age threshold

**Step 1:** Write 5 tests, one per bullet above. Use `#[pg_test]`.

**Step 2:** Run:

```bash
cargo pgrx test pg18
```

Expected: all 5 pass on first run, because the workers kept their logic.

**Step 3:** Commit:

```bash
git add src/integration_tests.rs
git commit -m "test: v2 integration test suite (insert/recall/primitives)"
```

### Task 1.9: Bump extension version → `0.2.0`, update control file

**Files:**
- Modify: `/home/loganb/ghola/extension/pg_ghola.control`
- Modify: `/home/loganb/ghola/extension/Cargo.toml` (version bump)
- Delete: old `.sql` migration files
  (`migration-0.0.4-thalamic-gating.sql`, `migration-0.0.5-temporal-tokens.sql`,
   `migration-0.0.6-sub-mnemes.sql`) — v2 is fresh install, no migrations.

**Step 1:** Bump version:
- `pg_ghola.control`: `default_version = '0.2.0'`
- `Cargo.toml`: `version = "0.2.0"`

**Step 2:** Delete migrations:

```bash
git rm migration-0.0.*.sql
```

**Step 3:** Build the new SQL file:

```bash
cargo pgrx schema pg16 > pg_ghola--0.2.0.sql
```

Confirm the file exists and contains the new 5-table schema.

**Step 4:** Run smoke test on empty DB (Gate 5):

```bash
createdb ghola_smoke
psql ghola_smoke -c "CREATE EXTENSION pg_ghola"
psql ghola_smoke -c "\dt semantic.*"
```

Expected: lists exactly the 5 tables.

**Step 5:** Commit:

```bash
git add -A
git commit -m "release: pg_ghola v0.2.0 (greenfield v2 schema)"
```

### Task 1.10: Build & ship `pg_ghola` container image

**Files:**
- Modify: `/home/loganb/ghola/extension/Dockerfile.cnpg` — confirm it still
  symlinks `pg_ghola.so` → `pg_recall.so` (the prod extension is named
  `pg_recall`; CNPG install expects that name).

**Step 1:** Build:

```bash
cd /home/loganb/ghola/extension
docker build -f Dockerfile.cnpg -t ghcr.io/logan-broit/pg-ghola:0.2.0 .
```

Expected: build succeeds.

**Step 2:** Import into k3s on NUC (deferred to Phase 10 — don't push to
prod yet). For now just verify it runs locally:

```bash
docker run --rm ghcr.io/logan-broit/pg-ghola:0.2.0 pg_config --sharedir
```

**Step 3:** Commit any Dockerfile changes:

```bash
git add Dockerfile.cnpg
git commit -m "build: pg_ghola 0.2.0 image builds clean"
git push
```

### Gate 1

- `cargo pgrx test pg18` passes.
- `createdb ghola_smoke && psql -c "CREATE EXTENSION pg_ghola"` succeeds
  on an empty DB with the 5-table schema.
- No references to `sub_mnemes`, `matched_position`, `clusters`,
  `gating_*` remain in `src/`.

If all three hold, tag the Phase 1 completion commit (e.g. `extension-v0.2.0`) on `main`.

---

# Phase 2 — Episodic Postgres schema

**Purpose:** Ship the episodic schema (`episodic.*`) that Pipeline A
writes to and `/v1/episodic/*` reads from. This schema is NOT managed by
pg_ghola — it's plain Postgres DDL, applied by Chapterhouse at boot.

**Branch:** `main` (monorepo). Work occurs in `_chapterhouse/` and
migrates into `cmd/ch-server/` + `internal/{handler,repository,pipeline_b}/`
as each piece is touched.

### Task 2.1: Write schema migration SQL

**Files:**
- Create: `/home/loganb/ghola/_chapterhouse/ch-server/internal/repository/migrations/001_episodic.sql`
  containing the DDL from the design doc verbatim (sessions, turns, shares
  + all indexes).

**Step 1:** Create directory if absent and write file.

**Step 2:** Add a `0000_schemas.sql` that ensures both schemas exist:

```sql
CREATE SCHEMA IF NOT EXISTS episodic;
CREATE EXTENSION IF NOT EXISTS pgcrypto; -- for gen_random_uuid
CREATE EXTENSION IF NOT EXISTS vector;   -- pgvector
```

### Task 2.2: Write failing integration test

**Files:**
- Create: `/home/loganb/ghola/_chapterhouse/ch-server/internal/repository/episodic_schema_test.go`

**Step 1:** Test that, after running migrations on a fresh Postgres:

```go
func TestEpisodicSchemaHasExpectedTables(t *testing.T) {
    db := testutil.NewPostgres(t) // spins up ephemeral pg with extensions
    ApplyMigrations(t, db)
    got := queryTables(t, db, "episodic")
    want := []string{"sessions", "shares", "turns"}
    require.Equal(t, want, got)
}

func TestEpisodicTurnsHasEmbeddingColumn(t *testing.T) {
    db := testutil.NewPostgres(t)
    ApplyMigrations(t, db)
    typ := columnType(t, db, "episodic", "turns", "embedding")
    require.Equal(t, "vector", typ)
}
```

**Step 2:** Run:

```bash
cd /home/loganb/ghola/_chapterhouse/ch-server
go test ./internal/repository/... -run TestEpisodic -v
```

Expected: fail (migration runner doesn't exist yet).

### Task 2.3: Implement migration runner

**Files:**
- Create: `/home/loganb/ghola/_chapterhouse/ch-server/internal/repository/migrate.go`
  with a simple embed-based migrator that applies `*.sql` in alpha order
  inside a transaction, tracking applied versions in
  `_migrations.applied(name text primary key)`.

**Step 1:** Implement, ~60 lines of Go. The runner MUST substitute
`${EMBEDDING_DIM}` tokens in migration SQL before execution, reading
the value from the `EMBEDDING_DIM` environment variable. Fail fast if
`EMBEDDING_DIM` is unset or not a positive integer. This is how we keep
the substrate dimension-agnostic — same migration file runs against
768d, 1024d, 1536d, etc. without editing.

**Step 2:** Run tests:

```bash
EMBEDDING_DIM=1024 go test ./internal/repository/... -run TestEpisodic -v
```

Expected: both pass. Repeat with `EMBEDDING_DIM=384` and confirm the
same tests pass against a DB where `\d episodic.turns` reports
`embedding | vector(384)`.

**Step 3:** Commit:

```bash
git add ch-server/internal/repository/
git commit -m "feat: episodic migrations + runner with \${EMBEDDING_DIM} substitution"
```

### Task 2.3b: Dimension-agnosticism test

**Files:**
- Create: `ch-server/internal/repository/dimension_test.go`

**Step 1:** Write a test that:
  - Spins up Postgres
  - Runs migrations with `EMBEDDING_DIM=384`
  - Asserts `episodic.turns.embedding` is `vector(384)`
  - Inserts a 384-vector, recalls it
  - Tears down
  - Repeats with `EMBEDDING_DIM=1536`, asserts `vector(1536)`

**Step 2:** Run; expect pass. Commit. This locks in the dimension-
agnostic invariant from `CONSTRAINT: swappable_models_and_dimensions`.

### Task 2.4: Wire migrations into server boot

**Files:**
- Modify: `/home/loganb/ghola/_chapterhouse/ch-server/cmd/ch-server/main.go`
  (or equivalent entrypoint) — call `repository.ApplyMigrations(ctx, pool)`
  after pool init, before HTTP start.

**Step 1:** Add call with explicit error-on-fail.

**Step 2:** Local smoke test:

```bash
cd /home/loganb/ghola/_chapterhouse
docker compose -f deploy/dev-compose.yml up -d postgres
CH_DATABASE_URL=postgres://... go run ./ch-server/cmd/ch-server
```

Expected: log line `applied migration 0000_schemas.sql` and
`applied migration 001_episodic.sql`, then server up.

**Step 3:** Commit:

```bash
git add ch-server/cmd/ch-server/main.go
git commit -m "feat: apply episodic migrations at server boot"
```

### Gate 2

- Fresh Postgres → `ch-server` boots → `\dt episodic.*` shows
  `sessions`, `shares`, `turns`.
- All pgvector / FTS / GIN indexes present (`\d episodic.turns`).

---

# Phase 3 — Chapterhouse internal REST surface

**Purpose:** Replace the legacy agent-facing MCP tools (`remember`,
`recall`, `forget`, `remember_session`, etc.) with internal
`/v1/episodic/*` and `/v1/semantic/*` endpoints called only by the Ghola
local service.

**Branch:** `main` (monorepo). Work occurs in `_chapterhouse/` and
migrates into `cmd/ch-server/` + `internal/{handler,repository,pipeline_b}/`
as each piece is touched.

### Task 3.1: Archive legacy MCP tool surface

**Files:**
- Move: `ch-server/internal/mcp/` → `ch-server/internal/mcp_legacy/`
  (keeps the code but takes it off the compile path pending removal).

**Step 1:** Move the directory and update any `package mcp` → `package
mcp_legacy` in the moved files.

**Step 2:** Remove routing to `mcp_legacy` from `cmd/ch-server/main.go`
— temporarily the server exposes NO MCP tools.

**Step 3:** Confirm compile:

```bash
go build ./...
```

**Step 4:** Commit:

```bash
git add .
git commit -m "refactor: park legacy MCP tool surface as mcp_legacy"
```

### Task 3.2: Define REST contract (OpenAPI stub)

**Files:**
- Create: `/home/loganb/ghola/_chapterhouse/docs/api/v1-chapterhouse.yaml` —
  minimal OpenAPI 3.1 listing all 7 endpoints from the design doc
  (`episodic/{ingest,query,share,forget}`, `semantic/{query,feedback,list}`)
  with request/response shapes.

**Step 1:** Write the YAML. Keep it small — paths + one schema per
request/response.

**Step 2:** Commit:

```bash
git add docs/api/v1-chapterhouse.yaml
git commit -m "docs: v1 chapterhouse API contract"
```

### Task 3.3: Write handler tests (TDD — all 7 endpoints)

**Files:**
- Create: `ch-server/internal/handler/episodic_test.go`
- Create: `ch-server/internal/handler/semantic_test.go`

**Step 1:** For each endpoint write one happy-path test +
one auth/ACL rejection test. Use `httptest.NewRecorder`. Total ~14 tests.

**Step 2:** Run:

```bash
go test ./internal/handler/...
```

Expected: all fail with "no such handler" / compile errors.

**Step 3:** Commit test-only:

```bash
git add ch-server/internal/handler/
git commit -m "test: v1 episodic+semantic handler tests (failing)"
```

### Task 3.4: Implement `POST /v1/episodic/ingest`

**Files:**
- Create: `ch-server/internal/handler/episodic.go` — `IngestHandler`
- Create: `ch-server/internal/repository/episodic.go` — `InsertTurnsBatch`
- Modify: `ch-server/cmd/ch-server/main.go` — route wiring

Body shape:

```json
{
  "session": {
    "id": "uuid", "started_at": "...", "ended_at": "...", "turn_count": N
  },
  "turns": [
    { "id": "uuid", "parent_id": "uuid|null",
      "role": "user|assistant|system|tool", "content": "...",
      "tool_name": "...", "tool_input": {...}, "tool_output": {...},
      "bookmark_label": null, "embedding": [D floats; D = EMBEDDING_DIM],
      "entities": ["..."], "tags": ["..."],
      "created_at": "..." }
  ]
}
```

**Step 1:** Implement handler + repo function (transactional, UPSERT on
`(id)` for idempotency — critical for Pipeline A retry safety).

**Step 2:** Run tests:

```bash
go test ./internal/handler/... -run Ingest -v
```

Expected: pass.

**Step 3:** Commit.

### Task 3.5: Implement `POST /v1/episodic/query`

Request:
```json
{ "user_id": "...", "query_text": "...", "query_embedding": [...],
  "limit": 10, "include_shared": true,
  "filters": { "session_id": "...", "entities": [...], "tags": [...] } }
```

Response: ranked list of turns with score breakdown
(semantic cosine, FTS ts_rank, merged score) + tier=`episodic`.

**Step 1:** Implement query: vector HNSW + FTS, hybrid merge with
configurable weights (mirror pg_ghola's `w_semantic`/`w_fts` defaults).
Respect ACL: `user_id=self` OR `id IN (SELECT scope_id FROM episodic.shares WHERE ...)`

**Step 2:** Run tests (happy path + ACL deny).

**Step 3:** Commit.

### Task 3.6: Implement `POST /v1/episodic/share`, `forget`

**Step 1:** `share` inserts into `episodic.shares` with ACL validation.

**Step 2:** `forget` issues a soft delete (`content = '[forgotten]'`,
nullify embedding) to preserve tree structure. Hard delete is a separate
`owner_user_id`-only admin call, deferred.

**Step 3:** Tests + commit.

### Task 3.7: Implement `/v1/semantic/{query,feedback,list}`

**Files:**
- Create: `ch-server/internal/handler/semantic.go`
- Create: `ch-server/internal/repository/semantic.go` — calls
  `semantic.recall(...)` from pg_ghola v2 + Bayesian feedback SQL.

**Step 1:** `/query` wraps `semantic.recall`. `/feedback` applies a
Bayesian update on `confidence`. `/list` paginates
`semantic.mnemes` with filters.

**Step 2:** Tests + commit.

### Task 3.8: Auth — per-user API keys

**Files:**
- Create: `ch-server/internal/auth/apikey.go` + migration adding
  `auth.api_keys(user_id uuid, key_hash text, created_at)`.
- Modify: `ch-server/internal/middleware/authz.go` — check `Authorization:
  Bearer <api-key>` and inject `user_id` into request context. Reject if
  missing or invalid.

**Step 1:** Write auth tests first (valid key, invalid key, missing
header). Run → fail.

**Step 2:** Implement. Run → pass.

**Step 3:** Commit.

### Gate 3

- `curl -H "Authorization: Bearer <key>" -X POST http://localhost:8080/v1/episodic/ingest -d '{...}'` returns 200 + inserted rows visible in `episodic.turns`.
- `curl .../v1/semantic/query` against a seeded DB returns ranked mnemes
  with full pg_ghola v2 cognitive scoring.
- All handler tests pass.

---

# Phase 4 — Ghola local service skeleton

**Purpose:** The Go binary agents talk to. Single binary, two entrypoints
(`ghola` = HTTP, `ghola-mcp` = MCP), one shared core library.

**Branch:** `main` (monorepo). Work in `cmd/ghola/`, `cmd/ghola-mcp/`,
and `internal/{core,sietch,pipeline_a,http,mcp,chapterhouse}/`.

### Task 4.1: Write core interface (Go) — `internal/core/core.go`

**Files:**
- Create: `/home/loganb/ghola/internal/core/core.go`
- Create: `/home/loganb/ghola/internal/core/core_test.go`

**Step 1:** Failing tests for all 11 operations from the design doc:

```go
type fakeSietch struct{ /* ... */ }
type fakeChapterhouse struct{ /* ... */ }
type fakeEmbedder struct{ /* ... */ }

func TestCoreRecord(t *testing.T) { /* assert turn lands in sietch */ }
func TestCoreBranch(t *testing.T) { /* assert new branch has correct parent */ }
func TestCoreBookmark(t *testing.T) { /* bookmark_label set */ }
func TestCoreNavigate(t *testing.T) { /* current-pointer moved */ }
func TestCoreRecall(t *testing.T) { /* fans out to sietch+chapterhouse; merged */ }
func TestCoreForget(t *testing.T) { /* flags in all three tiers */ }
func TestCoreShare(t *testing.T) { /* hits chapterhouse /share */ }
func TestCoreConsolidate(t *testing.T) { /* triggers pipeline A */ }
func TestCoreSessionStart(t *testing.T) { /* creates sietch row */ }
func TestCoreSessionEnd(t *testing.T) { /* flushes + marks closed */ }
func TestCoreListSessions(t *testing.T) { /* enumerates episodic */ }
func TestCoreFeedback(t *testing.T) { /* hits chapterhouse /feedback */ }
```

**Step 2:** Run — all fail (`Core` type missing).

**Step 3:** Commit tests only.

### Task 4.2: Minimal `Core` implementation

**Files:**
- Modify: `internal/core/core.go` — define `type Core struct { Sietch SietchStore; CH ChapterhouseClient; Embedder Embedder }`
- Define interfaces: `SietchStore`, `ChapterhouseClient`, `Embedder`

**Step 1:** Implement each method as a thin orchestration calling the
interface. Write ≤20 lines per method.

**Step 2:** Run tests → all pass.

**Step 3:** Commit.

### Task 4.3: Sietch implementation (SQLite + sqlite-vec + FTS5)

**Files:**
- Create: `internal/sietch/sietch.go` + `sietch_test.go`
- Create: `internal/sietch/schema.sql` (DDL from design doc)

**Step 1:** Test cases: open session (new file), record turn, branch,
vector search, FTS search, garbage-collect expired session.

**Step 2:** Implement using `mattn/go-sqlite3` + the
`asg017/sqlite-vec` extension (load at connection open).

**Step 3:** Run → pass. Commit.

### Task 4.4: Chapterhouse HTTP client

**Files:**
- Create: `internal/chapterhouse/client.go` + `client_test.go`

**Step 1:** Test cases with `httptest.Server` for each of the 7
endpoints. Assert: correct URL, auth header present, body matches
contract, error mapping sensible.

**Step 2:** Implement — 7 methods on `type Client struct{ base string; apiKey string; http *http.Client }`.

**Step 3:** Commit.

### Task 4.5: Melange embedding client

**Files:**
- Create: `internal/embedding/melange.go` + test.

**Step 1:** Simple HTTP call to `${MELANGE_URL}/v1/embeddings`,
returns `[]float32`. Retry on 5xx with exponential backoff, budget P50
< 50ms per design doc.

**Step 2:** Tests + commit.

### Task 4.6: HTTP/JSON server (`cmd/ghola`)

**Files:**
- Create: `cmd/ghola/main.go`
- Create: `internal/http/server.go` — thin router over `core.Core`

**Step 1:** Test cases: one POST test per operation.

**Step 2:** Implement — 11 routes → 11 core calls. Server listens on
`localhost:7421` (loopback only; reject non-loopback connections in
middleware).

**Step 3:** Smoke test:

```bash
cd /home/loganb/ghola
go run ./cmd/ghola &
curl -X POST localhost:7421/v1/session_start -d '{"user_id":"u1"}'
```

Expected: 200 + `session_id`.

**Step 4:** Commit.

### Gate 4

- `go test ./...` all green in `ghola` repo.
- `go run ./cmd/ghola` serves all 11 operations.
- Core library orchestrates sietch + chapterhouse-client + embedder.
- Local service holds no Postgres credentials (Gate 6 half-satisfied).

---

# Phase 5 — Pipeline A worker (inside local service)

**Purpose:** Continuous working → episodic consolidation. No LLM. Runs
inside `cmd/ghola`. Lossless. Watermark-driven.

### Task 5.1: Watermark schema in sietch

**Files:**
- Modify: `internal/sietch/schema.sql` — add
  `CREATE TABLE pipeline_a_state (session_id TEXT PRIMARY KEY,
   last_consolidated_turn_id INTEGER NOT NULL, last_run_at INTEGER NOT NULL);`

### Task 5.2: Entity extractor (regex + simple NER)

**Files:**
- Create: `internal/pipeline_a/entities.go` + test.

**Step 1:** Failing tests for:
- Extract proper nouns (capitalized runs).
- Extract quoted "things" / `code_like_tokens`.
- Normalize Unicode + lowercase.

**Step 2:** Implement using `regexp` + a tiny allowlist. No ML model.

**Step 3:** Commit.

### Task 5.3: Pipeline A loop

**Files:**
- Create: `internal/pipeline_a/worker.go`

**Step 1:** Failing tests:
- Seeds sietch with 20 turns, runs one tick, asserts Chapterhouse
  ingest called with 20 turns, watermark advances to max turn id.
- Second tick with no new turns → no HTTP call.
- Ingest fails → watermark does NOT advance; retry next tick.

**Step 2:** Implement — tick every 5 min OR on explicit `Consolidate`
call OR on `SessionEnd`. Batch-call `Chapterhouse.EpisodicIngest`.

**Step 3:** Commit.

### Task 5.4: Hybrid-C+D coherence pass

**Files:**
- Modify: `internal/pipeline_a/worker.go` — on branch-terminal detection
  (no activity in a branch for N minutes), issue a second batch call
  with `coherence_pass=true` to rewrite the branch into a cohesive
  episodic record. For v1a, "rewrite" is concatenation with branch
  metadata; LLM rewriting is deferred.

### Gate 5

- 1-hour run with synthetic 1 turn/sec → episodic grows continuously,
  never stalls, watermark monotonic. Hits Gate 2 of the acceptance
  criteria.

---

# Phase 6 — MCP wrapper (`cmd/ghola-mcp`)

**Purpose:** Expose the same 11 operations as MCP tools for Claude Code.

### Task 6.1: MCP scaffold

**Files:**
- Create: `cmd/ghola-mcp/main.go`
- Create: `internal/mcp/server.go`

**Step 1:** Use `github.com/mark3labs/mcp-go` (or `go-mcp`). Register
11 tools wrapping the same `core.Core`.

**Step 2:** Test with `claude mcp add ghola <binary>` and call one tool.

**Step 3:** Commit.

### Gate 6

- Claude Code `/mcp` shows `ghola` connected. `record`/`recall` work via
  MCP stdio against an already-running ghola HTTP server (they share the
  core library via in-process calls, not HTTP roundtrip).

---

# Phase 7 — Pi-mono TypeScript extension

**Purpose:** Pi-mono agents call the HTTP surface.

### Task 7.1: Scaffold package

**Files:**
- Create: `/home/loganb/ghola/clients/pi-mono-ext/` (npm package
  `@logan-broit/ghola-pi-mono`).
- `package.json`, `tsconfig.json`, `src/client.ts`, `src/hooks.ts`,
  `tests/client.test.ts`.

**Step 1:** Tests for: `new GholaClient({baseUrl})`, `.record(...)`,
`.recall(...)`, each hitting `http://localhost:7421`.

**Step 2:** Implement with `fetch`. Commit.

### Task 7.2: Pi-mono integration hook

**Files:**
- `src/hooks.ts` exports `onTurn(turn) => client.record(turn)` and
  `onRecall(query) => client.recall(query)`.

**Step 1:** Integration test: mount hook into a pi-mono test agent,
verify both HTTP calls fire.

### Gate 7

- `@logan-broit/ghola-pi-mono` usable from a pi-mono app; sample app
  records and recalls successfully.

---

# Phase 8 — Pipeline B worker (inside Chapterhouse)

**Purpose:** Nightly cross-user distillation: episodic → semantic.
LLM-assisted. Cron at 02:00.

### Task 8.1: Pattern detector — entity co-occurrence

**Files:**
- Create: `ch-server/internal/pipeline_b/detect.go` + test.

**Step 1:** Test: given 5 sessions mentioning `(Postgres, CNPG)` together,
detector yields that pair with `support_count=5`. Given a pair in only
2 sessions, it's below threshold (≥3) and does NOT yield.

**Step 2:** Implement SQL-level CTE:

```sql
WITH pairs AS (
  SELECT unnest(entities) AS e, session_id FROM episodic.turns
   WHERE ingested_at >= now() - interval '24 hours'
)
SELECT a.e AS e1, b.e AS e2, count(DISTINCT a.session_id) AS support
  FROM pairs a JOIN pairs b ON a.session_id = b.session_id AND a.e < b.e
 GROUP BY 1,2 HAVING count(DISTINCT a.session_id) >= 3;
```

### Task 8.2: Mentat client + prompt

**Files:**
- Create: `ch-server/internal/pipeline_b/mentat.go`

**Step 1:** HTTP client to vLLM (OpenAI-compatible). Prompt template
accepts `{turns[]}` batch, returns JSON
`{concept, content, memory_type, entities}`. Strict JSON validation;
reject & log on parse error; never write malformed to semantic.

**Step 2:** Tests stubbing the LLM. Commit.

### Task 8.3: Dedup + upsert into semantic

**Files:**
- `ch-server/internal/pipeline_b/upsert.go` + test.

**Step 1:** Before insert: compute HNSW similarity against existing
`semantic.mnemes`. If cosine > 0.9, strengthen existing via Bayesian
update + append to `source_episodic_ids` + `contributor_user_ids`.
Else insert new row + enqueue co-activation / contradiction jobs.

### Task 8.4: Wire cron

**Files:**
- Modify: `ch-server/cmd/ch-server/main.go` — register a cron (prefer
  `robfig/cron/v3` or similar) firing at 02:00 local.

**Step 1:** Test: inject clock, assert job runs at 02:00. Commit.

### Gate 8

- Synthetic 24h episodic load (≥3 sessions referencing the same entity
  pair) triggers Pipeline B; exactly one semantic mneme inserted per
  pattern; `contributor_user_ids` reflects all contributors.

---

# Phase 9 — docker-compose local-dev stack

**Purpose:** `docker compose up` in `ghola` repo launches a complete
end-to-end stack in < 30s.

### Task 9.1: Compose file

**Files:**
- Create: `/home/loganb/ghola/deploy/docker-compose/docker-compose.yml`
- Services: `postgres` (with pg_ghola v2 image), `chapterhouse`,
  `melange` (local embedding), `mentat` (local vLLM, optional profile),
  `ghola` (the local service — usually run on host, compose profile
  `all`).
- Create: `/home/loganb/ghola/deploy/docker-compose/.env.example`
- Create: `/home/loganb/ghola/deploy/docker-compose/seed.sql` — empty
  DB, relies on chapterhouse's migration runner.

### Task 9.2: Startup script & smoke test

**Files:**
- Create: `/home/loganb/ghola/scripts/dev-up.sh` — one-liner bringing
  everything up + `curl`ing `/health` against every service.

**Step 1:**
```bash
cd /home/loganb/ghola/deploy/docker-compose
docker compose up -d
../../scripts/dev-up.sh
```

Expected: all services healthy in < 30s (Gate 7 of acceptance criteria).

### Gate 9

- `docker compose up && ./scripts/dev-up.sh` prints all-green.
- A pi-mono sample app + a Claude Code sample session both call
  record/recall successfully (Gate 1 of acceptance criteria).

---

# Phase 10 — Production deploy

**Purpose:** Fresh `CREATE EXTENSION pg_ghola v0.2.0` on an empty
production DB. New chapterhouse image. No data migration.

### Task 10.1: Drop-and-recreate prod DB

**⚠ Destructive. Confirm with user before executing.**

**Step 1:** Scale chapterhouse deploy to 0 (via git push to
`homelab-k3s/` — ArgoCD will sync):

```bash
cd ~/ai/homelab-k3s
# edit apps/chapterhouse/.../values.yaml: replicas: 0
git add -A && git commit -m "chore: ch-server scale 0 for v2 migration" && git push
```

**Step 2:** Wait for ArgoCD sync (check with `kubectl -n argocd get app chapterhouse -o json | jq '.status.sync.status'`).

**Step 3:** Drop old schema in production Postgres:

```bash
ssh nuc "kubectl -n ch-system exec -it memory-db-1 -- psql -U postgres -d memories \
  -c 'DROP EXTENSION IF EXISTS pg_recall CASCADE; DROP SCHEMA IF EXISTS ghola CASCADE; \
      DROP SCHEMA IF EXISTS episodic CASCADE; DROP SCHEMA IF EXISTS semantic CASCADE;'"
```

### Task 10.2: Ship new images

**Step 1:** Build + push `ghcr.io/logan-broit/pg-ghola:0.2.0`.

**Step 2:** Build + push `ghcr.io/logan-broit/chapterhouse:<sha>` from
the monorepo `main` (after Phase 3's chapterhouse migration is
complete).

**Step 3:** Update homelab-k3s manifests:
- Point CNPG to the new pg_ghola image.
- Bump chapterhouse image tag.
- Scale replicas back to 1.

**Step 4:** Git push → ArgoCD sync.

**Step 5:** Verify:
```bash
kubectl -n ch-system exec memory-db-1 -- psql -U postgres -d memories \
  -c "SELECT extname, extversion FROM pg_extension WHERE extname='pg_recall';"
```

Expected: `pg_recall | 0.2.0`.

**Step 6:** `curl https://chapterhouse.thesgc.internal/v1/semantic/list`
with a valid API key returns 200 + `[]`.

### Gate 10

- `kubectl -n ch-system get pods` all Running.
- `chapterhouse.thesgc.internal/v1/episodic/ingest` accepts a seed POST.
- Fresh install matches v1a success criterion 5.

---

# Phase 11 — Acceptance-criteria integration tests

**Purpose:** One integration test per v1a success criterion. Until all 7
pass, the release is not shippable.

### Task 11.1: E2E — two agents, same service (criterion 1)

**Files:**
- Create: `/home/loganb/ghola/test/e2e_two_agents_test.go`

**Step 1:** Start compose stack. Launch a pi-mono sample + an MCP
client. Both `record` one turn, then `recall` each other's turn. Assert
both see both turns.

### Task 11.2: Pipeline A 1-hour endurance (criterion 2)

**Files:**
- Create: `/home/loganb/ghola/test/endurance_pipeline_a_test.go`

**Step 1:** Synthetic load: 1 turn/sec for 60 min. Assert episodic row
count == 3600, watermark == 3600, zero errors in log.

### Task 11.3: Pipeline B end-to-end (criterion 3)

**Step 1:** Seed episodic with 3 sessions each containing
`(CNPG, Postgres)`. Trigger Pipeline B manually (not via cron). Assert
one new semantic mneme; verify cognitive primitives (Hebbian weight
update present) by querying `semantic.associations`.

### Task 11.4: Sharing cross-user (criterion 4)

**Step 1:** User A records; A shares session; User B's `recall` returns
A's turn. Assert `tier == "episodic"` and attribution preserved.

### Task 11.5: Fresh-install smoke (criterion 5)

**Step 1:** Re-run the smoke from Task 1.9 against a brand-new
Postgres container, this time via the compose stack (`docker compose up
--force-recreate postgres`). Assert 5 tables in `semantic.*`, 3 in
`episodic.*`.

### Task 11.6: No-Postgres-creds-on-device (criterion 6)

**Step 1:** On the laptop (not workstation), install only the
`ghola` binary + API key env var. Run the pi-mono sample. Assert:
- `env | grep -i POSTGRES` — returns empty (no such env var).
- Client still works (round-trip record/recall).

### Task 11.7: 30-second cold-start (criterion 7)

**Step 1:** `docker compose down -v && time docker compose up -d && wait_healthy`.
Assert wall-clock < 30s on a fresh clone with image cache populated.

### Task 11.8: Dimension-agnosticism (derived from CONSTRAINT: swappable_models_and_dimensions)

**Step 1:** Run the full E2E suite three times, each with a different
`EMBEDDING_DIM`:
- `EMBEDDING_DIM=384` (BGE-small) — uses a stub embedder returning
  deterministic 384-vectors.
- `EMBEDDING_DIM=1024` (Qwen3 reference, default).
- `EMBEDDING_DIM=1536` (Ada-002-shape).

**Step 2:** Assert all 7 prior acceptance tests pass at each dimension
without SQL or Go edits. If any pass only at 1024, the dim-agnostic
invariant is broken — fix the leak.

### Gate 11

All 8 tests green (criteria 1–7 plus dimension-agnosticism). Tag
`ghola@v0.1.0` + `pg_ghola@v0.2.0` + `chapterhouse@v2.0.0`.

---

## Repo commit map

For orientation during execution:

| Repo | Path | Phases | Notes |
|---|---|---|---|
| `logan-broit/ghola` | `/home/loganb/ghola/` | 0, 1–9, 11 | Monorepo. Work on `main` directly. Subdirs: `extension/` (Phase 1), `_chapterhouse/` (Phase 2, 3, 8 — migrates into root), `cmd/` + `internal/` + `clients/` + `deploy/` + `test/` (Phases 4–9, 11) |
| `logan-broit/homelab-k3s` | `/home/loganb/ai/homelab-k3s/` | 10 | Manifest updates for production deploy (ArgoCD auto-syncs) |

Archived mid-plan:
- `logan-broit/pg_ghola` — history preserved inside `ghola/extension/`
  via `git subtree add`. Repo archived on GitHub once the monorepo
  push landed.
- `logan-broit/chapterhouse` — history preserved inside
  `ghola/_chapterhouse/` via `git subtree add`. Left active on GitHub
  (not owned by this project).

## Risk register

- **ArgoCD auto-sync reverts manual scaling.** Addressed: always scale
  via git push, not `kubectl scale`.
- **CNPG extension name is `pg_recall`, not `pg_ghola`.** Addressed:
  Dockerfile symlinks `pg_ghola.so` → `pg_recall.so`; tests target
  `pg_recall` in prod smoke.
- **sqlite-vec loading.** Requires the extension `.so` to ship with the
  ghola binary. Bundle via `//go:embed` or a post-install script; test
  on a clean VM.
- **Pipeline B LLM cost spikes.** vLLM is local (NUC). If inference
  slows, Pipeline B misses its 02:00 window; acceptable — it catches up
  on the next run because scan is watermark-idempotent.
- **30-second cold start** depends on the embedding model being
  pre-pulled. First-ever run will exceed. Add a `make seed` target to
  pre-pull the image layer.

## References

- Design: `docs/plans/2026-04-19-greenfield-tiered-memory-design.md`
- Architecture artifact: `docs/plans/assets/GholaArchitecture.tsx`
- Prior design this supersedes:
  `docs/plans/2026-04-16-multi-granularity-encoding-design.md`
- Iter 16 invalidation: `.samsara/iterations/016.md`
