package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestUpgradePreservesExistingEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Build the actual pre-lineage schema, not a mock approximation of it.
	for _, migration := range migrations[:6] {
		if _, err := raw.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
 INSERT INTO schema_migrations VALUES (6, '2026-01-01T00:00:00Z');
 INSERT INTO events (id,type,source,event_time,ingest_time,data,metadata,dedup_key)
 VALUES ('evt_old','old','test','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','{"value":9007199254740993}','{}','old');`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := db.Inspect("evt_old")
	if err != nil {
		t.Fatal(err)
	}
	if ins.Event.Depth != 0 || ins.Event.Provenance != nil || len(ins.ChildEventIDs) != 0 {
		t.Fatalf("invented historical lineage: %+v", ins.Event)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Inspect("evt_old"); err != nil {
		t.Fatal(err)
	}
}

func TestLineageMigrationIsTransactional(t *testing.T) {
	db := openTestDB(t)
	// Apply to a deliberately conflicting shape: the depth ALTER succeeds,
	// then provenance fails. Rollback must remove depth and preserve old rows.
	if _, err := db.Exec(`DROP INDEX idx_events_parent; ALTER TABLE events DROP COLUMN depth;
 DELETE FROM schema_migrations WHERE version >= 7;`); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(); err == nil {
		t.Fatal("conflicting migration should fail")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('events') WHERE name='depth'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed migration left a partially altered schema")
	}
}
