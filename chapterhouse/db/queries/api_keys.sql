-- name: CreateAPIKey :one
INSERT INTO api_keys (user_id, key_hash, key_prefix, name, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetAPIKeyByHash :one
SELECT ak.*, u.username, u.email, u.is_admin, u.org_id, u.deactivated_at as user_deactivated_at
FROM api_keys ak
JOIN users u ON ak.user_id = u.id
WHERE ak.key_hash = $1
  AND ak.revoked_at IS NULL
  AND (ak.expires_at IS NULL OR ak.expires_at > NOW())
  AND u.deactivated_at IS NULL;

-- name: UpdateAPIKeyLastUsed :exec
UPDATE api_keys
SET last_used_at = NOW()
WHERE id = $1;

-- name: ListAPIKeysByUser :many
SELECT ak.*, u.username
FROM api_keys ak
JOIN users u ON ak.user_id = u.id
WHERE ak.user_id = $1
ORDER BY ak.created_at DESC;

-- name: ListAllAPIKeys :many
SELECT ak.*, u.username
FROM api_keys ak
JOIN users u ON ak.user_id = u.id
ORDER BY ak.created_at DESC
LIMIT $1 OFFSET $2;

-- name: GetAPIKeyByID :one
SELECT * FROM api_keys
WHERE id = $1;

-- name: RevokeAPIKey :exec
UPDATE api_keys
SET revoked_at = NOW()
WHERE id = $1;

-- name: RevokeAllAPIKeysByUser :exec
UPDATE api_keys
SET revoked_at = NOW()
WHERE user_id = $1 AND revoked_at IS NULL;

-- name: CountActiveAPIKeysByUser :one
SELECT COUNT(*) FROM api_keys
WHERE user_id = $1
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > NOW());
