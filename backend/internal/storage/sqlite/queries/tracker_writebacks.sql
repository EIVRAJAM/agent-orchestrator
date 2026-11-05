-- name: EnqueueTrackerWriteback :execrows
INSERT INTO tracker_writebacks (
    id, project_id, session_id, issue_id, pr_url, status, attempts,
    comment_url, failure_code, created_at, updated_at, claimed_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (project_id, issue_id, pr_url) DO NOTHING;

-- name: ClaimTrackerWriteback :one
UPDATE tracker_writebacks
SET status = 'claimed', attempts = attempts + 1, claimed_at = ?, updated_at = ?
WHERE id = (
    SELECT id FROM tracker_writebacks
    WHERE status = 'pending'
    ORDER BY created_at, id
    LIMIT 1
)
AND status = 'pending'
RETURNING id, project_id, session_id, issue_id, pr_url, status, attempts,
    comment_url, failure_code, created_at, updated_at, claimed_at;

-- name: FinishTrackerWriteback :execrows
UPDATE tracker_writebacks
SET status = ?, comment_url = ?, failure_code = ?, updated_at = ?
WHERE id = ? AND status = 'claimed';

-- name: RecoverStaleTrackerWritebacks :execrows
UPDATE tracker_writebacks
SET status = 'uncertain', failure_code = 'restart', updated_at = ?
WHERE status = 'claimed';

-- name: ListTrackerWritebacks :many
SELECT id, project_id, session_id, issue_id, pr_url, status, attempts,
    comment_url, failure_code, created_at, updated_at, claimed_at
FROM tracker_writebacks
WHERE (sqlc.arg(status) = '' OR status = sqlc.arg(status))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);
