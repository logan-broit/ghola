-- Migration: 008_add_session_id
-- Description: Add first-class session_id column for episodic context.
--              Links memories to the MCP transport session that created them.

-- ============================================================
-- Part 1: Add session_id column
-- ============================================================

ALTER TABLE memory_blocks ADD COLUMN session_id UUID;

-- Partial index for session-scoped queries
CREATE INDEX idx_memory_blocks_session_id
    ON memory_blocks (session_id)
    WHERE session_id IS NOT NULL AND is_current = true;

-- ============================================================
-- Part 2: Recreate view to include session_id
-- ============================================================

DROP VIEW IF EXISTS current_memory_blocks;
CREATE VIEW current_memory_blocks AS
SELECT
    id,
    guid,
    user_id,
    name,
    tier,
    value,
    tags,
    version,
    sort_order,
    memory_type,
    scope,
    session_id,
    recall_count,
    last_recalled_at,
    expires_at,
    created_at,
    modified_at
FROM memory_blocks
WHERE is_current = true
  AND (expires_at IS NULL OR expires_at > NOW());
