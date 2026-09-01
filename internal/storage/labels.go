package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/heath0xff/mrmr/internal/event"
)

// LabelEvent records the human judgment for an event. Relabeling replaces the
// prior judgment so the export always contains one current answer per event.
func (d *DB) LabelEvent(eventID, category string, requiresAction bool, expectedOutcome string) (*EventLabel, error) {
	category = strings.TrimSpace(category)
	if category == "" {
		return nil, fmt.Errorf("category is required")
	}
	if expectedOutcome != "ignore" && expectedOutcome != "notify" {
		return nil, fmt.Errorf("expected outcome must be ignore or notify")
	}
	var exists int
	if err := d.QueryRow(`SELECT 1 FROM events WHERE id = ?`, eventID).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("event %s not found", eventID)
		}
		return nil, fmt.Errorf("find event %s: %w", eventID, err)
	}

	label := &EventLabel{
		EventID:         eventID,
		Category:        category,
		RequiresAction:  requiresAction,
		ExpectedOutcome: expectedOutcome,
		LabeledAt:       time.Now().UTC(),
	}
	_, err := d.Exec(`INSERT INTO event_labels (event_id, category, requires_action, expected_outcome, labeled_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(event_id) DO UPDATE SET
			category = excluded.category,
			requires_action = excluded.requires_action,
			expected_outcome = excluded.expected_outcome,
			labeled_at = excluded.labeled_at`,
		label.EventID, label.Category, label.RequiresAction, label.ExpectedOutcome, label.LabeledAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("label event %s: %w", eventID, err)
	}
	return label, nil
}

// ListEvents returns recent events with their latest decision, execution, and
// optional human label. It is the backing query for the local review queue.
func (d *DB) ListEvents(limit int, unlabeledOnly bool) ([]ReviewEvent, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("limit must be positive")
	}
	where := ""
	if unlabeledOnly {
		where = "WHERE l.event_id IS NULL"
	}
	rows, err := d.Query(`
SELECT e.id, e.type, e.source, e.subject, e.event_time, e.data, e.metadata,
       d.id, d.interpreter, d.model, d.status, d.result, d.latency_ms, d.error,
       x.outcome,
       l.category, l.requires_action, l.expected_outcome, l.labeled_at
FROM events e
LEFT JOIN decisions d ON d.id = (
	SELECT id FROM decisions WHERE event_id = e.id ORDER BY created_at DESC LIMIT 1
)
LEFT JOIN executions x ON x.id = (
	SELECT id FROM executions WHERE event_id = e.id ORDER BY created_at DESC LIMIT 1
)
LEFT JOIN event_labels l ON l.event_id = e.id
`+where+`
ORDER BY e.ingest_time DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var events []ReviewEvent
	for rows.Next() {
		var (
			id, typ, source, subject, eventTime string
			dataJSON, metadataJSON              sql.NullString
			decisionID, interpreter, modelName  sql.NullString
			decisionStatus, resultJSON          sql.NullString
			latency                             sql.NullInt64
			decisionErr, outcome                sql.NullString
			category, expectedOutcome           sql.NullString
			requiresAction                      sql.NullInt64
			labeledAt                           sql.NullString
		)
		if err := rows.Scan(
			&id, &typ, &source, &subject, &eventTime, &dataJSON, &metadataJSON,
			&decisionID, &interpreter, &modelName, &decisionStatus, &resultJSON, &latency, &decisionErr,
			&outcome, &category, &requiresAction, &expectedOutcome, &labeledAt,
		); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e, err := decodeStoredEvent(id, typ, source, subject, eventTime, dataJSON, metadataJSON)
		if err != nil {
			return nil, err
		}
		review := ReviewEvent{Event: e, Outcome: outcome.String}
		if decisionID.Valid {
			dec := &event.Decision{
				ID: decisionID.String, EventID: id, Interpreter: interpreter.String,
				Model: modelName.String, Status: decisionStatus.String,
				LatencyMs: latency.Int64, Error: decisionErr.String,
			}
			if resultJSON.Valid && resultJSON.String != "null" {
				if err := json.Unmarshal([]byte(resultJSON.String), &dec.Result); err != nil {
					return nil, fmt.Errorf("decode decision %s result: %w", dec.ID, err)
				}
			}
			review.Decision = dec
		}
		if category.Valid {
			at, err := time.Parse(time.RFC3339Nano, labeledAt.String)
			if err != nil {
				return nil, fmt.Errorf("decode label for event %s: %w", id, err)
			}
			review.Label = &EventLabel{
				EventID: id, Category: category.String, RequiresAction: requiresAction.Int64 != 0,
				ExpectedOutcome: expectedOutcome.String, LabeledAt: at,
			}
		}
		events = append(events, review)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	return events, nil
}

// LabeledEvents returns every labeled event in stable ingestion order for
// deterministic JSONL exports.
func (d *DB) LabeledEvents() ([]LabeledEvent, error) {
	rows, err := d.Query(`
SELECT e.id, e.type, e.source, e.subject, e.event_time, e.data, e.metadata,
       l.category, l.requires_action, l.expected_outcome, l.labeled_at
FROM event_labels l
JOIN events e ON e.id = l.event_id
ORDER BY e.ingest_time, e.id`)
	if err != nil {
		return nil, fmt.Errorf("list labeled events: %w", err)
	}
	defer rows.Close()

	var events []LabeledEvent
	for rows.Next() {
		var (
			id, typ, source, subject, eventTime string
			dataJSON, metadataJSON              sql.NullString
			category, expectedOutcome           string
			requiresAction                      bool
			labeledAt                           string
		)
		if err := rows.Scan(&id, &typ, &source, &subject, &eventTime, &dataJSON, &metadataJSON,
			&category, &requiresAction, &expectedOutcome, &labeledAt); err != nil {
			return nil, fmt.Errorf("scan labeled event: %w", err)
		}
		e, err := decodeStoredEvent(id, typ, source, subject, eventTime, dataJSON, metadataJSON)
		if err != nil {
			return nil, err
		}
		at, err := time.Parse(time.RFC3339Nano, labeledAt)
		if err != nil {
			return nil, fmt.Errorf("decode label for event %s: %w", id, err)
		}
		events = append(events, LabeledEvent{Event: e, Label: EventLabel{
			EventID: id, Category: category, RequiresAction: requiresAction,
			ExpectedOutcome: expectedOutcome, LabeledAt: at,
		}})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list labeled events: %w", err)
	}
	return events, nil
}

func decodeStoredEvent(id, typ, source, subject, eventTime string, dataJSON, metadataJSON sql.NullString) (event.Event, error) {
	at, err := time.Parse(time.RFC3339Nano, eventTime)
	if err != nil {
		return event.Event{}, fmt.Errorf("decode event %s time: %w", id, err)
	}
	e := event.Event{ID: id, Type: typ, Source: source, Subject: subject, Timestamp: at}
	if dataJSON.Valid && dataJSON.String != "null" {
		if err := json.Unmarshal([]byte(dataJSON.String), &e.Data); err != nil {
			return event.Event{}, fmt.Errorf("decode event %s data: %w", id, err)
		}
	}
	if metadataJSON.Valid && metadataJSON.String != "null" {
		if err := json.Unmarshal([]byte(metadataJSON.String), &e.Metadata); err != nil {
			return event.Event{}, fmt.Errorf("decode event %s metadata: %w", id, err)
		}
	}
	return e, nil
}
