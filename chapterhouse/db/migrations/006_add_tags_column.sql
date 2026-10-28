-- Migration: 006_add_tags_column
-- Description: Add a proper tags column to memory_blocks, replacing the
--              [tag1,tag2] prefix convention in the value field.

-- ============================================================
-- Part 1: Add tags column
-- ============================================================

ALTER TABLE memory_blocks
ADD COLUMN tags TEXT[] NOT NULL DEFAULT '{}';

-- GIN index for array containment queries (@>, &&)
CREATE INDEX idx_memory_blocks_tags ON memory_blocks USING GIN (tags);

-- ============================================================
-- Part 2: Migrate existing tags from value prefix
-- ============================================================

-- Extract [tag1,tag2,...] prefix into the tags array and strip it from value
UPDATE memory_blocks
SET
    tags = ARRAY(
        SELECT LOWER(TRIM(t))
        FROM unnest(string_to_array(
            substring(value FROM '^\[([^\]]*)\]'),
            ','
        )) AS t
        WHERE TRIM(t) != ''
    ),
    value = TRIM(LEADING FROM substring(value FROM '^\[[^\]]*\]\s*(.*)$'))
WHERE value ~ '^\[';

-- ============================================================
-- Part 3: Recreate view to include tags
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
    expires_at,
    created_at,
    modified_at
FROM memory_blocks
WHERE is_current = true
  AND (expires_at IS NULL OR expires_at > NOW());
