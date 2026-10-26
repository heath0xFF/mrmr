package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/event"
)

func dashboardEvent(t *testing.T, db *DB, n int) string {
	t.Helper()
	id := fmt.Sprintf("evt_%04d", n)
	_, err := db.InsertEvent(event.Event{ID: id, Type: "service.warning", Source: "journal", Subject: "test.service", Timestamp: time.Now().UTC(), Data: map[string]any{"message": "Repeated warning"}, Metadata: map[string]any{"source_event_id": id}})
	if err != nil {
		t.Fatal(err)
	}
	// Fixed ingestion order makes pagination and the percentile window stable.
	if _, err := db.Exec(`UPDATE events SET ingest_time=? WHERE id=?`, time.Unix(int64(n), 0).UTC().Format(time.RFC3339Nano), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDashboardSummaryAndViews(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for i, status := range []string{"ok", "ok", "errored", "invalid"} {
		id := dashboardEvent(t, db, i)
		dec := event.Decision{ID: fmt.Sprintf("dec_%d", i), EventID: id, Interpreter: "judge", Model: "d1", Status: status, LatencyMs: []int64{10, 30, 90000, 0}[i]}
		if status == "errored" {
			dec.Error = "model call: i/o timeout"
		}
		if err := db.InsertDecision(dec); err != nil {
			t.Fatal(err)
		}
		if err := db.InsertExecution("exe_"+id, id, dec.ID, "notify", "discord", "shadow", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.LabelEvent("evt_0000", "routine", false, "ignore"); err != nil {
		t.Fatal(err)
	}
	// A second model with no successes must not invent a zero-ms percentile.
	id := dashboardEvent(t, db, 4)
	if err := db.InsertDecision(event.Decision{ID: "dec_other", EventID: id, Interpreter: "other", Model: "other", Status: "errored", Error: "connection refused", LatencyMs: 1}); err != nil {
		t.Fatal(err)
	}
	s, err := db.DashboardSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Events != 5 || s.Unlabeled != 4 || s.Shadow != 4 || len(s.Repeats) != 1 || s.Repeats[0].Count != 5 {
		t.Fatalf("summary: %+v", s)
	}
	for _, m := range s.Models {
		if m.Model == "d1" {
			if m.Count != 4 || m.Successes != 2 || m.Errors != 2 || m.Timeouts != 1 || *m.MedianMs != 10 || *m.P95Ms != 30 || m.LastSuccess == "" || m.LastError == "" {
				t.Fatalf("timing: %+v", m)
			}
		} else if m.MedianMs != nil || m.P95Ms != nil || m.Timeouts != 0 {
			t.Fatalf("no success: %+v", m)
		}
	}
	for view, want := range map[string]int{"all": 5, "unlabeled": 4, "shadow": 4, "ignore": 0, "errors": 3} {
		p, err := db.DashboardEvents(ctx, "", view, 0)
		if err != nil || len(p.Events) != want {
			t.Fatalf("view %s: %+v, %v", view, p, err)
		}
	}
	for query, want := range map[string]int{"TEST.SERVICE": 5, "Repeated": 5, "%' OR 1=1 --": 0} {
		p, err := db.DashboardEvents(ctx, query, "all", 0)
		if err != nil || len(p.Events) != want {
			t.Fatalf("query %q: %+v, %v", query, p, err)
		}
	}
}

func TestDashboardPaginationEmptyAndCancellation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s, err := db.DashboardSummary(ctx)
	if err != nil || s.Events != 0 || len(s.Models) != 0 || s.Models == nil {
		t.Fatalf("empty summary: %+v %v", s, err)
	}
	for i := 0; i < 52; i++ {
		dashboardEvent(t, db, i)
	}
	p, err := db.DashboardEvents(ctx, "", "all", 0)
	if err != nil || len(p.Events) != 50 || !p.More || p.Events[0].ID != "evt_0051" || p.Events[0].LatencyMs != nil {
		t.Fatalf("first page: %+v %v", p, err)
	}
	p, err = db.DashboardEvents(ctx, "", "all", 50)
	if err != nil || len(p.Events) != 2 || p.More || p.Events[0].ID != "evt_0001" {
		t.Fatalf("second page: %+v %v", p, err)
	}
	if _, err = db.DashboardEvents(ctx, "", "bad", 0); err == nil {
		t.Fatal("invalid view accepted")
	}
	if _, err = db.InspectContext(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing: %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = db.DashboardSummary(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("summary cancellation: %v", err)
	}
	if _, err = db.DashboardEvents(ctx, "", "all", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("events cancellation: %v", err)
	}
	if _, err = db.InspectContext(ctx, "evt_0000"); !errors.Is(err, context.Canceled) {
		t.Fatalf("detail cancellation: %v", err)
	}
}

func TestDashboardWindowAndIndexMigration(t *testing.T) {
	db := openTestDB(t)
	id := dashboardEvent(t, db, 0)
	// More than one decision per event is supported by storage. Use it to pin
	// the bound without generating a thousand unrelated event payloads.
	for i := 0; i <= DashboardWindow; i++ {
		at := time.Unix(int64(i), 0).UTC().Format(time.RFC3339Nano)
		if _, err := db.Exec(`INSERT INTO decisions(id,event_id,interpreter,model,status,latency_ms,created_at) VALUES(?,?,?,?,?,?,?)`, fmt.Sprint(i), id, "judge", "d1", "ok", i, at); err != nil {
			t.Fatal(err)
		}
	}
	s, err := db.DashboardSummary(context.Background())
	if err != nil || len(s.Models) != 1 || s.Models[0].Count != DashboardWindow || *s.Models[0].MedianMs != 500 || *s.Models[0].P95Ms != 950 {
		t.Fatalf("window: %+v %v", s, err)
	}
	// Force the second index creation to fail. The first must roll back, and
	// an existing event must survive the failed upgrade.
	if _, err := db.Exec(`DROP INDEX idx_events_ingest; DELETE FROM schema_migrations WHERE version>=8`); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(); err == nil {
		t.Fatal("conflicting migration should fail")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='idx_events_ingest'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial migration: %d %v", n, err)
	}
	if _, err := db.Inspect(id); err != nil {
		t.Fatal(err)
	}
}
