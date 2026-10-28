-- Migration: 001_initial_schema
-- Description: Initial schema for CNAM agentic memory system

-- Enable required extensions
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Users table
CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username VARCHAR(255) UNIQUE NOT NULL,
    email VARCHAR(255),
    display_name VARCHAR(255),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    modified_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    metadata JSONB DEFAULT '{}'::jsonb
);

CREATE INDEX idx_users_username ON users(username);
CREATE INDEX idx_users_email ON users(email) WHERE email IS NOT NULL;

-- Insert default user for single-user mode
INSERT INTO users (id, username, email, display_name)
VALUES (
    '00000000-0000-0000-0000-000000000000',
    'default',
    'user@localhost',
    'Default User'
);

-- Memory blocks table with versioning
CREATE TABLE memory_blocks (
    id BIGSERIAL PRIMARY KEY,
    guid UUID UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    tier VARCHAR(10) NOT NULL CHECK (tier IN ('core', 'index', 'state')),
    value TEXT,
    version INTEGER NOT NULL DEFAULT 1,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    modified_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT unique_user_memory_version UNIQUE (user_id, name, version)
);

CREATE INDEX idx_memory_blocks_user_tier ON memory_blocks(user_id, tier);
CREATE INDEX idx_memory_blocks_user_name ON memory_blocks(user_id, name);
CREATE INDEX idx_memory_blocks_created ON memory_blocks(created_at DESC);
CREATE INDEX idx_memory_blocks_user_name_version
    ON memory_blocks(user_id, name, version DESC);

-- View for current (latest version) memory blocks
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
    created_at,
    modified_at
FROM memory_blocks
ORDER BY user_id, name, version DESC;

-- Journal table for timestamped entries
CREATE TABLE journal (
    id BIGSERIAL PRIMARY KEY,
    guid UUID UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    entry_type VARCHAR(50) NOT NULL CHECK (
        entry_type IN ('conversation', 'insight', 'event', 'task',
                       'reflection', 'decision', 'solution')
    ),
    content TEXT NOT NULL,
    metadata JSONB DEFAULT '{}'::jsonb,
    vector_id UUID,
    superseded_by UUID REFERENCES journal(guid),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    modified_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_journal_user_type ON journal(user_id, entry_type);
CREATE INDEX idx_journal_user_created ON journal(user_id, created_at DESC);
CREATE INDEX idx_journal_vector_id ON journal(vector_id) WHERE vector_id IS NOT NULL;
CREATE INDEX idx_journal_metadata ON journal USING GIN (metadata);
CREATE INDEX idx_journal_content_fts
    ON journal USING GIN (to_tsvector('english', content));

-- Git commits table for provenance tracking
CREATE TABLE git_commits (
    id BIGSERIAL PRIMARY KEY,
    guid UUID UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    commit_hash VARCHAR(40) NOT NULL,
    commit_message TEXT NOT NULL,
    author_name VARCHAR(255),
    author_email VARCHAR(255),
    committed_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT unique_user_commit UNIQUE (user_id, commit_hash)
);

CREATE INDEX idx_git_commits_user ON git_commits(user_id);
CREATE INDEX idx_git_commits_hash ON git_commits(commit_hash);
CREATE INDEX idx_git_commits_date ON git_commits(committed_at DESC);

-- Audit log table
CREATE TABLE audit_log (
    id BIGSERIAL PRIMARY KEY,
    guid UUID UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    user_id UUID REFERENCES users(id),
    action VARCHAR(50) NOT NULL,
    resource_type VARCHAR(50) NOT NULL,
    resource_id VARCHAR(255),
    details JSONB DEFAULT '{}'::jsonb,
    ip_address TEXT,                      -- Changed from INET to TEXT for Go compatibility
    user_agent TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_audit_log_user ON audit_log(user_id) WHERE user_id IS NOT NULL;
CREATE INDEX idx_audit_log_action ON audit_log(action);
CREATE INDEX idx_audit_log_created ON audit_log(created_at DESC);
CREATE INDEX idx_audit_log_resource ON audit_log(resource_type, resource_id);
CREATE INDEX idx_audit_log_details ON audit_log USING GIN (details);

-- Function to update modified_at timestamp
CREATE OR REPLACE FUNCTION update_modified_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.modified_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Triggers for auto-updating modified_at
CREATE TRIGGER users_modified_at
    BEFORE UPDATE ON users
    FOR EACH ROW
    EXECUTE FUNCTION update_modified_at();

CREATE TRIGGER memory_blocks_modified_at
    BEFORE UPDATE ON memory_blocks
    FOR EACH ROW
    EXECUTE FUNCTION update_modified_at();

CREATE TRIGGER journal_modified_at
    BEFORE UPDATE ON journal
    FOR EACH ROW
    EXECUTE FUNCTION update_modified_at();
