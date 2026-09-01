// These tests pin the events handler, the runtime's only ingress: every way
// untrusted input is rejected (missing type/source, malformed JSON, oversized
// bodies) must fail with a 400 and {"error": ...} before the pipeline —
// SQLite, model call, policy — is ever touched, and a valid event must come
// back as a 200 whose body is the full trace of what happened. The handler
// is exercised directly with a recorder instead of booting the server so a
// failure points at ingress logic, not at the run loop.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
	"github.com/heath0xff/mrmr/internal/runtime"
	"github.com/heath0xff/mrmr/internal/storage"
)

// modelServer serves canned chat-completion contents over httptest, so
// these tests need no network and no real model. The call count lets the
// rejection cases assert the pipeline was never reached.
func modelServer(t *testing.T, contents ...string) (url string, calls *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		content := contents[len(contents)-1]
		if i < len(contents) {
			content = contents[i]
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":`+fmt.Sprintf("%q", content)+`}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

// newTestHandler wires a real runtime over a temp-dir SQLite DB and a mock
// model endpoint. The schema and policy mirror the runtime tests: one
// classification contract and a single importance threshold routing to
// stdout, so a canned "important" answer deterministically produces notify.
func newTestHandler(t *testing.T, contents ...string) (http.HandlerFunc, *atomic.Int32) {
	t.Helper()
	url, calls := modelServer(t, contents...)
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	rt := &runtime.Runtime{
		DB:       db,
		Client:   &model.Client{},
		ModelCfg: model.Config{Provider: "openai-compatible", BaseURL: url, Model: "mock-model"},
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
	return eventsHandler(rt), calls
}

// handlerResponse mirrors just the response fields the handler contract
// promises, so the assertions stay about the wire shape and not the
// runtime's internal structs.
type handlerResponse struct {
	EventID  string `json:"event_id"`
	Outcome  string `json:"outcome"`
	Decision *struct {
		Status string         `json:"status"`
		Result map[string]any `json:"result"`
	} `json:"decision"`
}

func post(t *testing.T, h http.HandlerFunc, contentType string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/events", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestEventsHandlerValidEvent(t *testing.T) {
	h, calls := newTestHandler(t, `{"category":"important","importance":0.95}`)

	rec := post(t, h, "application/json", `{"type":"commit.pushed","source":"github","data":{"msg":"fix bug"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp handlerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.EventID == "" {
		t.Error("event_id missing from response body")
	}
	if resp.Outcome != "notify" {
		t.Errorf("outcome = %q, want notify (importance 0.95 > 0.8)", resp.Outcome)
	}
	if resp.Decision == nil || resp.Decision.Status != "ok" {
		t.Fatalf("decision = %+v, want an ok decision in the body", resp.Decision)
	}
	if resp.Decision.Result["importance"] != 0.95 {
		t.Errorf("decision result = %v, want the model's validated result", resp.Decision.Result)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("model called %d times, want 1", n)
	}
}

func TestEventsHandlerRejectsBadRequests(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "missing type",
			body: `{"source":"github"}`,
		},
		{
			name: "missing source",
			body: `{"type":"commit.pushed"}`,
		},
		{
			name: "malformed JSON",
			body: `{"type": "commit.pushed", "source":`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, calls := newTestHandler(t, `{"category":"important","importance":0.95}`)
			rec := post(t, h, "application/json", tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); !strings.Contains(got, `"error"`) {
				t.Errorf("body = %s, want an {\"error\": ...} envelope", got)
			}
			// Rejection must happen before the pipeline: nothing persisted
			// and, as the cheapest observable proxy, no model call.
			if n := calls.Load(); n != 0 {
				t.Errorf("model called %d times, want 0 (rejected input must not reach the pipeline)", n)
			}
		})
	}
}

func TestEventsHandlerRejectsOversizedBody(t *testing.T) {
	h, calls := newTestHandler(t, `{"category":"important","importance":0.95}`)
	// The body is valid JSON but padded past the 1 MiB ingress bound; the
	// limit must trip before the event is constructed or anything persists.
	pad := strings.Repeat("a", 1<<20+64)
	rec := post(t, h, "application/json", `{"type":"t","source":"s","data":{"pad":"`+pad+`"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for body over the 1 MiB bound", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, `"error"`) {
		t.Errorf("body = %s, want an {\"error\": ...} envelope", got)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("model called %d times, want 0 (oversized input must not reach the pipeline)", n)
	}
}
