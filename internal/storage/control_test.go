package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func legacyControlDB(t *testing.T) *DB {
	t.Helper()
	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	for _, migration := range migrations[:8] {
		if _, err := raw.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL);
 INSERT INTO schema_migrations VALUES(8,'2026-01-01T00:00:00Z');
 INSERT INTO events(id,type,source,event_time,ingest_time,dedup_key) VALUES('old','test','test','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','old');
 INSERT INTO event_labels VALUES('old','routine',0,'ignore','2026-01-01T00:00:00Z');`); err != nil {
		t.Fatal(err)
	}
	return &DB{raw}
}
func TestControlMigrationPreservesLabelsAndHistory(t *testing.T) {
	db := legacyControlDB(t)
	if err := db.migrate(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM label_history WHERE event_id='old'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("existing label not retained: %d %v", count, err)
	}
	if _, err := db.LabelEvent("old", "incident", true, "notify"); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(`SELECT count(*) FROM label_history WHERE event_id='old'`).Scan(&count)
	if count != 2 {
		t.Fatal("CLI correction lost its previous label")
	}
	if err := db.migrate(); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(`SELECT count(*) FROM label_history WHERE event_id='old'`).Scan(&count)
	if count != 2 {
		t.Fatal("reopen duplicated history")
	}
	db.QueryRow(`SELECT count(*) FROM review_state`).Scan(&count)
	if count != 0 {
		t.Fatal("migration invented real-event origin or review state")
	}
}
func TestFutureSchemaIsRejectedWithoutMutation(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`INSERT INTO schema_migrations VALUES(999,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(); err == nil {
		t.Fatal("future schema accepted")
	}
	var version int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 999 {
		t.Fatal("future schema was changed")
	}
}

func TestDurableSafetyGateFailsClosed(t *testing.T) {
	db := openTestDB(t)
	blocked, err := db.EffectsBlocked()
	if err != nil || blocked {
		t.Fatalf("new gate: %v %v", blocked, err)
	}
	for _, value := range []string{"true", "damaged"} {
		if _, err := db.Exec(`INSERT INTO control_settings VALUES('shadow',?) ON CONFLICT(name) DO UPDATE SET value=excluded.value`, value); err != nil {
			t.Fatal(err)
		}
		blocked, err = db.EffectsBlocked()
		if !blocked || (value == "damaged" && err == nil) {
			t.Fatalf("gate %q did not fail closed: %v %v", value, blocked, err)
		}
	}
}

func TestControlMigrationFailureRollsBack(t *testing.T) {
	db := legacyControlDB(t)
	if _, err := db.Exec(`CREATE TABLE control_settings(name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(); err == nil {
		t.Fatal("conflicting migration accepted")
	}
	var count int
	db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='control_revisions'`).Scan(&count)
	if count != 0 {
		t.Fatal("failed migration left revision table")
	}
	db.QueryRow(`SELECT count(*) FROM event_labels WHERE event_id='old'`).Scan(&count)
	if count != 1 {
		t.Fatal("failed migration destroyed old label")
	}
}
