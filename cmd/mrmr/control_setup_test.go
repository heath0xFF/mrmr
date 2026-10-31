package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const setupYAML = `server:
  addr: 127.0.0.1:4242
db:
  path: test.db
models:
  judge:
    provider: openai-compatible
    base_url: MODEL_URL
    model: setup-test-model
interpret:
  model: judge
  questions:
    category:
      type: choice
      instructions: Classify this service event.
      criteria:
        incident: A service is broken.
        noise: Routine activity.
    requires_action:
      type: noul
      instructions: Does the operator need to act?
    severity:
      type: score
      instructions: How severe is the impact?
      criteria: [No impact, Degraded, Unavailable]
sources:
  - name: service-monitor
    type: systemd-journal
    units: [synthetic.service]
    event_type: service.warning
    every: 30s
    priority: warning
    paused: true
policy:
  - if:
      result.category.choice: incident
      result.requires_action.value: true
    then:
      notify:
        via: stdout
      shadow: true
default:
  ignore: true
`

func syntheticModel(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/chat/completions" {
			t.Error("unexpected model request path")
		}
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "synthetic-setup") || strings.Contains(string(b), "PRIVATE-EVENT") {
			t.Error("setup check did not use only synthetic input")
		}
		answer := `{"category":"incident","requires_action":true,"severity":2}`
		if strings.Contains(string(b), "Scheduled maintenance completed") {
			answer = `{"category":"noise","requires_action":false,"severity":0}`
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": answer}}}})
	}))
}

func TestManagementSetupTestIsEffectFree(t *testing.T) {
	m := testManager(t)
	controlEvent(t, m)
	m.db.Exec(`UPDATE events SET data='{"message":"PRIVATE-EVENT"}'`)
	var calls, effects atomic.Int32
	model := syntheticModel(t, &calls)
	defer model.Close()
	adapter := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { effects.Add(1) }))
	defer adapter.Close()
	candidate := strings.ReplaceAll(setupYAML, "MODEL_URL", model.URL)
	// Even a live action in a candidate cannot cross the evaluator boundary.
	candidate = strings.Replace(candidate, "notify:\n        via: stdout\n      shadow: true", "action:\n        type: http\n        url: "+adapter.URL, 1)
	w := controlRequest(m, "POST", "/manage/api/test-model", map[string]any{"yaml": candidate, "confirm": true})
	if w.Code != 200 {
		t.Fatalf("test model: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Ready    bool        `json:"ready"`
		Revision string      `json:"revision"`
		Report   evalSummary `json:"report"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Ready || result.Revision != revision(candidate) || result.Report.ValidDecisions != 2 || calls.Load() != 2 || effects.Load() != 0 {
		t.Fatalf("result=%+v calls=%d effects=%d", result, calls.Load(), effects.Load())
	}
	if result.Report.OutcomeCorrect != 1 {
		t.Fatal("connection success hid a policy mismatch")
	}
	for _, table := range []string{"decisions", "executions", "source_cursors", "control_draft", "control_evaluations"} {
		var count int
		if err := m.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("test mutated %s: count=%d err=%v", table, count, err)
		}
	}
	var count int
	m.db.QueryRow(`SELECT count(*) FROM events`).Scan(&count)
	if count != 1 || m.current.Load().yaml != controlYAML {
		t.Fatal("test changed operational events or active configuration")
	}
	b, _ := os.ReadFile(m.path)
	if string(b) != controlYAML {
		t.Fatal("test changed startup file")
	}
	m.db.QueryRow(`SELECT count(*) FROM control_audit WHERE operation='test-model'`).Scan(&count)
	if count != 2 {
		t.Fatal("test intent/result not audited")
	}
}

func TestManagementSetupTestRejectsBeforeNetwork(t *testing.T) {
	m := testManager(t)
	t.Setenv("MRMR_SETUP_MISSING_KEY", "")
	var calls atomic.Int32
	srv := syntheticModel(t, &calls)
	defer srv.Close()
	candidate := strings.ReplaceAll(setupYAML, "MODEL_URL", srv.URL)
	for _, tc := range []struct {
		name, yaml string
		confirm    bool
	}{
		{"consent", candidate, false},
		{"invalid", "invalid: [", true},
		{"placeholder", strings.Replace(candidate, "setup-test-model", "replace-with-your-model", 1), true},
		{"invalid-host", strings.Replace(candidate, srv.URL, "https://example.invalid/v1", 1), true},
		{"missing-key", strings.Replace(candidate, "model: setup-test-model", "model: setup-test-model\n    api_key_env: MRMR_SETUP_MISSING_KEY", 1), true},
		{"query", strings.Replace(candidate, srv.URL, srv.URL+"?key=oops", 1), true},
		{"non-http", strings.Replace(candidate, srv.URL, "file:///private", 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := controlRequest(m, "POST", "/manage/api/test-model", map[string]any{"yaml": tc.yaml, "confirm": tc.confirm})
			if w.Code != 400 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid setup made a model call")
	}
	// Server-side activation protection also covers clients bypassing the guide.
	placeholder := strings.Replace(controlYAML, "test-model", "replace-with-your-model", 1)
	req := saveControlDraft(t, m, placeholder)
	if w := controlRequest(m, "POST", "/manage/api/activate", req); w.Code == 200 || !strings.Contains(w.Body.String(), "example model") {
		t.Fatalf("placeholder activated: %d %s", w.Code, w.Body.String())
	}
}

func TestManagementSetupTestBeforeFirstActivation(t *testing.T) {
	m := testManager(t)
	m.current.Store(nil)
	if err := os.Remove(m.path); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv := syntheticModel(t, &calls)
	defer srv.Close()
	candidate := strings.ReplaceAll(setupYAML, "MODEL_URL", srv.URL)
	// Unpaused here proves that testing neither starts nor prepares a journal.
	candidate = strings.Replace(candidate, "    paused: true\n", "", 1)
	w := controlRequest(m, "POST", "/manage/api/test-model", map[string]any{"yaml": candidate, "confirm": true})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ready":true`) || calls.Load() != 2 {
		t.Fatalf("setup test: %d %s", w.Code, w.Body.String())
	}
	if m.current.Load() != nil {
		t.Fatal("model test activated first-run setup")
	}
	if _, err := os.Stat(m.path); !os.IsNotExist(err) {
		t.Fatal("model test created a startup file")
	}
}

