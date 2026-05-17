-- name: GetJournalEntry :one
SELECT * FROM journal
WHERE guid = $1 AND user_id = $2;

-- name: GetJournalEntryByID :one
SELECT * FROM journal
WHERE id = $1 AND user_id = $2;

-- name: ListJournalEntries :many
SELECT * FROM journal
WHERE user_id = $1
  AND ($2::varchar IS NULL OR entry_type = $2)
  AND superseded_by IS NULL
ORDER BY created_at DESC
LIMIT $3 OFFSET $4;

-- name: ListJournalEntriesByDateRange :many
SELECT * FROM journal
WHERE user_id = $1
  AND created_at >= $2
  AND created_at <= $3
  AND ($4::varchar IS NULL OR entry_type = $4)
  AND superseded_by IS NULL
ORDER BY created_at DESC
LIMIT $5 OFFSET $6;

-- name: CreateJournalEntry :one
INSERT INTO journal (
    user_id, entry_type, content, metadata, vector_id
) VALUES (
    $1, $2, $3, $4, $5
)
RETURNING *;

-- name: UpdateJournalEntry :one
UPDATE journal
SET content = $3,
    metadata = $4
WHERE guid = $1 AND user_id = $2
RETURNING *;

-- name: SupersedeJournalEntry :exec
UPDATE journal
SET superseded_by = $3
WHERE guid = $1 AND user_id = $2;

-- name: SetJournalVectorID :exec
UPDATE journal
SET vector_id = $3
WHERE guid = $1 AND user_id = $2;

-- name: DeleteJournalEntry :exec
DELETE FROM journal
WHERE guid = $1 AND user_id = $2;

-- name: SearchJournalFullText :many
SELECT *,
    ts_rank(to_tsvector('english', content), plainto_tsquery('english', $2)) as rank
FROM journal
WHERE user_id = $1
  AND to_tsvector('english', content) @@ plainto_tsquery('english', $2)
  AND superseded_by IS NULL
ORDER BY rank DESC, created_at DESC
LIMIT $3;

-- name: GetJournalEntriesByType :many
SELECT * FROM journal
WHERE user_id = $1 AND entry_type = $2
  AND superseded_by IS NULL
ORDER BY created_at DESC
LIMIT $3 OFFSET $4;

-- name: CountJournalEntriesByType :one
SELECT COUNT(*)::int
FROM journal
WHERE user_id = $1 AND entry_type = $2
  AND superseded_by IS NULL;

-- name: GetRecentDecisions :many
SELECT * FROM journal
WHERE user_id = $1
  AND entry_type = 'decision'
  AND superseded_by IS NULL
ORDER BY created_at DESC
LIMIT $2;

-- name: GetRecentSolutions :many
SELECT * FROM journal
WHERE user_id = $1
  AND entry_type = 'solution'
  AND superseded_by IS NULL
ORDER BY created_at DESC
LIMIT $2;

-- name: GetJournalEntriesByVectorIDs :many
SELECT * FROM journal
WHERE user_id = $1
  AND vector_id = ANY($2::uuid[])
ORDER BY created_at DESC;
