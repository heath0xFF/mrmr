// These tests pin storage's durability and dedup contracts. Dedup is the
// runtime's exactly-once guarantee: a redelivered webhook must not produce
// a second decision, so the (source, dedup_key) uniqueness — via
// source_event_id or payload hash — is behavior users depend on, not an
// implementation detail. Migration idempotence matters because every
// process restart reopens the same database file.
package storage

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/event"
)

func testEvent(sourceEventID string, data map[string]any) event.Event {
	e := event.Event{
		ID:        event.NewID("evt_"),
		Type:      "commit.pushed",
		Source:    "github",
		Subject:   "mrmr",
		Timestamp: time.Now().UTC(),
		Data:      data,
	}
	if sourceEventID != "" {
		e.Metadata = map[string]any{"source_event_id": sourceEventID}
	}
	return e
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestInsertEventDuplicateBySourceEventID(t *testing.T) {
	d := openTestDB(t)
	first := testEvent("gh-123", map[string]any{"sha": "abc"})
	rec, err := d.InsertEvent(first)
	if err != nil {
		t.Fatalf("insert first: %v", err)
	}
	if rec.Duplicate {
		t.Error("first insert must not be a duplicate")
	}

	// A second event carrying the same source_event_id — as a webhook
	// redelivery would — is the same upstream occurrence even though the
	// mrmr-side event ID differs.
	second := testEvent("gh-123", map[string]any{"sha": "abc"})
	rec, err = d.InsertEvent(second)
	if err != nil {
		t.Fatalf("insert second: %v", err)
	}
	if !rec.Duplicate {
		t.Error("same source_event_id must be detected as duplicate")
	}
}

func TestInsertEventDuplicateByPayloadHash(t *testing.T) {
	d := openTestDB(t)
	// No source_event_id: dedup falls back to hashing type + data.
	// Identical payloads from the same source are treated as one event.
	a := testEvent("", map[string]any{"sha": "abc", "msg": "fix"})
	b := testEvent("", map[string]any{"sha": "abc", "msg": "fix"})
	if rec, err := d.InsertEvent(a); err != nil || rec.Duplicate {
		t.Fatalf("first insert: rec=%v err=%v, want non-duplicate", rec, err)
	}
	rec, err := d.InsertEvent(b)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if !rec.Duplicate {
		t.Error("same payload with no source_event_id must be detected as duplicate")
	}

	// Different data hashes differently, so distinct events pass through.
	c := testEvent("", map[string]any{"sha": "def", "msg": "other"})
	rec, err = d.InsertEvent(c)
	if err != nil {
		t.Fatalf("third insert: %v", err)
	}
	if rec.Duplicate {
		t.Error("different payload must not be a duplicate")
	}
}

// Malformed optional IDs are absent identities, not shared synthetic keys.
// Hash fallback must still suppress actual redeliveries of the same payload.
func TestInvalidSourceEventIDFallsBackToPayload(t *testing.T) {
	for _, id := range []any{nil, "", true, []any{"id"}, map[string]any{"id": "x"}} {
		d := openTestDB(t)
		for i, payload := range []string{"first", "second", "second"} {
			e := testEvent("", map[string]any{"message": payload})
			e.Metadata = map[string]any{"source_event_id": id}
			rec, err := d.InsertEvent(e)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Duplicate != (i == 2) {
				t.Fatalf("id=%#v payload=%q duplicate=%v", id, payload, rec.Duplicate)
			}
		}
	}
}

// Every read path feeds inspection or export. None may undo exact ingestion
// by silently rounding a numeric identity on the way back out of SQLite.
func TestEventNumbersSurviveStorageReads(t *testing.T) {
	d := openTestDB(t)
	const id = json.Number("9007199254740993")
	e := testEvent("", map[string]any{"id": id})
	e.Metadata = map[string]any{"source_event_id": id}
	if _, err := d.InsertEvent(e); err != nil {
		t.Fatal(err)
	}
	if _, err := d.LabelEvent(e.ID, "important", true, "notify"); err != nil {
		t.Fatal(err)
	}
	ins, err := d.Inspect(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := d.ListEvents(10, false)
	if err != nil {
		t.Fatal(err)
	}
	labeled, err := d.LabeledEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || len(labeled) != 1 {
		t.Fatal("stored event missing from review/export")
	}
	for _, got := range []event.Event{ins.Event, list[0].Event, labeled[0].Event} {
		if got.Data["id"] != id || got.Metadata["source_event_id"] != id {
			t.Fatalf("numeric identity changed: %+v", got)
		}
		got.ID = event.NewID("evt_")
		if rec, err := d.InsertEvent(got); err != nil || !rec.Duplicate {
			t.Fatalf("round-tripped event did not deduplicate: rec=%+v err=%v", rec, err)
		}
	}
}

func TestDecisionRoundTrip(t *testing.T) {
	d := openTestDB(t)
	ev, err := d.InsertEvent(testEvent("gh-1", nil))
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	dec := event.Decision{
		ID:          event.NewID("dec_"),
		EventID:     ev.Event.ID,
		Interpreter: "local",
		Model:       "mock-model",
		Status:      "ok",
		Result:      map[string]any{"category": "important", "importance": 0.9},
		LatencyMs:   42,
	}
	if err := d.InsertDecision(dec); err != nil {
		t.Fatalf("insert decision: %v", err)
	}

	var (
		resultJSON, status, model, interpreter string
		latency                                int64
	)
	err = d.QueryRow(`SELECT interpreter, model, status, result, latency_ms FROM decisions WHERE id = ?`, dec.ID).
		Scan(&interpreter, &model, &status, &resultJSON, &latency)
	if err != nil {
		t.Fatalf("query decision: %v", err)
	}
	if interpreter != dec.Interpreter || model != dec.Model || status != "ok" || latency != 42 {
		t.Errorf("round trip got interpreter=%q model=%q status=%q latency=%d, want original", interpreter, model, status, latency)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result["category"] != "important" || result["importance"] != 0.9 {
		t.Errorf("result = %v, want marshaled decision result back", result)
	}
}

func TestExecutionRoundTrip(t *testing.T) {
	d := openTestDB(t)
	ev, err := d.InsertEvent(testEvent("gh-2", nil))
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	// executions.decision_id carries a foreign key, so the decision row
	// must exist first — the audit chain events → decisions → executions
	// is enforced by the schema, not by convention.
	dec := event.Decision{
		ID: "dec_1", EventID: ev.Event.ID, Interpreter: "mock", Model: "mock-model",
		Status: "ok", Result: map[string]any{"importance": 0.9}, LatencyMs: 42,
	}
	if err := d.InsertDecision(dec); err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	if err := d.InsertExecution("exe_1", ev.Event.ID, "dec_1", "notify", "stdout", "ok", ""); err != nil {
		t.Fatalf("insert execution: %v", err)
	}
	var eventID, decisionID, outcome, adapter, status, execErr string
	err = d.QueryRow(`SELECT event_id, decision_id, outcome, adapter, status, error FROM executions WHERE id = ?`, "exe_1").
		Scan(&eventID, &decisionID, &outcome, &adapter, &status, &execErr)
	if err != nil {
		t.Fatalf("query execution: %v", err)
	}
	if eventID != ev.Event.ID || decisionID != "dec_1" || outcome != "notify" || adapter != "stdout" || status != "ok" || execErr != "" {
		t.Errorf("round trip got eventID=%q dec=%q outcome=%q adapter=%q status=%q err=%q, want original values",
			eventID, decisionID, outcome, adapter, status, execErr)
	}
}

func TestMigrationsIdempotentOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	d1.Close()

	// Reopening the same file — what every process restart does — must
	// not re-apply migrations or fail. Version bookkeeping in
	// schema_migrations makes the second open a no-op.
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open of migrated DB: %v", err)
	}
	defer d2.Close()

	var applied int
	if err := d2.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != len(migrations) {
		t.Errorf("applied migrations = %d, want %d (each exactly once)", applied, len(migrations))
	}
}