func TestManagementSetupTestFailureDoesNotLeak(t *testing.T) {
	m := testManager(t)
	const secret = "test-only-never-return-this-credential"
	t.Setenv("MRMR_SETUP_TEST_KEY", secret)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("configured credential was not used")
		}
		w.WriteHeader(401)
		io.WriteString(w, secret)
	}))
	defer srv.Close()
	candidate := strings.ReplaceAll(setupYAML, "MODEL_URL", srv.URL)
	candidate = strings.Replace(candidate, "model: setup-test-model", "model: setup-test-model\n    api_key_env: MRMR_SETUP_TEST_KEY", 1)
	w := controlRequest(m, "POST", "/manage/api/test-model", map[string]any{"yaml": candidate, "confirm": true})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ready":false`) || strings.Contains(w.Body.String(), secret) || calls.Load() != 2 {
		t.Fatalf("failure result: status=%d calls=%d", w.Code, calls.Load())
	}
}

func TestManagementSetupTestCancellation(t *testing.T) {
	m := testManager(t)
	entered := make(chan struct{}, 1)
	stopped := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done()
		stopped <- struct{}{}
	}))
	defer srv.Close()
	candidate := strings.ReplaceAll(setupYAML, "MODEL_URL", srv.URL)
	b, _ := json.Marshal(map[string]any{"yaml": candidate, "confirm": true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "http://localhost/manage/api/test-model", strings.NewReader(string(b))).WithContext(ctx)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer "+testControlToken)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Mrmr-Control", "1")
	done := make(chan struct{})
	go func() { defer close(done); m.ServeHTTP(httptest.NewRecorder(), r) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("model request did not start")
	}
	cancel()
	for _, ch := range []<-chan struct{}{stopped, done} {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("request survived cancellation")
		}
	}
}
