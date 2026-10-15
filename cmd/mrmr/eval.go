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

type classMetrics struct {
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	Support   int     `json:"support"`
}

type calibrationBucket struct {
	Lower           float64 `json:"lower"`
	Upper           float64 `json:"upper"`
	Count           int     `json:"count"`
	Correct         int     `json:"correct"`
	Accuracy        float64 `json:"accuracy"`
	MeanProbability float64 `json:"mean_probability"`
}

type evalSummary struct {
	Modes             map[string]int            `json:"modes"`
	Confusion         map[string]map[string]int `json:"confusion"` // expected -> predicted; <invalid> includes failures
	Classes           map[string]classMetrics   `json:"per_class"`
	Calibration       []calibrationBucket       `json:"calibration,omitempty"`
	ActionLabeled     int                       `json:"requires_action_labeled"`
	Total             int                       `json:"total"`
	ValidDecisions    int                       `json:"valid_decisions"`
	CategoryCorrect   int                       `json:"category_correct"`
	CategoryAccuracy  float64                   `json:"category_accuracy"`
	ActionCorrect     int                       `json:"requires_action_correct"`
	ActionAccuracy    float64                   `json:"requires_action_accuracy"`
	OutcomeCorrect    int                       `json:"outcome_correct"`
	OutcomeAccuracy   float64                   `json:"outcome_accuracy"`
	ExpectedNonIgnore int                       `json:"expected_non_ignore"`
	FalseIgnores      int                       `json:"false_ignores"`
	FalseIgnoreRate   float64                   `json:"false_ignore_rate"`
}

