-- +goose Up
-- Pin the PR head that made the tracker write-back ready. A claimed intent for
-- a different head must fail closed rather than publish a stale readiness
-- comment after the branch moved.
ALTER TABLE tracker_writebacks ADD COLUMN head_sha TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tracker_writebacks DROP COLUMN head_sha;
