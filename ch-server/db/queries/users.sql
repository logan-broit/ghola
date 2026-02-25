-- name: GetUserByID :one
SELECT * FROM users
WHERE id = $1;

-- name: GetUserByUsername :one
SELECT * FROM users
WHERE username = $1;

-- name: CreateUser :one
INSERT INTO users (id, username, email, display_name, metadata)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: UpdateUser :one
UPDATE users
SET username = $2,
    email = $3,
    display_name = $4,
    metadata = $5
WHERE id = $1
RETURNING *;

-- name: DeleteUser :exec
DELETE FROM users
WHERE id = $1;

-- name: ListUsers :many
SELECT * FROM users
ORDER BY username
LIMIT $1 OFFSET $2;

-- name: GetOrCreateUser :one
INSERT INTO users (id, username, email, display_name)
VALUES ($1, $2, $3, $4)
ON CONFLICT (id) DO UPDATE
SET email = COALESCE(EXCLUDED.email, users.email),
    display_name = COALESCE(EXCLUDED.display_name, users.display_name)
RETURNING *;

-- ============================================================================
-- Admin-related queries
-- ============================================================================

-- name: GetUserByUsernameForAuth :one
SELECT * FROM users
WHERE username = $1 AND deactivated_at IS NULL;

-- name: SetUserPassword :exec
UPDATE users
SET password_hash = $2
WHERE id = $1;

-- name: SetUserAdmin :exec
UPDATE users
SET is_admin = $2
WHERE id = $1;

-- name: DeactivateUser :exec
UPDATE users
SET deactivated_at = NOW()
WHERE id = $1;

-- name: ReactivateUser :exec
UPDATE users
SET deactivated_at = NULL
WHERE id = $1;

-- name: ListUsersAdmin :many
SELECT * FROM users
WHERE ($3::boolean = FALSE OR deactivated_at IS NULL)
ORDER BY username
LIMIT $1 OFFSET $2;

-- name: CountUsers :one
SELECT
    COUNT(*) as total,
    COUNT(*) FILTER (WHERE deactivated_at IS NULL) as active,
    COUNT(*) FILTER (WHERE is_admin = TRUE AND deactivated_at IS NULL) as admins
FROM users;

-- name: GetAdminStats :one
SELECT
    (SELECT COUNT(*) FROM users WHERE deactivated_at IS NULL) as active_users,
    (SELECT COUNT(*) FROM users WHERE is_admin = TRUE AND deactivated_at IS NULL) as admin_users,
    (SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())) as active_api_keys,
    (SELECT COUNT(*) FROM admin_sessions WHERE revoked_at IS NULL AND expires_at > NOW()) as active_sessions;

-- name: CreateUserWithPassword :one
INSERT INTO users (id, username, email, display_name, password_hash, is_admin, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: UpdateUserAdmin :one
UPDATE users
SET username = $2,
    email = $3,
    display_name = $4,
    is_admin = $5,
    metadata = $6
WHERE id = $1
RETURNING *;
