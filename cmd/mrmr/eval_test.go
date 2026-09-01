package main

import (
	"context"
	"io"
	"testing"

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
