package sqlite

import (
	"testing"
	"time"
)

// Migration 0200 adds only operational facts: a manual-recovery hold and the
// durable Stop episode/cutoff. Existing conversations keep draining normally and
// no display state is introduced.
func TestMigration0200AddsChatQueueRecoveryFacts(t *testing.T) {
	db := openMigratedDatabaseCopyNoForeignKeys(t, 199)
	now := time.Date(2026, time.October, 23, 12, 0, 0, 0, time.UTC)
	mustExec(t, db, `
INSERT INTO projects (id, path, display_name, registered_at) VALUES
    ('p', '/tmp/p', 'P', ?);
INSERT INTO conversations (id, scope, project_id, created_at, updated_at) VALUES
    ('old-conversation', 'project', 'p', ?, ?);`, now, now, now)

	upTo(t, db, 200)

	var (
		recovery  int
		reason    string
		episode   string
		intentAt  any
		cutoffRow int
	)
	if err := db.QueryRow(`SELECT queue_recovery_required, stop_reason, stop_episode_id,
        stop_intent_at, stop_cutoff_rowid FROM conversations WHERE id = 'old-conversation'`).
		Scan(&recovery, &reason, &episode, &intentAt, &cutoffRow); err != nil {
		t.Fatalf("read migrated conversation facts: %v", err)
	}
	if recovery != 0 || reason != "" || episode != "" || intentAt != nil || cutoffRow != 0 {
		t.Fatalf("migrated defaults = (%d, %q, %q, %v, %d), want zero facts",
			recovery, reason, episode, intentAt, cutoffRow)
	}

	if _, err := db.Exec(`UPDATE conversations SET stop_reason = 'free-form' WHERE id = 'old-conversation'`); err == nil {
		t.Fatal("free-form stop reason was accepted")
	}
	if _, err := db.Exec(`UPDATE conversations SET queue_recovery_required = 2 WHERE id = 'old-conversation'`); err == nil {
		t.Fatal("non-boolean queue recovery hold was accepted")
	}
}
