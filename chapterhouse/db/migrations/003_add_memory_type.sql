-- Migration: 003_add_memory_type
-- Description: Add memory type taxonomy (factual, experiential, working)

-- Add memory_type column with default value and constraint
ALTER TABLE memory_blocks
ADD COLUMN memory_type VARCHAR(50) NOT NULL DEFAULT 'factual'
CHECK (memory_type IN ('factual', 'experiential', 'working'));

-- Add expiration support for working memories
ALTER TABLE memory_blocks
ADD COLUMN expires_at TIMESTAMPTZ;

-- Create index for type-based queries
CREATE INDEX idx_memory_blocks_type ON memory_blocks(memory_type);

-- Create composite index for type + user filtering
CREATE INDEX idx_memory_blocks_user_type ON memory_blocks(user_id, memory_type);

-- Create index for expired memory cleanup
CREATE INDEX idx_memory_blocks_expires ON memory_blocks(expires_at) WHERE expires_at IS NOT NULL;

-- Update the current_memory_blocks view to include new columns
DROP VIEW IF EXISTS current_memory_blocks;
CREATE VIEW current_memory_blocks AS
SELECT DISTINCT ON (user_id, name)
    id,
    guid,
    user_id,
    name,
    tier,
    value,
    version,
    sort_order,
    memory_type,
    expires_at,
    created_at,
    modified_at
FROM memory_blocks
WHERE expires_at IS NULL OR expires_at > NOW()
ORDER BY user_id, name, version DESC;

-- Comment on columns for documentation
COMMENT ON COLUMN memory_blocks.memory_type IS 'Type of memory: factual (standards, policies), experiential (solutions, lessons learned), working (session context)';
COMMENT ON COLUMN memory_blocks.expires_at IS 'Optional expiration timestamp for working memories (auto-set to 7 days for working type)';
