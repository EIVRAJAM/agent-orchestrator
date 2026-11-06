-- +goose Up
-- Instance identity is independent of the human-facing session id. A fresh
-- AO_DATA_DIR can reuse project-1 while this random identity remains unique.
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN instance_id TEXT NOT NULL DEFAULT '';
UPDATE sessions SET instance_id = lower(hex(randomblob(16))) WHERE instance_id = '';
-- Partial uniqueness permits historical/raw inserts that omitted the column;
-- the AFTER INSERT trigger below fills those rows immediately. Production
-- writes always supply a UUID, so the non-empty namespace is unique.
CREATE UNIQUE INDEX idx_sessions_instance_id ON sessions (instance_id) WHERE instance_id <> '';
CREATE TRIGGER trg_sessions_instance_id_default
AFTER INSERT ON sessions
WHEN NEW.instance_id = ''
BEGIN
    UPDATE sessions SET instance_id = lower(hex(randomblob(16)))
    WHERE id = NEW.id AND instance_id = '';
END;

CREATE TABLE session_branch_ownerships (
    session_id          TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    session_instance_id TEXT NOT NULL,
    branch              TEXT NOT NULL,
    worktree_path       TEXT NOT NULL DEFAULT '',
    registered_at       TIMESTAMP NOT NULL,
    PRIMARY KEY (session_instance_id, branch)
);
CREATE INDEX idx_session_branch_ownerships_session ON session_branch_ownerships (session_id);
CREATE INDEX idx_session_branch_ownerships_branch ON session_branch_ownerships (branch);

CREATE TABLE session_branch_tips (
    session_instance_id TEXT NOT NULL,
    branch              TEXT NOT NULL,
    head_sha            TEXT NOT NULL,
    observed_at         TIMESTAMP NOT NULL,
    PRIMARY KEY (session_instance_id, branch, head_sha),
    FOREIGN KEY (session_instance_id, branch)
        REFERENCES session_branch_ownerships (session_instance_id, branch) ON DELETE CASCADE
);
CREATE INDEX idx_session_branch_tips_lookup ON session_branch_tips (branch, head_sha);

CREATE TABLE session_pr_ownerships (
    pr_url              TEXT PRIMARY KEY,
    session_instance_id TEXT NOT NULL,
    source_branch       TEXT NOT NULL,
    head_sha            TEXT NOT NULL,
    registered_at       TIMESTAMP NOT NULL,
    FOREIGN KEY (session_instance_id, source_branch)
        REFERENCES session_branch_ownerships (session_instance_id, branch) ON DELETE CASCADE
);
CREATE INDEX idx_session_pr_ownerships_instance ON session_pr_ownerships (session_instance_id);
-- +goose StatementEnd

-- Backfill ownership for existing rows from the durable worktree records.
-- +goose StatementBegin
INSERT OR IGNORE INTO session_branch_ownerships (session_id, session_instance_id, branch, worktree_path, registered_at)
SELECT id, instance_id, branch, workspace_path, created_at
FROM sessions
WHERE branch <> '';

INSERT OR IGNORE INTO session_branch_ownerships (session_id, session_instance_id, branch, worktree_path, registered_at)
SELECT s.id, s.instance_id, w.branch, w.worktree_path, s.created_at
FROM session_worktrees w
JOIN sessions s ON s.id = w.session_id
WHERE w.branch <> '';

INSERT OR IGNORE INTO session_branch_tips (session_instance_id, branch, head_sha, observed_at)
SELECT s.instance_id, w.branch, w.creation_sha, s.created_at
FROM session_worktrees w
JOIN sessions s ON s.id = w.session_id
WHERE w.branch <> '' AND w.creation_sha <> '';

INSERT OR IGNORE INTO session_branch_tips (session_instance_id, branch, head_sha, observed_at)
SELECT s.instance_id, w.branch, w.base_sha, s.created_at
FROM session_worktrees w
JOIN sessions s ON s.id = w.session_id
WHERE w.branch <> '' AND w.base_sha <> '';

-- Existing PRs get ownership only when their source branch is an exact
-- registered session/worktree branch. Historical child branches and reused
-- prefixes deliberately remain unowned and therefore cannot write back.
INSERT OR IGNORE INTO session_branch_tips (session_instance_id, branch, head_sha, observed_at)
SELECT s.instance_id, p.source_branch, p.head_sha, p.updated_at
FROM pr p
JOIN sessions s ON s.id = p.session_id
JOIN session_branch_ownerships b
  ON b.session_instance_id = s.instance_id AND b.branch = p.source_branch
WHERE p.source_branch <> '' AND p.head_sha <> '';

INSERT OR IGNORE INTO session_pr_ownerships (pr_url, session_instance_id, source_branch, head_sha, registered_at)
SELECT p.url, s.instance_id, p.source_branch, p.head_sha, p.updated_at
FROM pr p
JOIN sessions s ON s.id = p.session_id
JOIN session_branch_ownerships b
  ON b.session_instance_id = s.instance_id AND b.branch = p.source_branch
JOIN session_branch_tips t
  ON t.session_instance_id = s.instance_id AND t.branch = p.source_branch AND t.head_sha = p.head_sha
WHERE p.source_branch <> '' AND p.head_sha <> '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER trg_sessions_instance_id_default;
DROP TABLE session_pr_ownerships;
DROP TABLE session_branch_tips;
DROP TABLE session_branch_ownerships;
DROP INDEX idx_sessions_instance_id;
ALTER TABLE sessions DROP COLUMN instance_id;
-- +goose StatementEnd
