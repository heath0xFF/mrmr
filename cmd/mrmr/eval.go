package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/model"
)

type evalCase struct {
	Name     string      `json:"name"`
	Event    event.Event `json:"event"`
	Expected struct {
		Category       string `json:"category"`
		RequiresAction *bool  `json:"requires_action"`
		Outcome        string `json:"outcome"`
	} `json:"expected"`
}

type evalSummary struct {
	Total             int     `json:"total"`
	ValidDecisions    int     `json:"valid_decisions"`
	CategoryCorrect   int     `json:"category_correct"`
	CategoryAccuracy  float64 `json:"category_accuracy"`
	ActionCorrect     int     `json:"requires_action_correct"`
	ActionAccuracy    float64 `json:"requires_action_accuracy"`
	OutcomeCorrect    int     `json:"outcome_correct"`
	OutcomeAccuracy   float64 `json:"outcome_accuracy"`
	ExpectedNonIgnore int     `json:"expected_non_ignore"`
	FalseIgnores      int     `json:"false_ignores"`
	FalseIgnoreRate   float64 `json:"false_ignore_rate"`
}

func eval(args []string) error {
	fs := flag.NewFlagSet("mrmr eval", flag.ContinueOnError)
	configPath := fs.String("config", "mrmr.yaml", "path to config file")
	datasetPath := fs.String("dataset", "testdata/generated-eval.jsonl", "path to labeled JSONL dataset")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	switch cfg.Models[cfg.Interpret.Model].Provider {
	case "openai-compatible":
		if f, ok := cfg.Interpret.Schema["category"]; !ok || f.Type != "string" {
			return fmt.Errorf("eval requires string interpret.schema field %q", "category")
		}
		if f, ok := cfg.Interpret.Schema["requires_action"]; !ok || f.Type != "boolean" {
			return fmt.Errorf("eval requires boolean interpret.schema field %q", "requires_action")
		}
	case "typesafe":
		if q, ok := cfg.Interpret.Questions["category"]; !ok || q.Type != "choice" {
			return fmt.Errorf("eval requires choice interpret.questions entry %q", "category")
		}
		if q, ok := cfg.Interpret.Questions["requires_action"]; !ok || q.Type != "noul" {
			return fmt.Errorf("eval requires noul interpret.questions entry %q", "requires_action")
		}
	}
	cases, err := loadEvalCases(*datasetPath)
	if err != nil {
		return err
	}

	summary := evaluate(context.Background(), cfg, cases, os.Stderr)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(summary)
}

func loadEvalCases(path string) ([]evalCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open dataset %s: %w", path, err)
	}
	defer f.Close()

	var cases []evalCase
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64*1024), 1<<20)
	for line := 1; s.Scan(); line++ {
		if len(s.Bytes()) == 0 {
			continue
		}
		var tc evalCase
		// Exported numeric identities must survive evaluation as written,
		// not round through float64 before reaching the model.
		dec := json.NewDecoder(bytes.NewReader(s.Bytes()))
		dec.UseNumber()
		if err := dec.Decode(&tc); err != nil {
			return nil, fmt.Errorf("parse dataset %s line %d: %w", path, line, err)
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return nil, fmt.Errorf("parse dataset %s line %d: expected one JSON object", path, line)
		}
		if tc.Name == "" || tc.Event.Type == "" || tc.Event.Source == "" || tc.Expected.Category == "" || tc.Expected.RequiresAction == nil {
			return nil, fmt.Errorf("parse dataset %s line %d: name, event type/source, category, and requires_action are required", path, line)
		}
		if tc.Expected.Outcome != "ignore" && tc.Expected.Outcome != "notify" {
			return nil, fmt.Errorf("parse dataset %s line %d: outcome must be ignore or notify", path, line)
		}
		cases = append(cases, tc)
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("read dataset %s: %w", path, err)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("dataset %s is empty", path)
	}
	return cases, nil
}

func evaluate(ctx context.Context, cfg *config.Config, cases []evalCase, progress io.Writer) evalSummary {
	summary := evalSummary{Total: len(cases)}
	client := &model.Client{}
	for i, tc := range cases {
		e := tc.Event
		// Generated fixtures may omit identity/time; keep their defaults
		// deterministic without rewriting the evidence in real events.
		if e.ID == "" {
			e.ID = fmt.Sprintf("eval_%03d", i+1)
		}
		if e.Timestamp.IsZero() {
			e.Timestamp = time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
		}
		payload, _ := json.Marshal(e)
		result, _, _, err := client.InterpretWithQuestions(ctx, cfg.Models[cfg.Interpret.Model], cfg.Interpret.Model, cfg.Interpret.Prompt, cfg.Interpret.Schema, cfg.Interpret.Questions, payload)

		category, requiresAction, outcome := "", false, "ignore"
		if err == nil {
			summary.ValidDecisions++
			category = evaluatedCategory(result)
			requiresAction = evaluatedRequiresAction(result)
			then, _ := cfg.Policy.Evaluate(result)
			outcome = then.Outcome()
		}

		categoryOK := err == nil && category == tc.Expected.Category
		actionOK := err == nil && requiresAction == *tc.Expected.RequiresAction
		outcomeOK := err == nil && outcome == tc.Expected.Outcome
		if categoryOK {
			summary.CategoryCorrect++
		}
		if actionOK {
			summary.ActionCorrect++
		}
		if outcomeOK {
			summary.OutcomeCorrect++
		}
		if tc.Expected.Outcome != "ignore" {
			summary.ExpectedNonIgnore++
			if outcome == "ignore" {
				summary.FalseIgnores++
			}
		}

		if err != nil {
			fmt.Fprintf(progress, "[%d/%d] ERROR %s: %v\n", i+1, len(cases), tc.Name, err)
		} else if categoryOK && actionOK && outcomeOK {
			fmt.Fprintf(progress, "[%d/%d] PASS  %s\n", i+1, len(cases), tc.Name)
		} else {
			fmt.Fprintf(progress, "[%d/%d] FAIL  %s: got category=%s requires_action=%t outcome=%s\n", i+1, len(cases), tc.Name, category, requiresAction, outcome)
		}
	}

	summary.CategoryAccuracy = ratio(summary.CategoryCorrect, summary.Total)
	summary.ActionAccuracy = ratio(summary.ActionCorrect, summary.Total)
	summary.OutcomeAccuracy = ratio(summary.OutcomeCorrect, summary.Total)
	summary.FalseIgnoreRate = ratio(summary.FalseIgnores, summary.ExpectedNonIgnore)
	return summary
}

// The evaluation dataset predates provider-specific result shapes. Preserve
// its labels while reading either a flat OpenAI-compatible result or the raw
// typed TypeSafe answer. A Noul's natural yes/no boundary is 0.5; policy may
// and usually should use a stricter threshold for consequential outcomes.
func evaluatedCategory(result map[string]any) string {
	if value, ok := result["category"].(string); ok {
		return value
	}
	if answer, ok := result["category"].(map[string]any); ok {
		value, _ := answer["choice"].(string)
		return value
	}
	return ""
}

func evaluatedRequiresAction(result map[string]any) bool {
	if value, ok := result["requires_action"].(bool); ok {
		return value
	}
	if answer, ok := result["requires_action"].(map[string]any); ok {
		value, _ := answer["noul"].(float64)
		return value >= 0.5
	}
	return false
}

func ratio(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total)
}
