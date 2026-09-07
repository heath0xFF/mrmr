// Source cursor persistence. The cursor is a per-source watermark: the
// poller's restart-safe position in a feed. It is an optimization and a
// resume point, never the dedup mechanism — dedup lives in InsertEvent's
// (source, source_event_id) key, so a lost or stale cursor can cause
// re-offering, never re-processing.
package storage

import (
	"database/sql"
	"fmt"
)

// Cursor returns the persisted watermark for a source. An unknown source is
// a fresh poller: "" and no error.
func (d *DB) Cursor(name string) (string, error) {
	var c string
	err := d.QueryRow(`SELECT cursor FROM source_cursors WHERE name = ?`, name).Scan(&c)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read cursor %s: %w", name, err)
	}
	return c, nil
}

// SetCursor persists the watermark upsert-style: the poll loop is the only
// writer per source, so last-write-wins is correct.
func (d *DB) SetCursor(name, cursor string) error {
	_, err := d.Exec(`INSERT INTO source_cursors (name, cursor, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at`,
		name, cursor, now())
	if err != nil {
		return fmt.Errorf("set cursor %s: %w", name, err)
	}
	return nil
}
