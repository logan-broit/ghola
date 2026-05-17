package mneme

// Raw SQL for pg_ghola operations.
// pg_ghola types (mnemes, recall_result, score_weights) live in the pg_ghola schema
// and are not managed by sqlc, so we use raw queries here.

const insertMneme = `
INSERT INTO ghola.mnemes (
	workspace_id, concept, content, embedding,
	memory_type, scope, tier, tags, session_id, expires_at
) VALUES ($1, $2, $3, $4::vector, $5, $6, $7, $8, $9, $10)
RETURNING id, workspace_id, concept, content, confidence, access_count,
          last_access, created_at, state, memory_type, scope, tier, tags,
          session_id, expires_at
`

const findByConcept = `
SELECT id, workspace_id, concept, content, confidence, access_count,
       last_access, created_at, state, memory_type, scope, tier, tags,
       session_id, expires_at
FROM ghola.mnemes
WHERE workspace_id = $1 AND concept = $2 AND state = 'active'
ORDER BY created_at DESC
LIMIT 1
`

const nearDuplicateCheck = `
SELECT id, concept, content, 1 - (embedding <=> $1::vector) AS similarity
FROM ghola.mnemes
WHERE workspace_id = $2 AND state = 'active' AND id != $3
ORDER BY embedding <=> $1::vector
LIMIT 3
`

const markSupersedes = `SELECT ghola.mark_supersedes($1, $2)`

const recallQuery = `
SELECT (r).mneme_id, (r).score, (r).content_match, (r).activation,
       (r).hebbian_boost, (r).confidence, (r).concept, (r).content
FROM ghola.recall($1, $2, $3::vector, $4, $5, $6::ghola.score_weights, $7, $8, $9, $10) AS r
`

const confirmRecall = `SELECT ghola.confirm_recall($1::uuid[])`

const deleteMneme = `
DELETE FROM ghola.mnemes
WHERE id = $1 AND workspace_id = $2
RETURNING id
`

const getMnemeByID = `
SELECT id, workspace_id, concept, content, confidence, access_count,
       last_access, created_at, state, memory_type, scope, tier, tags,
       session_id, expires_at
FROM ghola.mnemes
WHERE id = $1 AND state = 'active'
`

const updateScope = `
UPDATE ghola.mnemes
SET scope = $1, workspace_id = $2
WHERE id = $3 AND workspace_id = $4 AND state = 'active'
RETURNING id, workspace_id, concept, content, confidence, access_count,
          last_access, created_at, state, memory_type, scope, tier, tags,
          session_id, expires_at
`

const listMnemes = `
SELECT id, workspace_id, concept, content, confidence, access_count,
       last_access, created_at, state, memory_type, scope, tier, tags,
       session_id, expires_at
FROM ghola.mnemes
WHERE workspace_id = ANY($1) AND state = 'active'
  AND ($2::text IS NULL OR memory_type = $2)
  AND ($3::text[] IS NULL OR tags @> $3)
  AND ($4::uuid IS NULL OR session_id = $4)
ORDER BY created_at DESC
LIMIT $5
`

const exportMnemes = `
SELECT id, workspace_id, concept, content, confidence, access_count,
       last_access, created_at, state, memory_type, scope, tier, tags,
       session_id, expires_at
FROM ghola.mnemes
WHERE workspace_id = ANY($1) AND state = 'active'
ORDER BY created_at DESC
`

const listSessions = `
SELECT session_id,
       COUNT(*) AS memory_count,
       MIN(created_at) AS first_activity,
       MAX(created_at) AS last_activity
FROM ghola.mnemes
WHERE workspace_id = ANY($1) AND session_id IS NOT NULL AND state = 'active'
GROUP BY session_id
ORDER BY MAX(created_at) DESC
LIMIT $2
`

const weightedConfirmSingle = `UPDATE ghola.mnemes SET confidence = ghola.bayesian_update(confidence, $2) WHERE id = $1`

const feedbackUpdate = `UPDATE ghola.mnemes SET confidence = ghola.bayesian_update(confidence, $2) WHERE id = $1 AND workspace_id = ANY($3) RETURNING confidence`

const getSessionMemories = `
SELECT id, workspace_id, concept, content, confidence, access_count,
       last_access, created_at, state, memory_type, scope, tier, tags,
       session_id, expires_at
FROM ghola.mnemes
WHERE workspace_id = ANY($1) AND session_id = $2 AND state = 'active'
ORDER BY created_at ASC
`

// insertSubMneme writes one per-turn sub-record linked to a parent mneme.
// The search_vector column is GENERATED ALWAYS from content so we do not
// insert it. token_start / token_end hold CHARACTER offsets into the
// parent session_text (not token offsets -- the column names are preserved
// from Phase 1 schema to avoid a migration; see
// docs/plans/2026-04-16-multi-granularity-encoding-implementation.md).
const insertSubMneme = `
INSERT INTO ghola.sub_mnemes (
    mneme_id, position, role, content, embedding, token_start, token_end
) VALUES ($1, $2, $3, $4, $5::vector, $6, $7)
`
