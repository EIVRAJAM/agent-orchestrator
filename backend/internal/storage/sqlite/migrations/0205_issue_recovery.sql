-- +goose Up
-- Durable bounded-continuation state for issue workers. One row per session;
-- attempts can never exceed one, even across daemon restarts.
CREATE TABLE issue_recovery (
    session_id    TEXT PRIMARY KEY REFERENCES sessions (id) ON DELETE CASCADE,
    issue_id      TEXT NOT NULL,
    instance_id   TEXT NOT NULL,
    state         TEXT NOT NULL,
    reason        TEXT NOT NULL DEFAULT '',
    attempts      INTEGER NOT NULL DEFAULT 0,
    local_commits INTEGER NOT NULL DEFAULT 0,
    local_changes INTEGER NOT NULL DEFAULT 0,
    local_changes_known INTEGER NOT NULL DEFAULT 0,
    push          TEXT NOT NULL DEFAULT '',
    turn          TEXT NOT NULL DEFAULT '',
    pr_url        TEXT NOT NULL DEFAULT '',
    head_sha      TEXT NOT NULL DEFAULT '',
    observed_at   TIMESTAMP NOT NULL,
    attempted_at  TIMESTAMP,
    updated_at    TIMESTAMP NOT NULL
);

CREATE INDEX idx_issue_recovery_state ON issue_recovery (state);

-- +goose Down
DROP TABLE issue_recovery;
