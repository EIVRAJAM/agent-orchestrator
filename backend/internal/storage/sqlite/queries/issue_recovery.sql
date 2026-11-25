-- name: GetIssueRecovery :one
SELECT session_id, issue_id, instance_id, state, reason, attempts,
       local_commits, local_changes, local_changes_known, push, turn, pr_url, head_sha, observed_at,
       attempted_at, updated_at
FROM issue_recovery
WHERE session_id = ?;

-- name: EnsureIssueRecovery :execrows
INSERT INTO issue_recovery (
    session_id, issue_id, instance_id, state, reason, attempts,
    local_commits, local_changes, local_changes_known, push, turn, pr_url, head_sha, observed_at, updated_at
) VALUES (
    sqlc.arg(session_id), sqlc.arg(issue_id), sqlc.arg(instance_id),
    sqlc.arg(state), sqlc.arg(reason), 0,
    sqlc.arg(local_commits), sqlc.arg(local_changes), sqlc.arg(local_changes_known),
    sqlc.arg(push), sqlc.arg(turn), sqlc.arg(pr_url), sqlc.arg(head_sha),
    sqlc.arg(observed_at), sqlc.arg(updated_at)
)
ON CONFLICT (session_id) DO NOTHING;

-- name: UpdateIssueRecoveryObservation :execrows
UPDATE issue_recovery
SET reason = sqlc.arg(reason),
    local_commits = sqlc.arg(local_commits),
    local_changes = sqlc.arg(local_changes),
    local_changes_known = sqlc.arg(local_changes_known),
    push = sqlc.arg(push),
    turn = sqlc.arg(turn),
    pr_url = sqlc.arg(pr_url),
    head_sha = sqlc.arg(head_sha),
    observed_at = sqlc.arg(observed_at),
    updated_at = sqlc.arg(updated_at)
WHERE session_id = sqlc.arg(session_id)
  AND instance_id = sqlc.arg(instance_id)
  AND state IN ('observing', 'continued');

-- name: ClaimIssueRecovery :execrows
UPDATE issue_recovery
SET state = 'continued',
    reason = 'continuation_sent',
    attempts = 1,
    attempted_at = sqlc.arg(attempted_at),
    updated_at = sqlc.arg(updated_at)
WHERE session_id = sqlc.arg(session_id)
  AND instance_id = sqlc.arg(instance_id)
  AND state = 'observing'
  AND attempts = 0;

-- name: FinishIssueRecovery :execrows
UPDATE issue_recovery
SET state = sqlc.arg(state),
    reason = sqlc.arg(reason),
    local_commits = sqlc.arg(local_commits),
    local_changes = sqlc.arg(local_changes),
    local_changes_known = sqlc.arg(local_changes_known),
    push = sqlc.arg(push),
    turn = sqlc.arg(turn),
    pr_url = sqlc.arg(pr_url),
    head_sha = sqlc.arg(head_sha),
    updated_at = sqlc.arg(updated_at)
WHERE session_id = sqlc.arg(session_id)
  AND instance_id = sqlc.arg(instance_id)
  AND state <> 'complete';