func eval(args []string) error {
	fs := flag.NewFlagSet("mrmr eval", flag.ContinueOnError)
	configPath := fs.String("config", "mrmr.yaml", "path to config file")
	datasetPath := fs.String("dataset", "testdata/generated-eval.jsonl", "path to labeled JSONL dataset")
	mode := fs.String("mode", "all", "all, decision, or structured-output")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	cases, err := loadEvalCases(*datasetPath)
	if err != nil {
		return err
	}
	cases, err = selectEvalMode(cfg, cases, *mode)
	if err != nil {
		return err
	}
	for _, tc := range cases {
		spec := cfg.InterpreterFor(tc.Event.Type)
		if len(spec.Questions) == 0 {
			if f, ok := spec.Schema["category"]; !ok || f.Type != "string" {
				return fmt.Errorf("eval requires string interpret.schema field %q", "category")
			}
		} else {
			if q, ok := spec.Questions["category"]; !ok || q.Type != "choice" {
				return fmt.Errorf("eval requires choice interpret.questions entry %q", "category")
			}
		}
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
		if tc.Name == "" || tc.Event.Type == "" || tc.Event.Source == "" || tc.Expected.Category == "" {
			return nil, fmt.Errorf("parse dataset %s line %d: name, event type/source, and category are required", path, line)
		}
		if tc.Expected.Outcome != "ignore" && tc.Expected.Outcome != "notify" && tc.Expected.Outcome != "act" && tc.Expected.Outcome != "delegate" {
			return nil, fmt.Errorf("parse dataset %s line %d: outcome must be ignore, notify, act, or delegate", path, line)
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
	summary := evalSummary{Total: len(cases), Modes: map[string]int{}, Confusion: map[string]map[string]int{}, Classes: map[string]classMetrics{}}
	buckets := make([]calibrationBucket, 5)
	for i := range buckets {
		buckets[i].Lower = float64(i) / 5
		buckets[i].Upper = float64(i+1) / 5
	}
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
		spec := cfg.InterpreterFor(e.Type)
		mode := "structured-output"
		if len(spec.Questions) != 0 {
			mode = "decision"
		}
		summary.Modes[mode]++
		result, _, _, err := client.InterpretWithQuestions(ctx, cfg.Models[spec.Model], spec.Model, spec.Prompt, spec.Schema, spec.Questions, payload)

		category, requiresAction, outcome := "<invalid>", false, "ignore"
		actionPresent := false
		if err == nil {
			summary.ValidDecisions++
			category = evaluatedCategory(result)
			requiresAction, actionPresent = evaluatedRequiresAction(result)
			then, _ := cfg.Policy.EvaluateEvent(result, e)
			outcome = then.Outcome()
		}

		categoryOK := err == nil && category == tc.Expected.Category
		actionOK := tc.Expected.RequiresAction == nil || (err == nil && actionPresent && requiresAction == *tc.Expected.RequiresAction)
		if tc.Expected.RequiresAction != nil {
			summary.ActionLabeled++
		}
		outcomeOK := err == nil && outcome == tc.Expected.Outcome
		if categoryOK {
			summary.CategoryCorrect++
		}
		if actionOK && tc.Expected.RequiresAction != nil {
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

		if summary.Confusion[tc.Expected.Category] == nil {
			summary.Confusion[tc.Expected.Category] = map[string]int{}
		}
		summary.Confusion[tc.Expected.Category][category]++
		if err == nil {
			if answer, ok := result["category"].(map[string]any); ok {
				if probabilities, ok := answer["probabilities"].(map[string]any); ok {
					if probability, ok := probabilities[category].(float64); ok {
						index := min(int(probability*5), 4)
						buckets[index].Count++
						buckets[index].MeanProbability += probability
						if categoryOK {
							buckets[index].Correct++
						}
					}
				}
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
	summary.ActionAccuracy = ratio(summary.ActionCorrect, summary.ActionLabeled)
	summary.OutcomeAccuracy = ratio(summary.OutcomeCorrect, summary.Total)
	summary.FalseIgnoreRate = ratio(summary.FalseIgnores, summary.ExpectedNonIgnore)
	for expected, predictions := range summary.Confusion {
		m := summary.Classes[expected]
		for predicted, n := range predictions {
			m.Support += n
			if _, exists := summary.Classes[predicted]; !exists && predicted != "<invalid>" {
				summary.Classes[predicted] = classMetrics{}
			}
		}
		summary.Classes[expected] = m
	}
	for class, metrics := range summary.Classes {
		predicted := 0
		for _, row := range summary.Confusion {
			predicted += row[class]
		}
		correct := summary.Confusion[class][class]
		metrics.Precision, metrics.Recall = ratio(correct, predicted), ratio(correct, metrics.Support)
		summary.Classes[class] = metrics
	}
	for _, bucket := range buckets {
		if bucket.Count == 0 {
			continue
		}
		bucket.MeanProbability /= float64(bucket.Count)
		bucket.Accuracy = ratio(bucket.Correct, bucket.Count)
		summary.Calibration = append(summary.Calibration, bucket)
	}
	return summary
}

// Select before inference so mixed datasets can grade one model role contract
// without spending calls or changing denominators for the other contract.
func selectEvalMode(cfg *config.Config, cases []evalCase, mode string) ([]evalCase, error) {
	if mode != "all" && mode != "decision" && mode != "structured-output" {
		return nil, fmt.Errorf("eval mode must be all, decision, or structured-output")
	}
	var selected []evalCase
	for _, tc := range cases {
		decision := len(cfg.InterpreterFor(tc.Event.Type).Questions) != 0
		if mode == "all" || (mode == "decision" && decision) || (mode == "structured-output" && !decision) {
			selected = append(selected, tc)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no dataset events match eval mode %q", mode)
	}
	return selected, nil
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

func evaluatedRequiresAction(result map[string]any) (bool, bool) {
	if value, ok := result["requires_action"].(bool); ok {
		return value, true
	}
	if answer, ok := result["requires_action"].(map[string]any); ok {
		if value, ok := answer["value"].(bool); ok {
			return value, true
		}
		value, ok := answer["noul"].(float64)
		return value >= 0.5, ok
	}
	return false, false
}

func ratio(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total)
}
