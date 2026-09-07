// These tests pin trace durability. The trace returned to whoever posted an
// event is gone the moment that caller drops it; the stored copy is what
// answers "why did this happen?" a week later, so it must match the returned
// trace step for step and must exist for the short-circuit paths too — a
// filter exclusion is precisely the case someone comes back to inspect.
package runtime

import (
	"context"
	"testing"

	"github.com/heath0xff/mrmr/internal/filter"
	"github.com/heath0xff/mrmr/internal/policy"
)

func TestIngestPersistsTraceForInspection(t *testing.T) {
	url, _ := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)

	resp, err := rt.Ingest(context.Background(), testEvent("gh-trace"))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	stored, err := rt.DB.EventTrace(resp.EventID)
	if err != nil {
		t.Fatalf("EventTrace: %v", err)
	}
	if len(stored) != len(resp.Trace) {
		t.Fatalf("stored trace has %d steps, want %d (the returned trace)", len(stored), len(resp.Trace))
	}
	for i := range resp.Trace {
		if stored[i].Stage != resp.Trace[i].Stage || stored[i].Msg != resp.Trace[i].Msg {
			t.Fatalf("stored step %d = %+v, want %+v", i, stored[i], resp.Trace[i])
		}
	}
}

func TestIngestPersistsTraceForFilterExcludedEvent(t *testing.T) {
	url, calls := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)
	rt.Filters = filter.List{{Field: "source", Op: filter.OpNeq, Value: "github"}}

	resp, err := rt.Ingest(context.Background(), testEvent("gh-filtered"))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("model called %d times, want 0 (filter must short-circuit)", n)
	}
	stored, err := rt.DB.EventTrace(resp.EventID)
	if err != nil {
		t.Fatalf("EventTrace: %v", err)
	}
	if len(stored) != len(resp.Trace) {
		t.Fatalf("stored trace has %d steps, want %d", len(stored), len(resp.Trace))
	}
	var sawFilter bool
	for _, s := range stored {
		if s.Stage == "filter" {
			sawFilter = true
		}
	}
	if !sawFilter {
		t.Errorf("stored trace = %+v, want a filter step explaining the exclusion", stored)
	}
}

func TestIngestDuplicateStoresNoTrace(t *testing.T) {
	url, _ := modelServer(t, `{"category":"important","importance":0.9}`)
	rt := newTestRuntime(t, url)

	if _, err := rt.Ingest(context.Background(), testEvent("gh-dup-trace")); err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	// The duplicate's event ID was never stored, so there is nothing for a
	// trace to hang off; the original event's trace is the explanation.
	dup := testEvent("gh-dup-trace")
	resp, err := rt.Ingest(context.Background(), dup)
	if err != nil {
		t.Fatalf("second Ingest: %v", err)
	}
	if !resp.Duplicate {
		t.Fatal("second ingest of the same source_event_id must be a duplicate")
	}
	stored, err := rt.DB.EventTrace(dup.ID)
	if err != nil {
		t.Fatalf("EventTrace: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("duplicate stored %d trace steps, want 0", len(stored))
	}
}

func TestIngestEmitStoresTraceForEveryEventInTheChain(t *testing.T) {
	// An emitted child runs the pipeline itself, so it gets its own trace
	// under its own event id — including the last child, which the depth cap
	// records without interpreting. If any hop stored no trace, an emitted
	// chain would be unexplainable past the first one.
	url, _ := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)
	rt.Policy = policy.Policy{
		Rules: []policy.Rule{{
			If:   map[string]any{"result.importance": "> 0.8"},
			Then: policy.Then{Action: &policy.Action{Type: "emit", EventType: "chain.child"}},
		}},
		Default: policy.Then{Ignore: true},
	}

	if _, err := rt.Ingest(context.Background(), testEvent("gh-emit-trace")); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	var untraced int
	if err := rt.DB.QueryRow(`SELECT COUNT(*) FROM events e
		WHERE NOT EXISTS (SELECT 1 FROM event_traces t WHERE t.event_id = e.id)`).Scan(&untraced); err != nil {
		t.Fatalf("count untraced events: %v", err)
	}
	if untraced != 0 {
		t.Errorf("%d stored events have no trace, want 0", untraced)
	}
}
