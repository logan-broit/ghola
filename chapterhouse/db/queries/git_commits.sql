-- name: GetGitCommit :one
SELECT * FROM git_commits
WHERE guid = $1 AND user_id = $2;

-- name: GetGitCommitByHash :one
SELECT * FROM git_commits
WHERE commit_hash = $1 AND user_id = $2;

-- name: ListGitCommits :many
SELECT * FROM git_commits
WHERE user_id = $1
ORDER BY committed_at DESC
LIMIT $2 OFFSET $3;

-- name: ListGitCommitsByDateRange :many
SELECT * FROM git_commits
WHERE user_id = $1
  AND committed_at >= $2
  AND committed_at <= $3
ORDER BY committed_at DESC;

-- name: CreateGitCommit :one
INSERT INTO git_commits (
    user_id, commit_hash, commit_message, author_name, author_email, committed_at
) VALUES (
    $1, $2, $3, $4, $5, $6
)
RETURNING *;

-- name: GetLatestGitCommit :one
SELECT * FROM git_commits
WHERE user_id = $1
ORDER BY committed_at DESC
LIMIT 1;

-- name: CountGitCommits :one
SELECT COUNT(*)::int
FROM git_commits
WHERE user_id = $1;
