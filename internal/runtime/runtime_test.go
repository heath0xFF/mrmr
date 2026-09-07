// These tests pin the whole mrmr loop end to end against a mock
// OpenAI-compatible endpoint and a real SQLite database. The invariants
// that matter: a decision must be persisted for every interpreted
// non-duplicate event, invalid or errored interpretation must never reach
// policy (fail toward ignore), and duplicates and filter-excluded events
// must short-circuit before the model is called.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/filter"
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

// The filter tests pin the gate at the pipeline level: an excluded
// event never reaches the model, leaves no Decision, and leaves an
// audited Execution row with a NULL decision reference. The model
// server's call count is the assertion hook for "never reached the
// model".
func TestIngestFilterExcludesBeforeModel(t *testing.T) {
	url, calls := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)
	rt.Filters = filter.List{{Field: "data.author", Op: filter.OpNeq, Value: "dependabot[bot]"}}

	// The event fails the gate and carries a secret that must not
	// leak into the trace or the response.
	e := testEvent("gh-filter-1")
	e.Data = map[string]any{"author": "dependabot[bot]", "token": "super-secret"}

	resp, err := rt.Ingest(context.Background(), e)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if resp.Outcome != "ignore" {
		t.Errorf("outcome = %q, want ignore", resp.Outcome)
	}
	if resp.Decision != nil {
		t.Fatalf("decision = %+v, want none: the model never ran", resp.Decision)
	}
	hasStages(t, resp, "receive", "persist", "filter", "outcome")
	if calls.Load() != 0 {
		t.Errorf("model calls = %d, want 0: a filtered event must not reach the model", calls.Load())
	}
	// The failure reason is value-free, so neither the event payload
	// nor the configured value may appear anywhere in the response.
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	for _, leaked := range []string{"super-secret", "dependabot[bot]"} {
		if strings.Contains(string(b), leaked) {
			t.Errorf("response leaks %q: %s", leaked, b)
		}
	}

	var n int
	if err := rt.DB.QueryRow(`SELECT COUNT(*) FROM decisions WHERE event_id = ?`, e.ID).Scan(&n); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	if n != 0 {
		t.Errorf("decision rows = %d, want 0", n)
	}
	if err := rt.DB.QueryRow(`SELECT COUNT(*) FROM executions WHERE event_id = ?`, e.ID).Scan(&n); err != nil {
		t.Fatalf("count executions: %v", err)
	}
	if n != 1 {
		t.Fatalf("execution rows = %d, want 1: the audit trail must show where the event stopped", n)
	}
	var nullRef bool
	var outcome, adapter, status string
	if err := rt.DB.QueryRow(`SELECT decision_id IS NULL, outcome, adapter, status FROM executions WHERE event_id = ?`, e.ID).Scan(&nullRef, &outcome, &adapter, &status); err != nil {
		t.Fatalf("query executions: %v", err)
	}
	if !nullRef {
		t.Error("decision_id is not NULL: a filtered event has no decision")
	}
	if outcome != "ignore" || adapter != "" || status != "filtered" {
		t.Errorf("execution = (outcome %q, adapter %q, status %q), want (ignore, \"\", filtered)", outcome, adapter, status)
	}
}

func TestIngestFilterPassesToModel(t *testing.T) {
	url, calls := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)
	rt.Filters = filter.List{{Field: "data.author", Op: filter.OpNeq, Value: "dependabot[bot]"}}

	e := testEvent("gh-filter-2")
	e.Data = map[string]any{"author": "alice", "msg": "fix bug"}

	resp, err := rt.Ingest(context.Background(), e)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if resp.Outcome != "notify" {
		t.Errorf("outcome = %q, want notify", resp.Outcome)
	}
	hasStages(t, resp, "receive", "persist", "filter", "interpret", "policy", "outcome")
	if calls.Load() != 1 {
		t.Errorf("model calls = %d, want 1", calls.Load())
	}
}

// TestIngestFilterMissingFieldExcludes pins the fail-closed rule at the
// pipeline level: an event without the filtered field is excluded even
// under neq, so a malformed event cannot pass the gate by default.
func TestIngestFilterMissingFieldExcludes(t *testing.T) {
	url, calls := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)
	rt.Filters = filter.List{{Field: "data.author", Op: filter.OpNeq, Value: "dependabot[bot]"}}

	// testEvent carries data.msg but no data.author.
	resp, err := rt.Ingest(context.Background(), testEvent("gh-filter-3"))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if resp.Outcome != "ignore" {
		t.Errorf("outcome = %q, want ignore", resp.Outcome)
	}
	hasStages(t, resp, "receive", "persist", "filter", "outcome")
	if calls.Load() != 0 {
		t.Errorf("model calls = %d, want 0", calls.Load())
	}
}

