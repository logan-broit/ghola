-- Migration: 007_recall_tracking
-- Description: Add recall tracking columns for importance scoring.
--              Tracks how often and when each memory was last recalled.

-- ============================================================
-- Part 1: Add recall tracking columns
-- ============================================================

ALTER TABLE memory_blocks ADD COLUMN recall_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE memory_blocks ADD COLUMN last_recalled_at TIMESTAMPTZ;

-- Index for identifying frequently/infrequently recalled memories
CREATE INDEX idx_memory_blocks_recall_count ON memory_blocks (recall_count DESC)
    WHERE is_current = true;

-- ============================================================
-- Part 2: Recreate view to include new columns
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
    recall_count,
    last_recalled_at,
    expires_at,
    created_at,
    modified_at
FROM memory_blocks
WHERE is_current = true
  AND (expires_at IS NULL OR expires_at > NOW());
