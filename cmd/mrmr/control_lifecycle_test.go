package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/runtime"
)

func TestManagementFirstRunAndActivation(t *testing.T) {
	m := testManager(t)
	m.current.Store(nil)
	if err := os.Remove(m.path); err != nil {
		t.Fatal(err)
	}
	w := controlRequest(m, "GET", "/manage/api/state", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"setup":true`) {
		t.Fatalf("setup status: %s", w.Body.String())
	}
	if w := controlRequest(m, "GET", "/dashboard/api/events", nil); w.Code != 503 {
		t.Fatal("setup exposes runtime evidence before activation")
	}
	req := saveControlDraft(t, m, controlYAML)
	if w := controlRequest(m, "POST", "/manage/api/activate", req); w.Code != 200 {
		t.Fatalf("first activation: %s", w.Body.String())
	}
	if m.current.Load() == nil {
		t.Fatal("setup did not activate runtime")
	}
	if b, err := os.ReadFile(m.path); err != nil || string(b) != controlYAML {
		t.Fatal("setup did not create valid startup configuration")
	}
}
func TestManagementActivationDrainsBeforeSwitch(t *testing.T) {
	m := testManager(t)
	stopping, release := make(chan struct{}), make(chan struct{})
	old := m.current.Load()
	old.jobs = []sourceJob{func(ctx context.Context, stop <-chan struct{}, rt *runtime.Runtime) {
		select {
		case <-stop:
			close(stopping)
		case <-ctx.Done():
			return
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}}
	m.startSources(old.jobs, old.rt)
	candidate := strings.Replace(controlYAML, "Classify this event.", "A new revision.", 1)
	cfg, err := config.Parse([]byte(candidate))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.activate(context.Background(), cfg, candidate, old.revision, false) }()
	select {
	case <-stopping:
	case <-time.After(time.Second):
		t.Fatal("source was not asked to drain")
	}
	if m.current.Load() != old {
		t.Fatal("active revision changed before old source drained")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("activation did not complete")
	}
	if m.current.Load() == old {
		t.Fatal("activation did not switch runtime")
	}
}
func TestManagementFailedSourceKeepsRuntimeAndCursor(t *testing.T) {
	m := testManager(t)
	if err := m.db.SetCursor("feed", "known-cursor"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MISSING_CONTROL_TEST_TOKEN", "")
	candidate := controlYAML + `sources:
  - name: feed
    type: http-poller
    url: http://127.0.0.1:1/feed
    every: 30s
    event_type: test.item
    bearer_token_env: MISSING_CONTROL_TEST_TOKEN
`
	c, err := config.Parse([]byte(candidate))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.activate(context.Background(), c, candidate, m.activeRevision(), false); err == nil {
		t.Fatal("missing source credential passed readiness")
	}
	if m.current.Load().yaml != controlYAML {
		t.Fatal("failed source replaced runtime")
	}
	cursor, err := m.db.Cursor("feed")
	if err != nil || cursor != "known-cursor" {
		t.Fatal("failed activation changed existing cursor")
	}
	// An empty cursor is still an existing record, not permission to delete
	// it as a fresh preparation artifact during compensating cleanup.
	if err := m.db.SetCursor("feed", ""); err != nil {
		t.Fatal(err)
	}
	if err = m.activate(context.Background(), c, candidate, m.activeRevision(), false); err == nil {
		t.Fatal("missing credential accepted")
	}
	var count int
	if err = m.db.QueryRow(`SELECT count(*) FROM source_cursors WHERE name='feed'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("failed activation deleted an existing empty checkpoint")
	}
}
func TestManagementShadowFailureStaysClosed(t *testing.T) {
	m := testManager(t)
	if _, err := m.db.Exec(`CREATE TRIGGER reject_safety_audit BEFORE INSERT ON control_audit BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	w := controlRequest(m, "POST", "/manage/api/shadow", map[string]bool{"confirm": true})
	if w.Code != 400 || !m.blocked.Load() || !strings.Contains(w.Body.String(), "Retry before restarting") {
		t.Fatalf("failed safety write: %d %s", w.Code, w.Body.String())
	}
	if durable, err := m.db.EffectsBlocked(); err != nil || durable {
		t.Fatal("failed audit must roll back its gate transaction")
	}
	if _, err := m.db.Exec(`DROP TRIGGER reject_safety_audit`); err != nil {
		t.Fatal(err)
	}
	w = controlRequest(m, "POST", "/manage/api/shadow", map[string]bool{"confirm": true})
	if durable, err := m.db.EffectsBlocked(); err != nil || !durable || w.Code != 200 {
		t.Fatal("retry did not persist the safety gate")
	}
}

func TestManagementEvaluationCancellation(t *testing.T) {
	m := testManager(t)
	controlEvent(t, m)
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	text := strings.Replace(controlYAML, "http://127.0.0.1:1/v1", server.URL, 1)
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	m.current.Store(m.snapshot(cfg, text))
	before, _ := m.review("evt_review")
	w := controlRequest(m, "POST", "/manage/api/review", map[string]any{"id": "evt_review", "expected": before.Revision, "category": "routine", "requires_action": false, "outcome": "ignore", "state": "reviewed", "origin": "real"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = controlRequest(m, "POST", "/manage/api/evaluate", map[string]any{"yaml": text, "mode": "interpreter", "confirm": true})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("evaluation did not start")
	}
	w = controlRequest(m, "POST", "/manage/api/cancel-evaluation", map[string]any{})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	done := make(chan struct{})
	go func() { m.evalWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("evaluation leaked after cancellation")
	}
	var status string
	if err := m.db.QueryRow(`SELECT status FROM control_evaluations`).Scan(&status); err != nil || status != "interrupted" {
		t.Fatalf("job status: %s %v", status, err)
	}
}
