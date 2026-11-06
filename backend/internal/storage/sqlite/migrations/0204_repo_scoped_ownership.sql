-- +goose Up
-- Repository identity is part of ownership. The existing 0202 tables were
-- instance+branch scoped; rebuild them with the canonical provider|host|repo
-- key. Historical rows without repository evidence are retained only as
-- inert rows with an empty repo key; historical PR ownership is not copied
-- because it cannot prove a repository and must fail closed.
-- +goose StatementBegin
ALTER TABLE session_worktrees ADD COLUMN repo_key TEXT NOT NULL DEFAULT '';

CREATE TABLE session_branch_ownerships_v2 (
    session_id          TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    session_instance_id TEXT NOT NULL,
    repo_key            TEXT NOT NULL,
    branch              TEXT NOT NULL,
    worktree_path       TEXT NOT NULL DEFAULT '',
    registered_at       TIMESTAMP NOT NULL,
    PRIMARY KEY (session_instance_id, repo_key, branch)
);
INSERT OR IGNORE INTO session_branch_ownerships_v2 (session_id, session_instance_id, repo_key, branch, worktree_path, registered_at)
SELECT session_id, session_instance_id, '', branch, worktree_path, registered_at
FROM session_branch_ownerships;

CREATE TABLE session_branch_tips_v2 (
    session_instance_id TEXT NOT NULL,
    repo_key            TEXT NOT NULL,
    branch              TEXT NOT NULL,
    head_sha            TEXT NOT NULL,
    observed_at         TIMESTAMP NOT NULL,
    PRIMARY KEY (session_instance_id, repo_key, branch, head_sha),
    FOREIGN KEY (session_instance_id, repo_key, branch)
        REFERENCES session_branch_ownerships_v2 (session_instance_id, repo_key, branch) ON DELETE CASCADE
);
INSERT OR IGNORE INTO session_branch_tips_v2 (session_instance_id, repo_key, branch, head_sha, observed_at)
SELECT session_instance_id, '', branch, head_sha, observed_at
FROM session_branch_tips;

CREATE TABLE session_pr_ownerships_v2 (
    pr_url              TEXT PRIMARY KEY,
    session_instance_id TEXT NOT NULL,
    repo_key            TEXT NOT NULL,
    source_branch       TEXT NOT NULL,
    head_sha            TEXT NOT NULL,
    registered_at       TIMESTAMP NOT NULL,
    FOREIGN KEY (session_instance_id, repo_key, source_branch)
        REFERENCES session_branch_ownerships_v2 (session_instance_id, repo_key, branch) ON DELETE CASCADE
);

DROP TABLE session_pr_ownerships;
DROP TABLE session_branch_tips;
DROP TABLE session_branch_ownerships;
ALTER TABLE session_branch_ownerships_v2 RENAME TO session_branch_ownerships;
ALTER TABLE session_branch_tips_v2 RENAME TO session_branch_tips;
ALTER TABLE session_pr_ownerships_v2 RENAME TO session_pr_ownerships;

CREATE INDEX idx_session_branch_ownerships_session ON session_branch_ownerships (session_id);
CREATE INDEX idx_session_branch_ownerships_branch ON session_branch_ownerships (branch);
CREATE INDEX idx_session_branch_tips_lookup ON session_branch_tips (branch, head_sha);
CREATE INDEX idx_session_pr_ownerships_instance ON session_pr_ownerships (session_instance_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE TABLE session_branch_ownerships_old (
    session_id          TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    session_instance_id TEXT NOT NULL,
    branch              TEXT NOT NULL,
    worktree_path       TEXT NOT NULL DEFAULT '',
    registered_at       TIMESTAMP NOT NULL,
    PRIMARY KEY (session_instance_id, branch)
);
INSERT OR IGNORE INTO session_branch_ownerships_old (session_id, session_instance_id, branch, worktree_path, registered_at)
SELECT session_id, session_instance_id, branch, worktree_path, registered_at
FROM session_branch_ownerships;

CREATE TABLE session_branch_tips_old (
    session_instance_id TEXT NOT NULL,
    branch              TEXT NOT NULL,
    head_sha            TEXT NOT NULL,
    observed_at         TIMESTAMP NOT NULL,
    PRIMARY KEY (session_instance_id, branch, head_sha),
    FOREIGN KEY (session_instance_id, branch)
        REFERENCES session_branch_ownerships_old (session_instance_id, branch) ON DELETE CASCADE
);
INSERT OR IGNORE INTO session_branch_tips_old (session_instance_id, branch, head_sha, observed_at)
SELECT session_instance_id, branch, head_sha, observed_at
FROM session_branch_tips;

CREATE TABLE session_pr_ownerships_old (
    pr_url              TEXT PRIMARY KEY,
    session_instance_id TEXT NOT NULL,
    source_branch       TEXT NOT NULL,
    head_sha            TEXT NOT NULL,
    registered_at       TIMESTAMP NOT NULL,
    FOREIGN KEY (session_instance_id, source_branch)
        REFERENCES session_branch_ownerships_old (session_instance_id, branch) ON DELETE CASCADE
);
INSERT OR IGNORE INTO session_pr_ownerships_old (pr_url, session_instance_id, source_branch, head_sha, registered_at)
SELECT pr_url, session_instance_id, source_branch, head_sha, registered_at
FROM session_pr_ownerships;

DROP TABLE session_pr_ownerships;
DROP TABLE session_branch_tips;
DROP TABLE session_branch_ownerships;
ALTER TABLE session_branch_ownerships_old RENAME TO session_branch_ownerships;
ALTER TABLE session_branch_tips_old RENAME TO session_branch_tips;
ALTER TABLE session_pr_ownerships_old RENAME TO session_pr_ownerships;

CREATE INDEX idx_session_branch_ownerships_session ON session_branch_ownerships (session_id);
CREATE INDEX idx_session_branch_ownerships_branch ON session_branch_ownerships (branch);
CREATE INDEX idx_session_branch_tips_lookup ON session_branch_tips (branch, head_sha);
CREATE INDEX idx_session_pr_ownerships_instance ON session_pr_ownerships (session_instance_id);
ALTER TABLE session_worktrees DROP COLUMN repo_key;
-- +goose StatementEnd
