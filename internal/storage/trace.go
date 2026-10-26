// Trace persistence and the single-event inspection query. The trace is
// written once, after the pipeline has finished with an event: the steps
// describe work that already happened, so there is nothing to gain from
// writing them incrementally and something to lose — a second write per
// stage on the synchronous ingest path.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/heath0xff/mrmr/internal/event"
)

// InsertTrace persists an event's pipeline trace in one transaction, so an
// inspected trace is either the whole story or absent — a half-written
// trace would read as "the runtime stopped at policy" and be a lie.
//
// Steps are stored with their arrival order (seq) and read back by it
// rather than by timestamp: steps within one event can share a clock tick,
// and the sequence is what actually happened.
func (d *DB) InsertTrace(eventID string, steps []event.TraceStep) error {
	if len(steps) == 0 {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("insert trace %s: %w", eventID, err)
	}
	defer tx.Rollback() // no-op once committed; covers every error return below

	stmt, err := tx.Prepare(`INSERT INTO event_traces (event_id, seq, at, stage, msg) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("insert trace %s: %w", eventID, err)
	}
	defer stmt.Close()
	for i, s := range steps {
		if _, err := stmt.Exec(eventID, i, s.At.UTC().Format(time.RFC3339Nano), s.Stage, s.Msg); err != nil {
			return fmt.Errorf("insert trace %s step %d: %w", eventID, i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("insert trace %s: %w", eventID, err)
	}
	return nil
}

// EventTrace returns one event's trace in pipeline order.
func (d *DB) EventTrace(eventID string) ([]event.TraceStep, error) {
	return d.eventTrace(context.Background(), eventID)
}

func (d *DB) eventTrace(ctx context.Context, eventID string) ([]event.TraceStep, error) {
	rows, err := d.QueryContext(ctx, `SELECT at, stage, msg FROM event_traces WHERE event_id = ? ORDER BY seq`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read trace %s: %w", eventID, err)
	}
	defer rows.Close()

	var steps []event.TraceStep
	for rows.Next() {
		var at, stage, msg string
		if err := rows.Scan(&at, &stage, &msg); err != nil {
			return nil, fmt.Errorf("scan trace %s: %w", eventID, err)
		}
		ts, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, fmt.Errorf("decode trace %s step time: %w", eventID, err)
		}
		steps = append(steps, event.TraceStep{At: ts, Stage: stage, Msg: msg})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read trace %s: %w", eventID, err)
	}
	return steps, nil
}

// Inspect assembles the durable records without holding a transaction across
// queries. In-flight work may still show pending intent or an incomplete trace;
// callers can inspect again after completion. No query holds the sole database
// connection while another query needs it.
func (d *DB) Inspect(eventID string) (*Inspection, error) {
	return d.InspectContext(context.Background(), eventID)
}

// InspectContext lets HTTP readers cancel work without changing the CLI contract.
func (d *DB) InspectContext(ctx context.Context, eventID string) (*Inspection, error) {
	var (
		id, typ, source, subject, eventTime    string
		dataJSON, metadataJSON, provenanceJSON sql.NullString
		depth                                  int
	)
	err := d.QueryRowContext(ctx, `SELECT id, type, source, subject, event_time, data, metadata, depth, provenance FROM events WHERE id = ?`, eventID).
		Scan(&id, &typ, &source, &subject, &eventTime, &dataJSON, &metadataJSON, &depth, &provenanceJSON)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("event %s not found: %w", eventID, sql.ErrNoRows)
	}
	if err != nil {
		return nil, fmt.Errorf("read event %s: %w", eventID, err)
	}
	e, err := decodeStoredEvent(id, typ, source, subject, eventTime, dataJSON, metadataJSON, depth, provenanceJSON)
	if err != nil {
		return nil, err
	}
	ins := &Inspection{Event: e}

	if ins.Decisions, err = d.eventDecisions(ctx, eventID); err != nil {
		return nil, err
	}
	if ins.Executions, err = d.eventExecutions(ctx, eventID); err != nil {
		return nil, err
	}
	if ins.Trace, err = d.eventTrace(ctx, eventID); err != nil {
		return nil, err
	}
	if ins.Label, err = d.eventLabel(ctx, eventID); err != nil {
		return nil, err
	}
	rows, err := d.QueryContext(ctx, `SELECT id FROM events WHERE json_extract(provenance, '$.parent_event_id') = ? ORDER BY ingest_time, id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read child events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read child event: %w", err)
		}
		ins.ChildEventIDs = append(ins.ChildEventIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read child events: %w", err)
	}
	return ins, nil
}

func (d *DB) eventDecisions(ctx context.Context, eventID string) ([]event.Decision, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, interpreter, model, status, result, latency_ms, error
		FROM decisions WHERE event_id = ? ORDER BY created_at, id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read decisions for %s: %w", eventID, err)
	}
	defer rows.Close()

	var out []event.Decision
	for rows.Next() {
		dec := event.Decision{EventID: eventID}
		var resultJSON, decErr sql.NullString
		if err := rows.Scan(&dec.ID, &dec.Interpreter, &dec.Model, &dec.Status, &resultJSON, &dec.LatencyMs, &decErr); err != nil {
			return nil, fmt.Errorf("scan decision for %s: %w", eventID, err)
		}
		dec.Error = decErr.String
		if resultJSON.Valid && resultJSON.String != "null" {
			if err := json.Unmarshal([]byte(resultJSON.String), &dec.Result); err != nil {
				return nil, fmt.Errorf("decode decision %s result: %w", dec.ID, err)
			}
		}
		out = append(out, dec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read decisions for %s: %w", eventID, err)
	}
	return out, nil
}

func (d *DB) eventExecutions(ctx context.Context, eventID string) ([]Execution, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, decision_id, outcome, adapter, status, error, created_at, policy_rule, policy_hash
		FROM executions WHERE event_id = ? ORDER BY created_at, id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read executions for %s: %w", eventID, err)
	}
	defer rows.Close()

	var out []Execution
	for rows.Next() {
		x := Execution{EventID: eventID}
		var decisionID, adapter, execErr sql.NullString
		var createdAt string
		if err := rows.Scan(&x.ID, &decisionID, &x.Outcome, &adapter, &x.Status, &execErr, &createdAt, &x.PolicyRule, &x.PolicyHash); err != nil {
			return nil, fmt.Errorf("scan execution for %s: %w", eventID, err)
		}
		x.DecisionID, x.Adapter, x.Error = decisionID.String, adapter.String, execErr.String
		at, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("decode execution %s time: %w", x.ID, err)
		}
		x.CreatedAt = at
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read executions for %s: %w", eventID, err)
	}
	return out, nil
}

func (d *DB) eventLabel(ctx context.Context, eventID string) (*EventLabel, error) {
	var (
		category, expectedOutcome, labeledAt string
		requiresAction                       bool
	)
	err := d.QueryRowContext(ctx, `SELECT category, requires_action, expected_outcome, labeled_at
		FROM event_labels WHERE event_id = ?`, eventID).
		Scan(&category, &requiresAction, &expectedOutcome, &labeledAt)
	if err == sql.ErrNoRows {
		return nil, nil // unlabeled is the common case, not an error
	}
	if err != nil {
		return nil, fmt.Errorf("read label for %s: %w", eventID, err)
	}
	at, err := time.Parse(time.RFC3339Nano, labeledAt)
	if err != nil {
		return nil, fmt.Errorf("decode label for event %s: %w", eventID, err)
	}
	return &EventLabel{
		EventID: eventID, Category: category, RequiresAction: requiresAction,
		ExpectedOutcome: expectedOutcome, LabeledAt: at,
	}, nil
}
