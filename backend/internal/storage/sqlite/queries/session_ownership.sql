-- name: RegisterSessionBranch :execrows
-- Register one exact branch/worktree relation for a session instance and
-- canonical repository identity. The subquery binds the branch to the
-- instance that actually owns session_id.
INSERT INTO session_branch_ownerships (session_id, session_instance_id, repo_key, branch, worktree_path, registered_at)
SELECT s.id, s.instance_id, sqlc.arg(repo_key), sqlc.arg(branch), sqlc.arg(worktree_path), sqlc.arg(registered_at)
FROM sessions s
WHERE s.id = sqlc.arg(session_id)
  AND s.instance_id <> ''
  AND sqlc.arg(repo_key) <> ''
  AND sqlc.arg(branch) <> ''
ON CONFLICT (session_instance_id, repo_key, branch) DO NOTHING;

-- name: RegisterSessionBranchTip :execrows
INSERT INTO session_branch_tips (session_instance_id, repo_key, branch, head_sha, observed_at)
SELECT b.session_instance_id, b.repo_key, b.branch, sqlc.arg(head_sha), sqlc.arg(observed_at)
FROM session_branch_ownerships b
WHERE b.session_instance_id = sqlc.arg(session_instance_id)
  AND b.repo_key = sqlc.arg(repo_key)
  AND b.branch = sqlc.arg(branch)
  AND sqlc.arg(head_sha) <> ''
ON CONFLICT (session_instance_id, repo_key, branch, head_sha) DO NOTHING;

-- name: ListSessionBranchTips :many
SELECT session_instance_id, repo_key, branch, head_sha
FROM session_branch_tips
WHERE session_instance_id = ?
ORDER BY repo_key, branch, head_sha;

-- name: RegisterSessionPROwnership :execrows
INSERT INTO session_pr_ownerships (pr_url, session_instance_id, repo_key, source_branch, head_sha, registered_at)
SELECT sqlc.arg(pr_url), b.session_instance_id, b.repo_key, b.branch, sqlc.arg(head_sha), sqlc.arg(registered_at)
FROM session_branch_ownerships b
JOIN session_branch_tips t
  ON t.session_instance_id = b.session_instance_id
 AND t.repo_key = b.repo_key
 AND t.branch = b.branch
 AND t.head_sha = sqlc.arg(head_sha)
WHERE b.session_instance_id = sqlc.arg(session_instance_id)
  AND b.repo_key = sqlc.arg(repo_key)
  AND b.branch = sqlc.arg(source_branch)
ON CONFLICT (pr_url) DO NOTHING;

-- name: GetSessionPROwnership :one
SELECT p.pr_url, p.session_instance_id, p.repo_key, p.source_branch, p.head_sha, p.registered_at
FROM session_pr_ownerships p
JOIN session_branch_ownerships b
  ON b.session_instance_id = p.session_instance_id
 AND b.repo_key = p.repo_key
 AND b.branch = p.source_branch
JOIN session_branch_tips t
  ON t.session_instance_id = p.session_instance_id
 AND t.repo_key = p.repo_key
 AND t.branch = p.source_branch
 AND t.head_sha = p.head_sha
WHERE p.pr_url = ?;

-- name: GetSessionInstanceID :one
SELECT instance_id FROM sessions WHERE id = ?;

-- name: RefreshSessionPROwnership :execrows
-- Advance an existing PR ownership row only when the new head is already a
-- registered tip of the same instance/repository/source branch. This is the
-- branch-move path; a foreign head or repository cannot replace ownership.
UPDATE session_pr_ownerships
SET head_sha = sqlc.arg(new_head_sha),
    registered_at = sqlc.arg(new_registered_at)
WHERE session_pr_ownerships.pr_url = sqlc.arg(target_pr_url)
  AND session_pr_ownerships.session_instance_id = sqlc.arg(expected_instance_id)
  AND session_pr_ownerships.repo_key = sqlc.arg(expected_repo_key)
  AND session_pr_ownerships.source_branch = sqlc.arg(expected_source_branch)
  AND EXISTS (
      SELECT 1
      FROM session_branch_tips AS t
      WHERE t.session_instance_id = sqlc.arg(expected_instance_id)
        AND t.repo_key = sqlc.arg(expected_repo_key)
        AND t.branch = sqlc.arg(expected_source_branch)
        AND t.head_sha = sqlc.arg(new_head_sha)
  );

-- name: GetSessionProjectOrigin :one
SELECT p.repo_origin_url
FROM sessions s
JOIN projects p ON p.id = s.project_id
WHERE s.id = ?;

-- name: GetWorkspaceRepoOrigin :one
SELECT wr.repo_origin_url
FROM sessions s
JOIN workspace_repos wr ON wr.project_id = s.project_id
WHERE s.id = ? AND wr.name = ?;
