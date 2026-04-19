-- Migration: 002_admin_auth
-- Description: Add API key authentication and admin console support

-- Add admin-related columns to users table
ALTER TABLE users
ADD COLUMN password_hash VARCHAR(255),
ADD COLUMN is_admin BOOLEAN NOT NULL DEFAULT FALSE,
ADD COLUMN deactivated_at TIMESTAMPTZ;

-- Create index for active admin users
CREATE INDEX idx_users_active_admin ON users(is_admin)
    WHERE is_admin = TRUE AND deactivated_at IS NULL;

-- API keys table for tester authentication
CREATE TABLE api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key_hash VARCHAR(64) NOT NULL,      -- SHA-256 hash of the key
    key_prefix VARCHAR(8) NOT NULL,      -- First 8 chars for identification
    name VARCHAR(255) NOT NULL,          -- Human-readable name for the key
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);

CREATE INDEX idx_api_keys_user ON api_keys(user_id);
CREATE INDEX idx_api_keys_hash ON api_keys(key_hash);
CREATE INDEX idx_api_keys_prefix ON api_keys(key_prefix);
CREATE INDEX idx_api_keys_active ON api_keys(user_id)
    WHERE revoked_at IS NULL;

-- Admin sessions table for web console authentication
CREATE TABLE admin_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash VARCHAR(64) NOT NULL,     -- SHA-256 hash of session token
    ip_address TEXT,                      -- Changed from INET to TEXT for Go compatibility
    user_agent TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ
);

CREATE INDEX idx_admin_sessions_user ON admin_sessions(user_id);
CREATE INDEX idx_admin_sessions_token ON admin_sessions(token_hash);
CREATE INDEX idx_admin_sessions_active ON admin_sessions(user_id)
    WHERE revoked_at IS NULL;
