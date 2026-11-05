-- +goose Up
-- Durable, at-most-once outbox for the single tracker mutation AO permits:
-- a deterministic PR-ready issue comment. A unique key prevents duplicate
-- intents across retries, restarts, and concurrent observers. Once a row is
-- claimed it is never retried blindly; an ambiguous remote result becomes
-- 'uncertain' instead of causing a duplicate comment.
CREATE TABLE tracker_writebacks (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL REFERENCES projects (id),
    session_id    TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    issue_id      TEXT NOT NULL,
    pr_url        TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'claimed', 'sent', 'uncertain', 'failed')),
    attempts      INTEGER NOT NULL DEFAULT 0,
    comment_url   TEXT NOT NULL DEFAULT '',
    failure_code  TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMP NOT NULL,
    updated_at    TIMESTAMP NOT NULL,
    claimed_at    TIMESTAMP,
    UNIQUE (project_id, issue_id, pr_url)
);
CREATE INDEX idx_tracker_writebacks_pending
    ON tracker_writebacks (status, created_at, id);

-- +goose Down
DROP TABLE tracker_writebacks;
