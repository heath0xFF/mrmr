// These tests pin the whole mrmr loop end to end against a mock
// OpenAI-compatible endpoint and a real SQLite database. The invariants
// that matter: a decision must be persisted for every non-duplicate event,
// invalid or errored interpretation must never reach policy (fail toward
// ignore), and duplicates must short-circuit before the model is called.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
	"github.com/heath0xff/mrmr/internal/storage"
)

// fail500 marks a model server response as an internal server error
// instead of content, letting one canned-sequence server cover both the
// content and the transport-failure cases.
const fail500 = "\x00http500"

func testEvent(sourceEventID string) event.Event {
	e := event.Event{
		ID:        event.NewID("evt_"),
		Type:      "commit.pushed",
		Source:    "github",
		Subject:   "mrmr",
		Timestamp: time.Now().UTC(),
		Data:      map[string]any{"msg": "fix bug"},
	}
	if sourceEventID != "" {
		e.Metadata = map[string]any{"source_event_id": sourceEventID}
	}
	return e
}

// modelServer serves canned assistant contents in request order, repeating
// the last one if the client asks more times than expected. The call count
// doubles as an assertion hook: duplicates must never increment it.
func modelServer(t *testing.T, contents ...string) (url string, calls *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		content := contents[len(contents)-1]
		if i < len(contents) {
			content = contents[i]
		}
		if content == fail500 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "model down")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

func newTestRuntime(t *testing.T, modelURL string) *Runtime {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Minimal interpretation contract: classify and score the event, with
	// the enum so bad categories become schema violations, and a single
	// threshold rule routing high-importance events to stdout.
	return &Runtime{
		DB:     db,
		Client: &model.Client{},
		ModelCfg: model.Config{
			Provider: "openai-compatible",
			BaseURL:  modelURL,
			Model:    "mock-model",
		},
		ModelKey: "mock",
		Prompt:   "classify this event",
		Schema: model.Schema{
			"category":   {Type: "string", Enum: []any{"important", "unimportant"}},
			"importance": {Type: "number"},
		},
		Policy: policy.Policy{
			Rules: []policy.Rule{{
				If:   map[string]any{"result.importance": "> 0.8"},
				Then: policy.Then{Notify: &policy.Notify{Via: "stdout"}},
			}},
			Default: policy.Then{Ignore: true},
		},
	}
}

func stages(resp *Response) []string {
	out := make([]string, len(resp.Trace))
	for i, s := range resp.Trace {
		out[i] = s.Stage
	}
	return out
}

func hasStages(t *testing.T, resp *Response, want ...string) {
	t.Helper()
	got := stages(resp)
	if len(got) != len(want) {
		t.Fatalf("trace stages = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("trace stages = %v, want %v", got, want)
		}
	}
}

func TestIngestImportantEventNotifies(t *testing.T) {
	url, _ := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)

	resp, err := rt.Ingest(context.Background(), testEvent("gh-1"))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if resp.Outcome != "notify" {
		t.Errorf("outcome = %q, want notify", resp.Outcome)
	}
	if resp.Decision == nil || resp.Decision.Status != "ok" {
		t.Fatalf("decision = %+v, want status ok", resp.Decision)
	}
	if resp.Decision.Result["importance"] != 0.95 {
		t.Errorf("decision result = %v, want the model's validated result", resp.Decision.Result)
	}
	// The full pipeline must be traceable in order: what was received,
	// stored, interpreted, matched, and executed.
	hasStages(t, resp, "receive", "persist", "interpret", "policy", "outcome")
}

func TestIngestUnimportantEventIgnores(t *testing.T) {
	url, _ := modelServer(t, `{"category":"unimportant","importance":0.2}`)
	rt := newTestRuntime(t, url)

	resp, err := rt.Ingest(context.Background(), testEvent("gh-2"))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if resp.Outcome != "ignore" {
		t.Errorf("outcome = %q, want ignore (below threshold)", resp.Outcome)
	}
	if resp.Decision == nil || resp.Decision.Status != "ok" {
		t.Fatalf("decision = %+v, want status ok", resp.Decision)
	}
	hasStages(t, resp, "receive", "persist", "interpret", "policy", "outcome")
}

func TestIngestDuplicateShortCircuitsBeforeModel(t *testing.T) {
	url, calls := modelServer(t, `{"category":"important","importance":0.9}`)
	rt := newTestRuntime(t, url)

	if _, err := rt.Ingest(context.Background(), testEvent("gh-dup")); err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	// Same source_event_id, fresh event ID — the shape of a webhook
	// redelivery. It must stop at dedup with no second decision.
	resp, err := rt.Ingest(context.Background(), testEvent("gh-dup"))
	if err != nil {
		t.Fatalf("second Ingest: %v", err)
	}
	if !resp.Duplicate {
		t.Error("second ingest of same source_event_id must be Duplicate")
	}
	if resp.Decision != nil {
		t.Errorf("duplicate must not produce a decision, got %+v", resp.Decision)
	}
	if resp.Outcome != "" {
		t.Errorf("duplicate must have no outcome, got %q", resp.Outcome)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("model called %d times, want 1 (duplicate must not reach the model)", n)
	}
	var decisions int
	if err := rt.DB.QueryRow(`SELECT COUNT(*) FROM decisions`).Scan(&decisions); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	if decisions != 1 {
		t.Errorf("stored decisions = %d, want 1 (no second decision for duplicate)", decisions)
	}
}

func TestIngestInvalidOutputTwiceFailsTowardIgnore(t *testing.T) {
	// Schema-invalid output on both the initial call and the self-
	// correction retry: the decision is recorded as invalid and the
	// outcome is ignore. An uninterpretable event must never notify.
	url, _ := modelServer(t, `{"category":"urgent","importance":0.9}`, `garbage`)
	rt := newTestRuntime(t, url)

	resp, err := rt.Ingest(context.Background(), testEvent("gh-3"))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if resp.Decision == nil || resp.Decision.Status != "invalid" {
		t.Fatalf("decision = %+v, want status invalid", resp.Decision)
	}
	if resp.Decision.Error == "" {
		t.Error("invalid decision must carry the validation error")
	}
	if resp.Outcome != "ignore" {
		t.Errorf("outcome = %q, want ignore (invalid decisions never reach policy)", resp.Outcome)
	}
	hasStages(t, resp, "receive", "persist", "interpret", "policy", "outcome")
}

func TestIngestModelEndpointDownFailsTowardIgnore(t *testing.T) {
	// The endpoint 500s on every transport attempt: the decision is
	// recorded as errored and the outcome is ignore. Failures of the
	// model tier degrade to silence, never to a false notification.
	url, calls := modelServer(t, fail500)
	rt := newTestRuntime(t, url)

	resp, err := rt.Ingest(context.Background(), testEvent("gh-4"))
	if err != nil {
		t.Fatalf("Ingest: %v (model errors must not fail ingestion)", err)
	}
	if resp.Decision == nil || resp.Decision.Status != "errored" {
		t.Fatalf("decision = %+v, want status errored", resp.Decision)
	}
	if resp.Outcome != "ignore" {
		t.Errorf("outcome = %q, want ignore (errored decisions never reach policy)", resp.Outcome)
	}
	if n := calls.Load(); n == 0 {
		t.Error("model endpoint was never called")
	}
	hasStages(t, resp, "receive", "persist", "interpret", "policy", "outcome")
}
