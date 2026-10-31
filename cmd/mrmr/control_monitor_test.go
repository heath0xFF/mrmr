package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
)

func monitorCandidate(t *testing.T, endpoint string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(strings.ReplaceAll(setupYAML, "MODEL_URL", endpoint)))
	if err != nil {
		t.Fatal(err)
	}
	c.Monitors = map[string]config.Monitor{"service-monitor": {Interpret: c.Interpret, Policy: c.Policy}}
	local := c.Monitors["service-monitor"]
	local.Policy.Rules[0].Then.Notify = &policy.Notify{Via: "discord", WebhookEnv: "MRMR_MONITOR_DISCORD", Message: "Event {{ .event.id }}"}
	c.Monitors["service-monitor"] = local
	other := c.Sources[0]
	other.Name = "other"
	c.Sources = append(c.Sources, other)
	// Neither a same-type legacy source nor a default model may receive this
	// monitor's test. An unreachable fallback makes a routing mistake visible.
	c.Models["fallback"] = model.Config{Provider: "openai-compatible", BaseURL: "http://127.0.0.1:1/v1", Model: "fallback"}
	c.Interpret = config.Interpret{Model: "fallback", Prompt: "Classify", Schema: model.Schema{"category": {Type: "string", Enum: []any{"global-only"}}, "requires_action": {Type: "boolean"}}}
	c.Policy = policy.Policy{Default: policy.Then{Ignore: true}}
	return c
}

func monitorYAML(t *testing.T, c *config.Config) string {
	t.Helper()
	b, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMonitorSetupRequiresExplicitSourceAndUsesLocalContract(t *testing.T) {
	m := testManager(t)
	var calls atomic.Int32
	srv := syntheticModel(t, &calls)
	defer srv.Close()
	candidate := monitorYAML(t, monitorCandidate(t, srv.URL))
	for _, source := range []string{"", "not-configured"} {
		w := controlRequest(m, "POST", "/manage/api/test-model", map[string]any{"yaml": candidate, "source": source, "confirm": true})
		if w.Code != 400 || calls.Load() != 0 {
			t.Fatal("ambiguous selection reached a model")
		}
	}
	w := controlRequest(m, "POST", "/manage/api/test-model", map[string]any{"yaml": candidate, "source": "service-monitor", "confirm": true})
	var result struct {
		Ready  bool        `json:"ready"`
		Report evalSummary `json:"report"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || !result.Ready || result.Report.OutcomeCorrect != 2 || calls.Load() != 2 {
		t.Fatalf("source-local test: %d %s", w.Code, w.Body.String())
	}
	for _, table := range []string{"events", "executions", "source_cursors"} {
		var count int
		if err := m.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("synthetic monitor test mutated operational state")
		}
	}
}

func TestMonitorActivationSafetyCredentialsAndReview(t *testing.T) {
	m := testManager(t)
	const secret = "synthetic-monitor-secret-value"
	t.Setenv("MRMR_MONITOR_DISCORD", secret)
	c := monitorCandidate(t, "http://127.0.0.1:1/v1")
	for _, useDefault := range []bool{false, true} {
		local := c.Monitors["service-monitor"]
		local.Policy.Rules[0].Then.Shadow = false
		if useDefault {
			local.Policy.Default = local.Policy.Rules[0].Then
			local.Policy.Rules = nil
		}
		c.Monitors["service-monitor"] = local
		req := saveControlDraft(t, m, monitorYAML(t, c))
		if w := controlRequest(m, "POST", "/manage/api/activate", req); w.Code != 400 {
			t.Fatal("source-local live effect bypassed management activation guard")
		}
		if m.current.Load().yaml != controlYAML {
			t.Fatal("failed activation replaced the active revision")
		}
	}
	c = monitorCandidate(t, "http://127.0.0.1:1/v1")
	local := c.Monitors["service-monitor"]
	local.Policy.Rules[0].Then.Notify.Message = secret
	c.Monitors["service-monitor"] = local
	if err := managementSafe([]byte(monitorYAML(t, c))); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("monitor credential value was accepted or disclosed")
	}
	c = monitorCandidate(t, "http://127.0.0.1:1/v1")
	legacy := *c
	legacy.Monitors = nil
	legacyReq := saveControlDraft(t, m, monitorYAML(t, &legacy))
	if w := controlRequest(m, "POST", "/manage/api/activate", legacyReq); w.Code != 200 {
		t.Fatal("could not activate legacy-source fixture")
	}
	if err := m.db.SetCursor("service-monitor", "retained-cursor"); err != nil {
		t.Fatal(err)
	}
	req := saveControlDraft(t, m, monitorYAML(t, c))
	if w := controlRequest(m, "POST", "/manage/api/activate", req); w.Code != 200 {
		t.Fatalf("shadow monitor activation: %d %s", w.Code, w.Body.String())
	}
	if cursor, err := m.db.Cursor("service-monitor"); err != nil || cursor != "retained-cursor" {
		t.Fatal("monitor activation reset source progress")
	}
	w := controlRequest(m, "GET", "/manage/api/state", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), secret) || !strings.Contains(w.Body.String(), `"kind":"Discord"`) || !strings.Contains(w.Body.String(), "configured (not tested)") {
		t.Fatal("unsafe or missing Discord connection state")
	}
	controlEvent(t, m)
	if _, err := m.db.Exec(`UPDATE events SET source='service-monitor' WHERE id='evt_review'`); err != nil {
		t.Fatal(err)
	}
	info, err := m.review("evt_review")
	if err != nil || !info.ContractSupported || !strings.Contains(strings.Join(info.Categories, ","), "incident") || strings.Contains(strings.Join(info.Categories, ","), "global-only") {
		t.Fatalf("review used the fallback contract: %+v %v", info, err)
	}

	// A saved policy comparison must use the source-local contract for both
	// candidate and baseline, not report the shared ignore default as evidence.
	if _, err := m.db.Exec(`INSERT INTO event_config_revisions VALUES('evt_review',?)`, m.activeRevision()); err != nil {
		t.Fatal(err)
	}
	if err := m.db.InsertDecision(event.Decision{ID: "dec_monitor", EventID: "evt_review", Interpreter: "judge", Model: c.Models["judge"].Model, Status: "ok", Result: map[string]any{
		"category": map[string]any{"choice": "incident"}, "requires_action": map[string]any{"value": true}, "severity": map[string]any{"score": 2},
	}}); err != nil {
		t.Fatal(err)
	}
	w = controlRequest(m, "POST", "/manage/api/review", map[string]any{"id": "evt_review", "expected": info.Revision, "category": "incident", "requires_action": true, "outcome": "notify", "state": "reviewed", "origin": "real"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	local = c.Monitors["service-monitor"]
	local.Policy.Rules = nil
	c.Monitors["service-monitor"] = local
	w = controlRequest(m, "POST", "/manage/api/evaluate", map[string]any{"yaml": monitorYAML(t, c), "mode": "policy", "confirm": true})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	m.evalWG.Wait()
	w = controlRequest(m, "GET", "/manage/api/evaluations", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"baseline_outcome":"notify"`) || !strings.Contains(w.Body.String(), `"false_ignores":1`) || !strings.Contains(w.Body.String(), `"status":"complete"`) {
		t.Fatalf("comparison lost monitor routing: %s", w.Body.String())
	}
	var executions int
	if err := m.db.QueryRow(`SELECT count(*) FROM executions`).Scan(&executions); err != nil || executions != 0 {
		t.Fatal("monitor evaluation dispatched effects")
	}
}
