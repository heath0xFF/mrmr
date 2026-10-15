package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
)

func TestDecisionEvaluationMetrics(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 3 {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "judge", "answers": map[string]any{"category": map[string]any{
			"type": "choice", "choice": "incident", "confidence": .1, "probabilities": map[string]any{"incident": .95, "noise": .05},
		}}})
	}))
	defer srv.Close()
	cfg := &config.Config{Models: map[string]model.Config{"judge": {Provider: "liquid-d1", BaseURL: srv.URL, Model: "judge"}}, Interpret: config.Interpret{Model: "judge", Questions: model.Questions{"category": {Type: "choice", Instructions: "Classify", Criteria: map[string]string{"incident": "Broken", "noise": "Routine"}}}}, Policy: policy.Policy{Default: policy.Then{Notify: &policy.Notify{Via: "stdout"}}}}
	cases := make([]evalCase, 3)
	for i := range cases {
		cases[i].Name = "synthetic"
		cases[i].Event = event.Event{Type: "test", Source: "test"}
		cases[i].Expected.Category = "incident"
		cases[i].Expected.Outcome = "notify"
	}
	cases[1].Expected.Category, cases[1].Expected.Outcome = "noise", "ignore"
	s := evaluate(context.Background(), cfg, cases, io.Discard)
	if s.Modes["decision"] != 3 || s.CategoryCorrect != 1 || s.FalseIgnoreRate != .5 || s.ActionLabeled != 0 {
		t.Fatalf("summary=%+v", s)
	}
	if s.Confusion["incident"]["<invalid>"] != 1 || s.Confusion["noise"]["incident"] != 1 || s.Classes["incident"].Precision != .5 || s.Classes["incident"].Recall != .5 || s.Classes["noise"].Support != 1 {
		t.Fatalf("classification metrics=%+v", s)
	}
	if len(s.Calibration) != 1 || s.Calibration[0].Lower != .8 || s.Calibration[0].MeanProbability != .95 || s.Calibration[0].Accuracy != .5 {
		t.Fatalf("calibration must use probability, not confidence: %+v", s.Calibration)
	}
}

func TestEvaluationModeSelection(t *testing.T) {
	cfg := &config.Config{Interpret: config.Interpret{Schema: model.Schema{"category": {Type: "string"}}}, InterpretByType: map[string]config.Interpret{"agent.completed": {Questions: model.Questions{"category": {Type: "choice"}}}}}
	cases := []evalCase{{Event: event.Event{Type: "journal"}}, {Event: event.Event{Type: "agent.completed"}}}
	for _, mode := range []string{"decision", "structured-output"} {
		selected, err := selectEvalMode(cfg, cases, mode)
		if err != nil || len(selected) != 1 {
			t.Fatalf("mode=%s cases=%d err=%v", mode, len(selected), err)
		}
		if (selected[0].Event.Type == "agent.completed") != (mode == "decision") {
			t.Fatal("wrong evaluation contract selected")
		}
	}
	if _, err := selectEvalMode(cfg, cases, "typo"); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if _, err := selectEvalMode(cfg, cases[:1], "decision"); err == nil {
		t.Fatal("empty mode accepted")
	}
}

func TestFollowUpEvaluationAndLabelValidation(t *testing.T) {
	url, _ := modelServer(t, `{"category":"no_action"}`)
	cfg := &config.Config{Models: map[string]model.Config{"judge": {Provider: "openai-compatible", BaseURL: url, Model: "judge"}}, Interpret: config.Interpret{Model: "unused"}, InterpretByType: map[string]config.Interpret{
		"agent.completed": {Model: "judge", Questions: model.Questions{"category": {Type: "choice", Instructions: "Judge findings", Criteria: map[string]string{"no_action": "Resolved", "notify": "Attention needed"}}}},
	}, Policy: policy.Policy{Default: policy.Then{Ignore: true}}}
	cases := []evalCase{{Name: "follow-up", Event: event.Event{Type: "agent.completed", Source: "mrmr.agent", Depth: 1}}}
	cases[0].Expected.Category, cases[0].Expected.Outcome = "no_action", "ignore"
	s := evaluate(context.Background(), cfg, cases, io.Discard)
	if s.CategoryAccuracy != 1 || s.OutcomeAccuracy != 1 || s.Modes["decision"] != 1 {
		t.Fatalf("follow-up evaluation=%+v", s)
	}
	cfg.Interpret = cfg.InterpreterFor("agent.completed")
	if err := validateLabelCategory(cfg, "no_action"); err != nil {
		t.Fatal(err)
	}
	if err := validateLabelCategory(cfg, "unconfigured"); err == nil {
		t.Fatal("invalid choice label accepted")
	}
	// Missing answers are not correct negative judgments.
	no := false
	cases[0].Expected.RequiresAction = &no
	s = evaluate(context.Background(), cfg, cases, io.Discard)
	if s.ActionLabeled != 1 || s.ActionCorrect != 0 {
		t.Fatal("missing yes/no answer was graded as false")
	}
}
