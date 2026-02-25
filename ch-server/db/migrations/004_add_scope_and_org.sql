-- Migration: 004_add_scope_and_org
-- Description: Add organization support and memory scope (personal/org)

-- Add org_id to users table
-- Use a default organization UUID for all existing users
ALTER TABLE users
ADD COLUMN org_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001';

-- Create index for org-based queries
CREATE INDEX idx_users_org ON users(org_id);

-- Add scope column to memory_blocks
ALTER TABLE memory_blocks
ADD COLUMN scope VARCHAR(20) NOT NULL DEFAULT 'personal'
CHECK (scope IN ('personal', 'org'));

-- Create index for scope-based queries
CREATE INDEX idx_memory_blocks_scope ON memory_blocks(scope);

-- Create composite index for org + scope queries
CREATE INDEX idx_memory_blocks_org_scope ON memory_blocks(user_id, scope);

-- Update the current_memory_blocks view to include scope
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
    scope,
    expires_at,
    created_at,
    modified_at
FROM memory_blocks
WHERE expires_at IS NULL OR expires_at > NOW()
ORDER BY user_id, name, version DESC;

-- Comment on columns for documentation
COMMENT ON COLUMN users.org_id IS 'Organization identifier - all users with same org_id can share org-scoped memories';
COMMENT ON COLUMN memory_blocks.scope IS 'Memory visibility scope: personal (only creator sees it) or org (all users in organization see it)';
