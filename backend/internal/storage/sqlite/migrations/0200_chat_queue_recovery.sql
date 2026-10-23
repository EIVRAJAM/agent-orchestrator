-- +goose Up
-- Durable facts for preserving never-sent Chat work across controller loss.
--
-- queue_recovery_required is an operational hold, not a display state. It is
-- set when a replacement controller adopts queued rows that were left behind;
-- it keeps those rows from draining automatically until a person explicitly
-- resumes the queue.
ALTER TABLE conversations ADD COLUMN queue_recovery_required INTEGER NOT NULL DEFAULT 0
    CHECK (queue_recovery_required IN (0, 1));

-- A Stop/cutoff is identified by the durable order of the queue, never by a
-- wall-clock timestamp. The rowid is SQLite's insert order; FIFO is already
-- requested_at then rowid, and rowid is strictly increasing within this schema.
ALTER TABLE conversations ADD COLUMN stop_reason TEXT NOT NULL DEFAULT ''
    CHECK (stop_reason IN ('', 'user_stop', 'interface_interrupt', 'handoff_interrupt', 'agent_switch', 'session_terminated'));
ALTER TABLE conversations ADD COLUMN stop_episode_id TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN stop_intent_at TIMESTAMP;
ALTER TABLE conversations ADD COLUMN stop_cutoff_rowid INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE conversations DROP COLUMN stop_cutoff_rowid;
ALTER TABLE conversations DROP COLUMN stop_intent_at;
ALTER TABLE conversations DROP COLUMN stop_episode_id;
ALTER TABLE conversations DROP COLUMN stop_reason;
ALTER TABLE conversations DROP COLUMN queue_recovery_required;
