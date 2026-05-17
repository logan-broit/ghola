-- name: CreateAuditLog :one
INSERT INTO audit_log (
    user_id, action, resource_type, resource_id, details, ip_address, user_agent
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
RETURNING *;

-- name: ListAuditLogs :many
SELECT * FROM audit_log
WHERE ($1::uuid IS NULL OR $1 = '00000000-0000-0000-0000-000000000000'::uuid OR user_id = $1)
  AND ($2::varchar IS NULL OR $2 = '' OR action = $2)
  AND ($3::varchar IS NULL OR $3 = '' OR resource_type = $3)
ORDER BY created_at DESC
LIMIT $4 OFFSET $5;

-- name: CountAuditLogs :one
SELECT COUNT(*) FROM audit_log
WHERE ($1::uuid IS NULL OR $1 = '00000000-0000-0000-0000-000000000000'::uuid OR user_id = $1)
  AND ($2::varchar IS NULL OR $2 = '' OR action = $2)
  AND ($3::varchar IS NULL OR $3 = '' OR resource_type = $3);

-- name: ListAuditLogsByDateRange :many
SELECT * FROM audit_log
WHERE ($1::uuid IS NULL OR user_id = $1)
  AND created_at >= $2
  AND created_at <= $3
ORDER BY created_at DESC
LIMIT $4 OFFSET $5;

-- name: GetAuditLogsByResource :many
SELECT * FROM audit_log
WHERE resource_type = $1 AND resource_id = $2
ORDER BY created_at DESC
LIMIT $3;
