-- name: RegisterSessionBranch :execrows
-- Register one exact branch/worktree relation for a session instance. The
-- subquery binds the branch to the instance that actually owns session_id.
INSERT INTO session_branch_ownerships (session_id, session_instance_id, branch, worktree_path, registered_at)
SELECT s.id, s.instance_id, sqlc.arg(branch), sqlc.arg(worktree_path), sqlc.arg(registered_at)
FROM sessions s
WHERE s.id = sqlc.arg(session_id)
  AND s.instance_id <> ''
  AND sqlc.arg(branch) <> ''
ON CONFLICT (session_instance_id, branch) DO NOTHING;

-- name: RegisterSessionBranchTip :execrows
INSERT INTO session_branch_tips (session_instance_id, branch, head_sha, observed_at)
SELECT b.session_instance_id, b.branch, sqlc.arg(head_sha), sqlc.arg(observed_at)
FROM session_branch_ownerships b
WHERE b.session_instance_id = sqlc.arg(session_instance_id)
  AND b.branch = sqlc.arg(branch)
  AND sqlc.arg(head_sha) <> ''
ON CONFLICT (session_instance_id, branch, head_sha) DO NOTHING;

-- name: ListSessionBranchTips :many
SELECT session_instance_id, branch, head_sha
FROM session_branch_tips
WHERE session_instance_id = ?
ORDER BY branch, head_sha;

-- name: RegisterSessionPROwnership :execrows
INSERT INTO session_pr_ownerships (pr_url, session_instance_id, source_branch, head_sha, registered_at)
SELECT sqlc.arg(pr_url), b.session_instance_id, b.branch, sqlc.arg(head_sha), sqlc.arg(registered_at)
FROM session_branch_ownerships b
JOIN session_branch_tips t
  ON t.session_instance_id = b.session_instance_id
 AND t.branch = b.branch
 AND t.head_sha = sqlc.arg(head_sha)
WHERE b.session_instance_id = sqlc.arg(session_instance_id)
  AND b.branch = sqlc.arg(source_branch)
ON CONFLICT (pr_url) DO NOTHING;

-- name: GetSessionPROwnership :one
SELECT p.pr_url, p.session_instance_id, p.source_branch, p.head_sha, p.registered_at
FROM session_pr_ownerships p
JOIN session_branch_ownerships b
  ON b.session_instance_id = p.session_instance_id AND b.branch = p.source_branch
JOIN session_branch_tips t
  ON t.session_instance_id = p.session_instance_id
 AND t.branch = p.source_branch
 AND t.head_sha = p.head_sha
WHERE p.pr_url = ?;

-- name: GetSessionInstanceID :one
SELECT instance_id FROM sessions WHERE id = ?;