// httpTarget is an httptest server standing in for an action or agent
// endpoint. It captures method and body and always answers 2xx.
func httpTarget(t *testing.T) (url string, method *string, body *string, status *int) {
	t.Helper()
	var m, b string
	var code int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		m, b, code = r.Method, string(raw), http.StatusOK
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &m, &b, &code
}

func TestIngestHTTPActionPostsDefaultBody(t *testing.T) {
	targetURL, gotMethod, gotBody, _ := httpTarget(t)
	url, _ := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)
	rt.Policy = policy.Policy{
		Rules: []policy.Rule{{
			If:   map[string]any{"result.importance": "> 0.8"},
			Then: policy.Then{Action: &policy.Action{Type: "http", URL: targetURL, Body: "{{ .result.importance }}"}},
		}},
		Default: policy.Then{Ignore: true},
	}

	resp, err := rt.Ingest(context.Background(), testEvent("gh-act-1"))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if resp.Outcome != "act" {
		t.Errorf("outcome = %q, want act", resp.Outcome)
	}
	if *gotMethod != http.MethodPost {
		t.Errorf("action method = %q, want POST default", *gotMethod)
	}
	if *gotBody != "0.95" {
		t.Errorf("action body = %q, want rendered template %q", *gotBody, "0.95")
	}
	hasStages(t, resp, "receive", "persist", "interpret", "policy", "outcome")
}

func TestIngestHTTPActionNon2xxIsError(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	url, _ := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)
	rt.Policy = policy.Policy{
		Rules: []policy.Rule{{
			If:   map[string]any{"result.importance": "> 0.8"},
			Then: policy.Then{Action: &policy.Action{Type: "http", URL: broken.URL}},
		}},
		Default: policy.Then{Ignore: true},
	}

	resp, err := rt.Ingest(context.Background(), testEvent("gh-act-2"))
	if err != nil {
		t.Fatalf("Ingest: %v", err) // an action failure is recorded, not a 500
	}
	last := resp.Trace[len(resp.Trace)-1]
	if last.Msg != "error: http POST "+broken.URL+": status 500" {
		t.Errorf("last trace = %+v, want the action's status error", last)
	}
}

