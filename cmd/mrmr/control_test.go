package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/storage"
)

const testControlToken = "test-only-operator-token-0123456789abcdef"
const controlYAML = `server:
  addr: 127.0.0.1:4242
db:
  path: test.db
models:
  judge:
    role: decision
    provider: openai-compatible
    base_url: http://127.0.0.1:1/v1
    model: test-model
interpret:
  model: judge
  prompt: Classify this event.
  schema:
    category:
      type: string
      enum: [routine, incident]
    requires_action:
      type: boolean
default:
  ignore: true
`

func testManager(t *testing.T) *manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	db, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mrmr.yaml")
	if err = os.WriteFile(path, []byte(controlYAML), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(controlYAML))
	if err != nil {
		t.Fatal(err)
	}
	m := &manager{ctx: ctx, db: db, path: path, bootstrap: cfg, token: sha256.Sum256([]byte(testControlToken))}
	m.current.Store(m.snapshot(cfg, controlYAML))
	if _, err = db.Exec(`INSERT INTO control_revisions VALUES(?,?,?,'active','')`, revision(controlYAML), controlYAML, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); m.sources.Wait(); m.cancelEvaluation(); m.evalWG.Wait(); db.Close() })
	return m
}
func controlRequest(m *manager, method, path string, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	if body != nil {
		json.NewEncoder(&b).Encode(body)
	}
	r := httptest.NewRequest(method, "http://localhost"+path, &b)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer "+testControlToken)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Mrmr-Control", "1")
	r.Header.Set("Origin", "http://localhost")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	return w
}
func TestManagementAccessBoundary(t *testing.T) {
	m := testManager(t)
	for _, tc := range []struct {
		method, path, peer, host, token, origin, header string
		want                                            int
	}{
		{"GET", "/manage/", "127.0.0.1:1", "localhost", "", "", "", 200},
		{"GET", "/manage", "127.0.0.1:1", "localhost", "", "", "", 307},
		{"GET", "/manage/api/state", "127.0.0.1:1", "localhost", "", "", "", 401},
		{"POST", "/manage/api/shadow", "127.0.0.1:1", "localhost", testControlToken, "http://evil.invalid", "1", 403},
		{"POST", "/manage/api/shadow", "127.0.0.1:1", "localhost", testControlToken, "http://localhost", "", 403},
		{"GET", "/manage/api/state", "192.0.2.1:1", "localhost", testControlToken, "", "", 403},
		{"GET", "/manage/api/state", "127.0.0.1:1", "evil.invalid", testControlToken, "", "", 403},
		{"GET", "/manage/api/state", "127.0.0.1:1", "localhost", testControlToken, "", "", 200},
		{"POST", "/dashboard/api/events", "127.0.0.1:1", "localhost", testControlToken, "http://localhost", "1", 405},
		{"POST", "/api/events", "127.0.0.1:1", "localhost", testControlToken, "http://localhost", "1", 404},
	} {
		r := httptest.NewRequest(tc.method, "http://localhost"+tc.path, strings.NewReader(`{"confirm":true}`))
		r.RemoteAddr = tc.peer
		r.Host = tc.host
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Mrmr-Control", tc.header)
		r.Header.Set("X-Forwarded-Host", "localhost")
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), testControlToken) {
			t.Fatal("operator credential leaked")
		}
	}
}
func saveControlDraft(t *testing.T, m *manager, text string) configRequest {
	t.Helper()
	var previous string
	_ = m.db.QueryRow(`SELECT revision FROM control_draft WHERE singleton=1`).Scan(&previous)
	req := configRequest{YAML: text, Expected: previous, Base: m.activeRevision()}
	w := controlRequest(m, "POST", "/manage/api/draft", req)
	if w.Code != 200 {
		t.Fatalf("save draft: %d %s", w.Code, w.Body.String())
	}
	req.Expected = revision(text)
	req.Confirm = true
	return req
}
func TestManagementDraftActivationAndRecovery(t *testing.T) {
	m := testManager(t)
	candidate := strings.Replace(controlYAML, "Classify this event.", "Use the new contract.", 1)
	req := saveControlDraft(t, m, candidate)
	if m.current.Load().yaml != controlYAML {
		t.Fatal("saving draft activated it")
	}
	// A stale browser cannot overwrite another draft.
	stale := configRequest{YAML: controlYAML, Expected: "stale", Base: m.activeRevision()}
	if w := controlRequest(m, "POST", "/manage/api/draft", stale); w.Code != 409 {
		t.Fatalf("stale draft: %d", w.Code)
	}
	if w := controlRequest(m, "POST", "/manage/api/activate", req); w.Code != 200 {
		t.Fatalf("activate: %d %s", w.Code, w.Body.String())
	}
	b, _ := os.ReadFile(m.path)
	if string(b) != candidate || m.current.Load().yaml != candidate {
		t.Fatal("active revision and startup file differ")
	}
	info, _ := os.Stat(m.path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("configuration file is not private")
	}
	// Rollback uses the same full validation and activation path.
	rollback := saveControlDraft(t, m, controlYAML)
	if w := controlRequest(m, "POST", "/manage/api/activate", rollback); w.Code != 200 {
		t.Fatalf("rollback: %s", w.Body.String())
	}
	if m.current.Load().yaml != controlYAML {
		t.Fatal("rollback not active")
	}
	req = saveControlDraft(t, m, candidate)
	os.WriteFile(m.path, []byte(controlYAML+"\n# external edit\n"), 0600)
	if w := controlRequest(m, "POST", "/manage/api/activate", req); w.Code == 200 {
		t.Fatal("external edit overwritten")
	}
	if m.current.Load().yaml != controlYAML {
		t.Fatal("file conflict changed running config")
	}
}
func TestManagementInvalidDraftAndUnsafeChanges(t *testing.T) {
	m := testManager(t)
	for _, text := range []string{"not: [yaml", controlYAML + "\n---\nignored: document", strings.Replace(controlYAML, "ignore: true", "notify:\n    via: stdout", 1), strings.Replace(controlYAML, "127.0.0.1:4242", "0.0.0.0:8000", 1)} {
		c, err := managedParse(text)
		if err == nil {
			err = m.activate(context.Background(), c, text, m.activeRevision())
		}
		if err == nil {
			t.Fatalf("unsafe config accepted: %s", text)
		}
		if m.current.Load().yaml != controlYAML {
			t.Fatal("invalid change replaced active configuration")
		}
	}
	for _, text := range []string{controlYAML + "\npassword: literal-secret", strings.Replace(controlYAML, "http://127.0.0.1:1/v1", "http://user:password@localhost/v1", 1), strings.Replace(controlYAML, "Classify this event.", "Bearer abcdef-secret", 1)} {
		if err := managementSafe([]byte(text)); err == nil {
			t.Fatal("inline credential accepted")
		}
	}
}
func TestManagementShadowGateBypassesBusyActivation(t *testing.T) {
	m := testManager(t)
	m.op.Lock()
	defer m.op.Unlock()
	w := controlRequest(m, "POST", "/manage/api/shadow", map[string]bool{"confirm": true})
	if w.Code != 200 {
		t.Fatalf("safety gate: %s", w.Body.String())
	}
	var stored string
	if err := m.db.QueryRow(`SELECT value FROM control_settings WHERE name='shadow'`).Scan(&stored); err != nil || stored != "true" || !m.blocked.Load() {
		t.Fatal("safety state not persisted")
	}
}
func controlEvent(t *testing.T, m *manager) {
	t.Helper()
	_, err := m.db.InsertEvent(event.Event{ID: "evt_review", Source: "test", Type: "service.warning", Timestamp: time.Now().UTC(), Data: map[string]any{"message": "Synthetic browser fixture", "id": json.Number("9007199254740993")}})
	if err != nil {
		t.Fatal(err)
	}
}
func TestManagementReviewConflictsAndHistory(t *testing.T) {
	m := testManager(t)
	controlEvent(t, m)
	before, err := m.review("evt_review")
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"id": "evt_review", "expected": before.Revision, "category": "routine", "requires_action": false, "outcome": "ignore", "state": "reviewed", "origin": "real"}
	if w := controlRequest(m, "POST", "/manage/api/review", body); w.Code != 200 {
		t.Fatalf("review: %s", w.Body.String())
	}
	after, _ := m.review("evt_review")
	if after.Label == nil || len(after.History) != 1 || after.State != "reviewed" {
		t.Fatalf("review: %+v", after)
	}
	body["expected"] = after.Revision
	if _, err := m.db.LabelEvent("evt_review", "incident", true, "notify"); err != nil {
		t.Fatal(err)
	}
	if w := controlRequest(m, "POST", "/manage/api/review", body); w.Code != 409 {
		t.Fatalf("CLI conflict: %d %s", w.Code, w.Body.String())
	}
	newest, _ := m.review("evt_review")
	if len(newest.History) != 2 {
		t.Fatal("CLI correction not recorded in history")
	}
	body["expected"] = newest.Revision
	body["state"] = "uncertain"
	if w := controlRequest(m, "POST", "/manage/api/review", body); w.Code != 200 {
		t.Fatalf("uncertain: %s", w.Body.String())
	}
	if w := controlRequest(m, "POST", "/manage/api/evaluate", map[string]any{"yaml": controlYAML, "mode": "policy", "confirm": true}); w.Code == 200 {
		t.Fatal("uncertain event used as real evaluation evidence")
	}
}
func TestManagementPolicyEvaluationDoesNotRunEffects(t *testing.T) {
	m := testManager(t)
	controlEvent(t, m)
	if _, err := m.db.Exec(`INSERT INTO event_config_revisions VALUES('evt_review',?)`, m.activeRevision()); err != nil {
		t.Fatal(err)
	}
	if err := m.db.InsertDecision(event.Decision{ID: "dec_review", EventID: "evt_review", Interpreter: "judge", Model: "test-model", Status: "ok", Result: map[string]any{"category": "routine", "requires_action": false}}); err != nil {
		t.Fatal(err)
	}
	before, _ := m.review("evt_review")
	w := controlRequest(m, "POST", "/manage/api/review", map[string]any{"id": "evt_review", "expected": before.Revision, "category": "routine", "requires_action": false, "outcome": "ignore", "state": "reviewed", "origin": "real"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = controlRequest(m, "POST", "/manage/api/evaluate", map[string]any{"yaml": controlYAML, "mode": "policy", "confirm": true})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	m.evalWG.Wait()
	var count int
	m.db.QueryRow(`SELECT count(*) FROM executions`).Scan(&count)
	if count != 0 {
		t.Fatal("evaluation performed an effect")
	}
	w = controlRequest(m, "GET", "/manage/api/evaluations", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"correct_outcomes":1`) || !strings.Contains(w.Body.String(), `"status":"complete"`) {
		t.Fatalf("report: %s", w.Body.String())
	}
}
