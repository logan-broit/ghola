-- name: GetCurrentMemoryBlocks :many
SELECT * FROM current_memory_blocks
WHERE user_id = $1
ORDER BY tier, sort_order, name;

-- name: GetCurrentMemoryBlocksByTier :many
SELECT * FROM current_memory_blocks
WHERE user_id = $1 AND tier = $2
ORDER BY sort_order, name;

-- name: GetCurrentMemoryBlockByName :one
SELECT * FROM current_memory_blocks
WHERE user_id = $1 AND name = $2;

-- name: GetMemoryBlockByGUID :one
SELECT * FROM memory_blocks
WHERE guid = $1 AND user_id = $2;

-- name: GetMemoryBlockByID :one
SELECT * FROM memory_blocks
WHERE id = $1 AND user_id = $2;

-- name: GetMemoryBlockHistory :many
SELECT * FROM memory_blocks
WHERE user_id = $1 AND name = $2
ORDER BY version DESC
LIMIT $3 OFFSET $4;

-- name: GetNextMemoryBlockVersion :one
SELECT COALESCE(MAX(version), 0) + 1 AS next_version
FROM memory_blocks
WHERE user_id = $1 AND name = $2;

-- name: CreateMemoryBlock :one
INSERT INTO memory_blocks (
    user_id, name, tier, value, version, sort_order, memory_type, scope, expires_at, tags
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
RETURNING *;

-- name: UpdateMemoryBlock :one
-- Updates by creating a new version (version must be provided)
INSERT INTO memory_blocks (
    user_id, name, tier, value, version, sort_order, memory_type, scope, expires_at, tags
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
RETURNING *;

-- name: DeleteMemoryBlock :exec
-- Soft delete by marking all versions (or implement actual delete)
DELETE FROM memory_blocks
WHERE user_id = $1 AND name = $2;

-- name: CountMemoryBlocksByUser :one
SELECT COUNT(DISTINCT name)::int
FROM memory_blocks
WHERE user_id = $1;

-- name: CountMemoryBlockVersions :one
SELECT COUNT(*)::int
FROM memory_blocks
WHERE user_id = $1 AND name = $2;

-- name: GetMemoryContext :many
-- Get all current memory blocks for context loading
SELECT * FROM current_memory_blocks
WHERE user_id = $1
ORDER BY
    CASE tier
        WHEN 'core' THEN 1
        WHEN 'index' THEN 2
        WHEN 'state' THEN 3
    END,
    sort_order,
    name;

-- name: GetMemoryStats :one
-- Get aggregate memory statistics across all users
SELECT
    COUNT(DISTINCT user_id)::bigint AS users_with_memories,
    COUNT(*)::bigint AS total_memory_blocks,
    COALESCE(SUM(LENGTH(value)), 0)::bigint AS total_content_bytes,
    COUNT(DISTINCT name)::bigint AS unique_memory_names
FROM current_memory_blocks;

-- name: GetAllCurrentMemoryBlocks :many
-- Get all current memory blocks for reindexing
SELECT * FROM current_memory_blocks
ORDER BY id;

-- name: GetAllCurrentMemoryBlocksWithOrg :many
-- Get all current memory blocks with org_id for reindexing
SELECT cmb.*, u.org_id
FROM current_memory_blocks cmb
JOIN users u ON cmb.user_id = u.id
ORDER BY cmb.id;

-- name: GetCurrentMemoryBlocksByType :many
SELECT * FROM current_memory_blocks
WHERE user_id = $1 AND memory_type = $2
ORDER BY tier, sort_order, name;

-- name: UpdateMemoryBlockType :one
-- Update the memory type of the latest version of a memory block
UPDATE memory_blocks
SET memory_type = $3,
    expires_at = $4,
    modified_at = NOW()
WHERE id = (
    SELECT cmb.id FROM current_memory_blocks cmb
    WHERE cmb.user_id = $1 AND cmb.name = $2
)
RETURNING *;

-- name: DeleteExpiredMemories :exec
-- Delete memory blocks that have expired
DELETE FROM memory_blocks
WHERE expires_at IS NOT NULL AND expires_at <= NOW();

-- name: PruneOldVersions :execresult
-- Delete old versions of memory blocks, keeping the most recent N versions per (user_id, name).
-- The is_current row is always preserved regardless of the retention limit.
DELETE FROM memory_blocks
WHERE id IN (
    SELECT id FROM (
        SELECT id,
            ROW_NUMBER() OVER (PARTITION BY user_id, name ORDER BY version DESC) AS rn
        FROM memory_blocks
    ) ranked
    WHERE rn > $1 AND id NOT IN (
        SELECT id FROM memory_blocks WHERE is_current = true
    )
);

-- name: GetMemoryTypeDistribution :many
-- Get count of memories by type for visualization
SELECT
    memory_type,
    COUNT(*)::bigint AS count
FROM current_memory_blocks
WHERE memory_type IS NOT NULL
GROUP BY memory_type
ORDER BY count DESC;

-- name: GetMemoryScopeDistribution :many
-- Get count of memories by scope for visualization
SELECT
    scope,
    COUNT(*)::bigint AS count
FROM current_memory_blocks
WHERE scope IS NOT NULL
GROUP BY scope
ORDER BY count DESC;

-- name: GetTopTags :many
-- Get most frequently used tags across all memories
SELECT
    tag,
    COUNT(*)::bigint AS count
FROM (
    SELECT DISTINCT
        user_id,
        name,
        unnest(tags) AS tag
    FROM current_memory_blocks
    WHERE array_length(tags, 1) > 0
) AS tags_extracted
WHERE tag != ''
GROUP BY tag
ORDER BY count DESC
LIMIT $1;

-- name: GetAccessibleMemoryBlocks :many
-- Get memory blocks accessible to a user (personal + org-scoped)
-- Requires joining with users table to get org_id
SELECT cmb.* FROM current_memory_blocks cmb
JOIN users u ON cmb.user_id = u.id
WHERE (cmb.user_id = $1 AND cmb.scope = 'personal')
   OR (u.org_id = (SELECT org_id FROM users WHERE id = $1) AND cmb.scope = 'org')
ORDER BY cmb.tier, cmb.sort_order, cmb.name;

-- name: GetAccessibleMemoryBlocksByType :many
-- Get accessible memory blocks filtered by type
SELECT cmb.* FROM current_memory_blocks cmb
JOIN users u ON cmb.user_id = u.id
WHERE memory_type = $2
  AND ((cmb.user_id = $1 AND cmb.scope = 'personal')
   OR (u.org_id = (SELECT org_id FROM users WHERE id = $1) AND cmb.scope = 'org'))
ORDER BY cmb.tier, cmb.sort_order, cmb.name;

-- name: UpdateMemoryBlockScope :one
-- Update the scope of a memory block
UPDATE memory_blocks
SET scope = $3,
    modified_at = NOW()
WHERE id = (
    SELECT cmb.id FROM current_memory_blocks cmb
    WHERE cmb.user_id = $1 AND cmb.name = $2
)
RETURNING *;

-- name: ExportMemories :many
-- Export memories in JSONL-ready format with filtering
SELECT
    cmb.id,
    cmb.guid,
    cmb.user_id,
    u.org_id,
    cmb.name,
    cmb.tier,
    cmb.value,
    cmb.tags,
    cmb.memory_type,
    cmb.scope,
    cmb.created_at,
    cmb.modified_at,
    cmb.expires_at
FROM current_memory_blocks cmb
JOIN users u ON cmb.user_id = u.id
WHERE (cmb.user_id = $1 AND cmb.scope = 'personal')
   OR (u.org_id = (SELECT org_id FROM users WHERE id = $1) AND cmb.scope = 'org')
ORDER BY cmb.created_at DESC;

-- name: SearchAccessibleMemoryBlocks :many
-- Search accessible memory blocks by keyword using ILIKE (name) and full-text search (value).
SELECT cmb.* FROM current_memory_blocks cmb
JOIN users u ON cmb.user_id = u.id
WHERE ((cmb.user_id = @user_id AND cmb.scope = 'personal')
   OR (u.org_id = (SELECT org_id FROM users WHERE id = @user_id) AND cmb.scope = 'org'))
  AND (
    cmb.name ILIKE '%' || @query || '%'
    OR to_tsvector('english', COALESCE(cmb.value, '')) @@ plainto_tsquery('english', @query)
  )
ORDER BY
    CASE WHEN cmb.name ILIKE '%' || @query || '%' THEN 0 ELSE 1 END,
    cmb.tier, cmb.sort_order, cmb.name
LIMIT @search_limit;

-- name: SearchAccessibleMemoryBlocksByType :many
-- Search accessible memory blocks by keyword filtered by memory type.
SELECT cmb.* FROM current_memory_blocks cmb
JOIN users u ON cmb.user_id = u.id
WHERE memory_type = @memory_type
  AND ((cmb.user_id = @user_id AND cmb.scope = 'personal')
   OR (u.org_id = (SELECT org_id FROM users WHERE id = @user_id) AND cmb.scope = 'org'))
  AND (
    cmb.name ILIKE '%' || @query || '%'
    OR to_tsvector('english', COALESCE(cmb.value, '')) @@ plainto_tsquery('english', @query)
  )
ORDER BY
    CASE WHEN cmb.name ILIKE '%' || @query || '%' THEN 0 ELSE 1 END,
    cmb.tier, cmb.sort_order, cmb.name
LIMIT @search_limit;