func TestIngestDelegatePostsAgentPayload(t *testing.T) {
	agentURL, _, gotBody, _ := httpTarget(t)
	url, _ := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)
	rt.AgentEndpoints = map[string]string{"triage": agentURL}
	rt.Policy = policy.Policy{
		Rules: []policy.Rule{{
			If:   map[string]any{"result.importance": "> 0.8"},
			Then: policy.Then{Delegate: &policy.Delegate{Agent: "triage", Prompt: "investigate this"}},
		}},
		Default: policy.Then{Ignore: true},
	}

	resp, err := rt.Ingest(context.Background(), testEvent("gh-del-1"))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if resp.Outcome != "delegate" {
		t.Errorf("outcome = %q, want delegate", resp.Outcome)
	}
	var payload struct {
		EventID  string         `json:"event_id"`
		Decision map[string]any `json:"decision"`
		Prompt   string         `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(*gotBody), &payload); err != nil {
		t.Fatalf("agent payload = %q, not valid JSON: %v", *gotBody, err)
	}
	if payload.EventID != resp.EventID || payload.Prompt != "investigate this" || payload.Decision["importance"] != 0.95 {
		t.Errorf("agent payload = %+v, want event id, prompt, and decision", payload)
	}
}

func TestIngestShadowRecordsButDoesNotExecute(t *testing.T) {
	url, _ := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)
	rt.Policy = policy.Policy{
		Rules: []policy.Rule{{
			If:   map[string]any{"result.importance": "> 0.8"},
			Then: policy.Then{Notify: &policy.Notify{Via: "stdout"}, Shadow: true},
		}},
		Default: policy.Then{Ignore: true},
	}

	resp, err := rt.Ingest(context.Background(), testEvent("gh-shadow-1"))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// The outcome is decided and recorded as if it fired...
	if resp.Outcome != "notify" {
		t.Errorf("outcome = %q, want notify (recorded)", resp.Outcome)
	}
	last := resp.Trace[len(resp.Trace)-1]
	if last.Msg != "shadow: notify recorded but not executed" {
		t.Errorf("last trace = %+v, want the shadow marker", last)
	}
	// ...and the execution row must say so, never "ok".
	row := rt.DB.QueryRow("SELECT status FROM executions WHERE event_id = ?", resp.EventID)
	var status string
	if err := row.Scan(&status); err != nil {
		t.Fatalf("query execution: %v", err)
	}
	if status != "shadow" {
		t.Errorf("execution status = %q, want shadow", status)
	}
}

// TestIngestEmitChainDepthCaps drives the full recursion path: every high-
// importance interpretation emits a child, the child re-enters the pipeline
// and does the same, and the chain must stop at the depth cap with the last
// event recorded and forced to ignore — never an infinite model loop.
// Each child's own trace lives in its child response (not bubbled into the
// parent's), so the cap's durable evidence is asserted through SQLite.
func TestIngestEmitChainDepthCaps(t *testing.T) {
	url, calls := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)
	rt.Policy = policy.Policy{
		Rules: []policy.Rule{{
			If:   map[string]any{"result.importance": "> 0.8"},
			Then: policy.Then{Action: &policy.Action{Type: "emit", EventType: "chain.child"}},
		}},
		Default: policy.Then{Ignore: true},
	}

	resp, err := rt.Ingest(context.Background(), testEvent("gh-emit-1"))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if resp.Outcome != "act" {
		t.Errorf("outcome = %q, want act (the emit itself)", resp.Outcome)
	}
	// The parent trace shows its own single emit; the chain's depth lives
	// in the database rows, asserted below.
	hasStages(t, resp, "receive", "persist", "interpret", "policy", "emit", "emit", "outcome")

	// Children at depths 1..maxDepth were emitted, plus the one event at
	// maxDepth+1 that the cap recorded without interpreting. The model ran
	// once per interpreted event: depths 0..maxDepth.
	var emitted int
	if err := rt.DB.QueryRow(`SELECT COUNT(*) FROM events WHERE source = 'mrmr'`).Scan(&emitted); err != nil {
		t.Fatalf("count emitted: %v", err)
	}
	if emitted != maxDepth+1 {
		t.Errorf("emitted events = %d, want %d", emitted, maxDepth+1)
	}
	var capped int
	if err := rt.DB.QueryRow(`SELECT COUNT(*) FROM executions WHERE error = 'max causal depth exceeded'`).Scan(&capped); err != nil {
		t.Fatalf("count capped executions: %v", err)
	}
	if capped != 1 {
		t.Errorf("depth-capped executions = %d, want 1", capped)
	}
	if calls.Load() != int32(maxDepth+1) {
		t.Errorf("model calls = %d, want %d (depths 0..%d)", calls.Load(), maxDepth+1, maxDepth)
	}
}

func TestIngestOverDepthCapForcedIgnore(t *testing.T) {
	url, calls := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)

	e := testEvent("gh-depth-1")
	e.Depth = maxDepth + 1
	resp, err := rt.Ingest(context.Background(), e)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if resp.Outcome != "ignore" {
		t.Errorf("outcome = %q, want ignore", resp.Outcome)
	}
	hasStages(t, resp, "receive", "persist", "depth", "outcome")
	if calls.Load() != 0 {
		t.Errorf("model calls = %d, want 0 past the cap", calls.Load())
	}
}

// TestIngestTraceWriteFailureStillSucceeds pins the fate-known rule: the
// event, decision, and execution rows are durable and the outcome has fired
// by the time the trace is written, so losing the durable trace must not
// turn into a 500 that would claim "fate unknown" and trigger a pointless
// retry (which would dedup-drop).
func TestIngestTraceWriteFailureStillSucceeds(t *testing.T) {
	url, _ := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)
	if _, err := rt.DB.Exec(`DROP TABLE event_traces`); err != nil {
		t.Fatalf("drop event_traces: %v", err)
	}

	resp, err := rt.Ingest(context.Background(), testEvent("gh-tracefail-1"))
	if err != nil {
		t.Fatalf("Ingest = err %v, want success with degraded trace", err)
	}
	if resp.Outcome != "notify" {
		t.Errorf("outcome = %q, want notify (already executed)", resp.Outcome)
	}
	if len(resp.Trace) == 0 {
		t.Error("response trace is empty; the in-memory trace must still go out")
	}
}

// TestIngestExecutionWriteFailureStillSucceeds covers the same rule on the
// execution row: the side effect has fired by the time the row is written,
// so the request must report the outcome, not fail it.
func TestIngestExecutionWriteFailureStillSucceeds(t *testing.T) {
	url, _ := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)
	if _, err := rt.DB.Exec(`DROP TABLE executions`); err != nil {
		t.Fatalf("drop executions: %v", err)
	}

	resp, err := rt.Ingest(context.Background(), testEvent("gh-exefail-1"))
	if err != nil {
		t.Fatalf("Ingest = err %v, want success with degraded audit row", err)
	}
	if resp.Outcome != "notify" {
		t.Errorf("outcome = %q, want notify (the side effect fired)", resp.Outcome)
	}
}
