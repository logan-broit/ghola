-- Migration: 005_is_current_and_search
-- Description: Replace DISTINCT ON view with is_current column + trigger for O(1) reads,
--              and add GIN indexes for server-side keyword search.

-- ============================================================
-- Part 1: is_current column (replaces DISTINCT ON sort)
-- ============================================================

-- Add the is_current column
ALTER TABLE memory_blocks
ADD COLUMN is_current BOOLEAN NOT NULL DEFAULT false;

-- Backfill: mark the latest non-expired version of each user+name as current
UPDATE memory_blocks SET is_current = true
WHERE id IN (
    SELECT DISTINCT ON (user_id, name) id
    FROM memory_blocks
    WHERE expires_at IS NULL OR expires_at > NOW()
    ORDER BY user_id, name, version DESC
);

-- Partial index for fast current-block lookups
CREATE INDEX idx_memory_blocks_is_current
    ON memory_blocks(user_id, name)
    WHERE is_current = true;

-- Trigger function: on INSERT, set new row as current and unset previous
CREATE OR REPLACE FUNCTION set_memory_block_current()
RETURNS TRIGGER AS $$
BEGIN
    -- Unset is_current for previous versions of this user+name
    UPDATE memory_blocks
    SET is_current = false
    WHERE user_id = NEW.user_id
      AND name = NEW.name
      AND is_current = true
      AND id != NEW.id;

    -- Mark the new row as current
    NEW.is_current := true;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER memory_blocks_set_current
    BEFORE INSERT ON memory_blocks
    FOR EACH ROW
    EXECUTE FUNCTION set_memory_block_current();

-- Recreate view using is_current (no more DISTINCT ON + full table sort)
DROP VIEW IF EXISTS current_memory_blocks;
CREATE VIEW current_memory_blocks AS
SELECT
    id,
    guid,
    user_id,
    name,
    tier,
    value,
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

-- ============================================================
-- Part 2: GIN indexes for keyword search
-- ============================================================

-- Trigram index on name for ILIKE substring matching (pg_trgm already enabled)
CREATE INDEX idx_memory_blocks_name_trgm
    ON memory_blocks USING GIN (name gin_trgm_ops);

-- Full-text search index on value for word-based matching
CREATE INDEX idx_memory_blocks_value_fts
    ON memory_blocks USING GIN (to_tsvector('english', COALESCE(value, '')));
