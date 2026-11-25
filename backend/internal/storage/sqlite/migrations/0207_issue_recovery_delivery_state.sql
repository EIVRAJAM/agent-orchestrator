-- +goose Up
-- Separate the durable claim from the demonstrable send. Existing rows from
-- the previous unshipped shape are conservative: we cannot prove the prompt
-- was delivered after the claim, so they become uncertain and are not resent.
ALTER TABLE issue_recovery ADD COLUMN sent_at TIMESTAMP;
UPDATE issue_recovery
SET state = 'uncertain', reason = 'continuation_uncertain'
WHERE state = 'continued';

-- +goose Down
ALTER TABLE issue_recovery DROP COLUMN sent_at;
