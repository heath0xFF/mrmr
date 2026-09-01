// Package storage is mrmr's durable state: SQLite. This is deliberately
// the only package that knows a database exists. Migrations are explicit,
// additive, and applied in a transaction each — never silently destructive,
// because a runtime whose decisions can't be audited is a runtime whose
// decisions can't be trusted.
package storage

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/heath0xff/mrmr/internal/event"
)

// DB wraps the SQLite handle. A single connection is used because SQLite
// is serialized anyway; the events table doubles as the queue in later
// milestones, so writes funnel through one point.
type DB struct{ *sql.DB }

type EventRecord struct {
	Event     event.Event
	Duplicate bool
}

type EventLabel struct {
	EventID         string    `json:"event_id"`
	Category        string    `json:"category"`
	RequiresAction  bool      `json:"requires_action"`
	ExpectedOutcome string    `json:"expected_outcome"`
	LabeledAt       time.Time `json:"labeled_at"`
}

type ReviewEvent struct {
	Event    event.Event     `json:"event"`
	Decision *event.Decision `json:"decision,omitempty"`
	Outcome  string          `json:"outcome,omitempty"`
	Label    *EventLabel     `json:"label,omitempty"`
}

type LabeledEvent struct {
	Event event.Event `json:"event"`
	Label EventLabel  `json:"label"`
}

// Open opens (creating if needed) the SQLite database with WAL mode
// and applies pending migrations.
func Open(path string) (*DB, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // ponytail: single writer; SQLite is serialized anyway
	d := &DB{db}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// migrations run in order; each is a forward-only transaction. Add new
// ones by appending — never edit an applied migration, since existing
// deployments have already recorded its version.
var migrations = []string{`
CREATE TABLE events (
	id         TEXT PRIMARY KEY,
	type       TEXT NOT NULL,
	source     TEXT NOT NULL,
	subject    TEXT NOT NULL DEFAULT '',
	event_time TEXT NOT NULL,
	ingest_time TEXT NOT NULL,
	data       TEXT,
	metadata   TEXT,
	dedup_key  TEXT NOT NULL,
	UNIQUE(source, dedup_key)
);
CREATE INDEX idx_events_time ON events(event_time);
`,
	`
CREATE TABLE decisions (
	id          TEXT PRIMARY KEY,
	event_id    TEXT NOT NULL REFERENCES events(id),
	interpreter TEXT NOT NULL,
	model       TEXT NOT NULL,
	status      TEXT NOT NULL,
	result      TEXT,
	latency_ms  INTEGER NOT NULL,
	error       TEXT,
	created_at  TEXT NOT NULL
);
CREATE INDEX idx_decisions_event ON decisions(event_id);
`,
	`
CREATE TABLE executions (
	id          TEXT PRIMARY KEY,
	event_id    TEXT NOT NULL REFERENCES events(id),
	decision_id TEXT REFERENCES decisions(id),
	outcome     TEXT NOT NULL,
	adapter     TEXT,
	status      TEXT NOT NULL,
	error       TEXT,
	created_at  TEXT NOT NULL
);
CREATE INDEX idx_executions_event ON executions(event_id);
`,
	`
CREATE TABLE event_labels (
	event_id          TEXT PRIMARY KEY REFERENCES events(id),
	category          TEXT NOT NULL,
	requires_action   INTEGER NOT NULL CHECK (requires_action IN (0, 1)),
	expected_outcome  TEXT NOT NULL,
	labeled_at        TEXT NOT NULL
);
CREATE INDEX idx_event_labels_time ON event_labels(labeled_at);
`,
}

func (d *DB) migrate() error {
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var cur int
	if err := d.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&cur); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i, m := range migrations[cur:] {
		v := cur + i + 1
		tx, err := d.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(m); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", v, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, v, now()); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", v, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// InsertEvent persists an event. Dedup key: metadata.source_event_id when
// present, otherwise a hash of the payload — delivery is at-least-once
// (webhook retries, poller overlap), so the same thing happening twice must
// be recognized as one event. Duplicates are reported, not silently
// swallowed: the trace says "duplicate" and the caller decides what that
// means.
func (d *DB) InsertEvent(e event.Event) (*EventRecord, error) {
	data, _ := json.Marshal(e.Data)
	meta, _ := json.Marshal(e.Metadata)
	dedup := ""
	if v, ok := e.Metadata["source_event_id"]; ok {
		dedup = fmt.Sprint(v)
	}
	if dedup == "" {
		h := sha256.New()
		fmt.Fprint(h, e.Type, "\x00", e.Subject, "\x00", string(data))
		dedup = hex.EncodeToString(h.Sum(nil))
	}

	res, err := d.Exec(`INSERT INTO events (id, type, source, subject, event_time, ingest_time, data, metadata, dedup_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source, dedup_key) DO NOTHING`,
		e.ID, e.Type, e.Source, e.Subject, e.Timestamp.Format(time.RFC3339Nano), now(), data, meta, dedup)
	if err != nil {
		return nil, fmt.Errorf("insert event %s: %w", e.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("insert event %s: %w", e.ID, err)
	}
	if n == 0 {
		return &EventRecord{Event: e, Duplicate: true}, nil
	}
	return &EventRecord{Event: e}, nil
}

func (d *DB) InsertDecision(dec event.Decision) error {
	result, _ := json.Marshal(dec.Result)
	_, err := d.Exec(`INSERT INTO decisions (id, event_id, interpreter, model, status, result, latency_ms, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		dec.ID, dec.EventID, dec.Interpreter, dec.Model, dec.Status, result, dec.LatencyMs, dec.Error, now())
	if err != nil {
		return fmt.Errorf("insert decision %s: %w", dec.ID, err)
	}
	return nil
}

func (d *DB) InsertExecution(id, eventID, decisionID, outcome, adapter, status, errMsg string) error {
	_, err := d.Exec(`INSERT INTO executions (id, event_id, decision_id, outcome, adapter, status, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, eventID, decisionID, outcome, adapter, status, errMsg, now())
	if err != nil {
		return fmt.Errorf("insert execution %s: %w", id, err)
	}
	return nil
}
