package storage

import (
	"database/sql"
	"fmt"
)

// EffectsBlocked applies even when an operator restarts without the management
// listener. Omitting a UI flag must never clear a durable safety decision.
func (d *DB) EffectsBlocked() (bool, error) {
	var value string
	err := d.QueryRow(`SELECT value FROM control_settings WHERE name='shadow'`).Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("read durable effect safety gate: %w", err)
	}
	if value != "true" && value != "false" {
		return true, fmt.Errorf("invalid durable effect safety gate")
	}
	return value == "true", nil
}
