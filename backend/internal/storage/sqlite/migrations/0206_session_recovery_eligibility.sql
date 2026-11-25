-- +goose Up
-- Automatic issue recovery is opt-in by durable creation fact. Existing rows
-- default to false; only workers inserted as issue tasks by this version are
-- eligible. This is intentionally not derived from idle, timestamps, or names.
ALTER TABLE sessions ADD COLUMN recovery_eligible INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sessions DROP COLUMN recovery_eligible;
