-- name: CreateAdminSession :one
INSERT INTO admin_sessions (user_id, token_hash, ip_address, user_agent, expires_at)
VALUES (@user_id, @token_hash, @ip_address, @user_agent, @expires_at)
RETURNING id, user_id, token_hash, ip_address, user_agent, created_at, expires_at, revoked_at;

-- name: GetAdminSessionByToken :one
-- Note: Despite the name, this query is used for all user sessions (admin and non-admin)
SELECT s.id, s.user_id, s.token_hash, s.ip_address, s.user_agent, s.created_at, s.expires_at, s.revoked_at,
       u.username, u.email, u.is_admin, u.org_id, u.deactivated_at as user_deactivated_at
FROM admin_sessions s
JOIN users u ON s.user_id = u.id
WHERE s.token_hash = $1
  AND s.revoked_at IS NULL
  AND s.expires_at > NOW()
  AND u.deactivated_at IS NULL;

-- name: RevokeAdminSession :exec
UPDATE admin_sessions
SET revoked_at = NOW()
WHERE id = $1;

-- name: RevokeAdminSessionByToken :exec
UPDATE admin_sessions
SET revoked_at = NOW()
WHERE token_hash = $1;

-- name: RevokeAllAdminSessionsByUser :exec
UPDATE admin_sessions
SET revoked_at = NOW()
WHERE user_id = $1 AND revoked_at IS NULL;

-- name: ListActiveAdminSessions :many
SELECT s.id, s.user_id, s.token_hash, s.ip_address, s.user_agent, s.created_at, s.expires_at, s.revoked_at,
       u.username
FROM admin_sessions s
JOIN users u ON s.user_id = u.id
WHERE s.revoked_at IS NULL
  AND s.expires_at > NOW()
ORDER BY s.created_at DESC;

-- name: CountActiveAdminSessions :one
SELECT COUNT(*) FROM admin_sessions
WHERE revoked_at IS NULL AND expires_at > NOW();

-- name: CleanupExpiredAdminSessions :exec
DELETE FROM admin_sessions
WHERE expires_at < NOW() - INTERVAL '7 days';
