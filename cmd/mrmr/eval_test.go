package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
)

func TestGeneratedEvalDataset(t *testing.T) {
	cases, err := loadEvalCases("../../testdata/generated-eval.jsonl")
	if err != nil {
		t.Fatalf("loadEvalCases: %v", err)
	}
	if len(cases) != 50 {
		t.Fatalf("dataset has %d cases, want 50", len(cases))
	}
}

// Real-event evaluation must grade the recorded evidence, including numeric
// identities and source time. Stable fixture defaults only fill missing fields.
func TestEvaluatePreservesRecordedEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "real.jsonl")
	if err := os.WriteFile(path, []byte(`{"name":"real","event":{"id":"evt_real","type":"test","source":"test","timestamp":"2026-09-15T12:00:00-05:00","data":{"id":9007199254740993}},"expected":{"category":"noise","requires_action":false,"outcome":"ignore"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cases, err := loadEvalCases(path)
	if err != nil {
		t.Fatal(err)
	}
	var observed event.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		for _, msg := range req.Messages {
			if msg.Role == "user" {
				// Keep the mock from hiding the very rounding under test.
				dec := json.NewDecoder(strings.NewReader(msg.Content))
				dec.UseNumber()
				if err := dec.Decode(&observed); err != nil {
					t.Error(err)
				}
			}
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"category\":\"noise\",\"requires_action\":false}"}}]}`)
	}))
	defer srv.Close()
	cfg := &config.Config{
		Models: map[string]model.Config{"mock": {BaseURL: srv.URL, Model: "mock"}},
		Interpret: config.Interpret{Model: "mock", Schema: model.Schema{
			"category": {Type: "string"}, "requires_action": {Type: "boolean"},
		}},
		Policy: policy.Policy{Default: policy.Then{Ignore: true}},
	}
	evaluate(context.Background(), cfg, cases, io.Discard)
	if observed.ID != "evt_real" || observed.Timestamp.Format(time.RFC3339) != "2026-09-15T12:00:00-05:00" || observed.Data["id"] != json.Number("9007199254740993") {
		t.Fatalf("evaluation rewrote recorded evidence: %+v", observed)
	}
	cases[0].Event.ID = ""
	cases[0].Event.Timestamp = time.Time{}
	evaluate(context.Background(), cfg, cases, io.Discard)
	if observed.ID != "eval_001" || observed.Timestamp.Format(time.RFC3339) != "2026-01-01T12:00:00Z" {
		t.Fatalf("missing fixture defaults: %+v", observed)
	}
}

func TestEvaluateMetrics(t *testing.T) {
	url, _ := modelServer(t,
		`{"category":"incident","importance":0.95,"requires_action":true}`,
		`{"category":"noise","importance":0.1,"requires_action":false}`,
	)
	cfg := &config.Config{
		Models: map[string]model.Config{
			"mock": {Provider: "openai-compatible", BaseURL: url, Model: "mock-model"},
		},
		Interpret: config.Interpret{
			Model:  "mock",
			Prompt: "classify",
			Schema: model.Schema{
				"category":        {Type: "string", Enum: []any{"incident", "actionable", "noise"}},
				"importance":      {Type: "number"},
				"requires_action": {Type: "boolean"},
			},
		},
		Policy: policy.Policy{
			Rules: []policy.Rule{{
				If: map[string]any{
					"result.importance":      "> 0.8",
					"result.requires_action": true,
				},
				Then: policy.Then{Notify: &policy.Notify{Via: "stdout"}},
			}},
			Default: policy.Then{Ignore: true},
		},
	}

	yes := true
	cases := []evalCase{
		{Name: "correct", Event: event.Event{Type: "test", Source: "test"}},
		{Name: "false-ignore", Event: event.Event{Type: "test", Source: "test"}},
	}
	cases[0].Expected.Category, cases[0].Expected.RequiresAction, cases[0].Expected.Outcome = "incident", &yes, "notify"
	cases[1].Expected.Category, cases[1].Expected.RequiresAction, cases[1].Expected.Outcome = "actionable", &yes, "notify"

	s := evaluate(context.Background(), cfg, cases, io.Discard)
	if s.Total != 2 || s.ValidDecisions != 2 || s.CategoryCorrect != 1 || s.ActionCorrect != 1 || s.OutcomeCorrect != 1 {
		t.Fatalf("summary counts = %+v, want one correct of two valid decisions", s)
	}
	if s.CategoryAccuracy != 0.5 || s.ActionAccuracy != 0.5 || s.OutcomeAccuracy != 0.5 || s.FalseIgnores != 1 || s.FalseIgnoreRate != 0.5 {
		t.Fatalf("summary rates = %+v, want 0.5 accuracy and false-ignore rate", s)
	}
}
