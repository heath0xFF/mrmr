package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func testTypeSafeQuestions() Questions {
	return Questions{
		"category": {
			Type: "choice", Instructions: "Which category best describes `data.message`?",
			Criteria: map[string]any{"incident": "Broken now", "noise": "Routine noise"},
		},
		"requires_action": {
			Type: "noul", Instructions: "Does this require operator action?",
			Criteria: map[string]any{"true": "Action is needed", "false": "No action is needed"},
		},
		"severity": {
			Type: "score", Instructions: "How severe is this event?",
			Criteria: []any{"No impact", "Degraded with a workaround", "Blocking without a workaround"},
		},
	}
}

func typeSafeResponse() map[string]any {
	return map[string]any{
		"model": "jev-1.13.0",
		"answers": map[string]any{
			"category": map[string]any{
				"type": "choice", "choice": "incident", "confidence": 0.9,
				"probabilities": map[string]any{"incident": 0.95, "noise": 0.05},
			},
			"requires_action": map[string]any{"type": "noul", "noul": 0.88},
			"severity": map[string]any{
				"type": "score", "score": 1.7, "confidence": 0.6,
				"legend": map[string]any{
					"0": "No impact", "1": "Degraded with a workaround", "2": "Blocking without a workaround",
				},
				"probabilities": map[string]any{"0": 0.0, "1": 0.3, "2": 0.7},
			},
		},
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 20},
	}
}

func TestInterpretTypeSafeHappyPath(t *testing.T) {
	const key = "test-typesafe-key"
	t.Setenv("TYPESAFE_TEST_KEY", key)
	var observed struct {
		State     map[string]any               `json:"state"`
		Model     string                       `json:"model"`
		Questions map[string]systemOneQuestion `json:"questions"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/systemone" || r.Header.Get("Authorization") != "Bearer "+key {
			t.Errorf("request path/auth = %q/%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&observed); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(typeSafeResponse())
	}))
	defer srv.Close()

	cfg := Config{Provider: "typesafe", BaseURL: srv.URL, APIKeyEnv: "TYPESAFE_TEST_KEY", Model: "jev-latest"}
	eventJSON := []byte(`{"data":{"message":"API down","id":9007199254740993}}`)
	result, _, modelID, err := (&Client{}).InterpretWithQuestions(context.Background(), cfg, "jev", "", nil, testTypeSafeQuestions(), eventJSON)
	if err != nil {
		t.Fatal(err)
	}
	if modelID != "jev-1.13.0" || observed.Model != "jev-latest" || len(observed.Questions) != 3 {
		t.Fatalf("model/result request = %q/%q questions=%d", modelID, observed.Model, len(observed.Questions))
	}
	data := observed.State["data"].(map[string]any)
	if data["id"] != json.Number("9007199254740993") {
		t.Fatalf("state numeric identity = %v, want exact JSON number", data["id"])
	}
	category := result["category"].(map[string]any)
	action := result["requires_action"].(map[string]any)
	if category["choice"] != "incident" || category["confidence"] != 0.9 || action["noul"] != 0.88 {
		t.Fatalf("validated result = %#v", result)
	}
}

func TestInterpretTypeSafeRejectsMalformedTypedAnswer(t *testing.T) {
	response := typeSafeResponse()
	response["answers"].(map[string]any)["category"].(map[string]any)["probabilities"] = map[string]any{"incident": 0.2, "noise": 0.2}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(response)
	}))
	defer srv.Close()

	cfg := Config{Provider: "typesafe", BaseURL: srv.URL, Model: "jev-latest"}
	_, _, _, err := (&Client{}).InterpretWithQuestions(context.Background(), cfg, "jev", "", nil, testTypeSafeQuestions(), []byte(`{}`))
	var invalid *InvalidOutputError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "sum") {
		t.Fatalf("err = %v, want invalid probability distribution", err)
	}
}

func TestInterpretTypeSafeRetriesRateLimitWithoutLeakingBody(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "private reflected request")
			return
		}
		json.NewEncoder(w).Encode(typeSafeResponse())
	}))
	defer srv.Close()

	cfg := Config{Provider: "typesafe", BaseURL: srv.URL, Model: "jev-latest"}
	result, _, _, err := (&Client{}).InterpretWithQuestions(context.Background(), cfg, "jev", "", nil, testTypeSafeQuestions(), []byte(`{}`))
	if err != nil || calls.Load() != 3 || result["category"] == nil {
		t.Fatalf("calls=%d result=%v err=%v", calls.Load(), result, err)
	}
}

func TestValidateQuestions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		questions Questions
		wantErr   bool
	}{
		{"valid", testTypeSafeQuestions(), false},
		{"empty", nil, true},
		{"dot in id", Questions{"bad.id": {Type: "noul", Instructions: "Is it bad?"}}, true},
		{"choice needs options", Questions{"category": {Type: "choice", Instructions: "Which?", Criteria: map[string]any{"only": "one"}}}, true},
		{"noul criteria complete", Questions{"action": {Type: "noul", Instructions: "Act?", Criteria: map[string]any{"true": "yes"}}}, true},
		{"score needs levels", Questions{"severity": {Type: "score", Instructions: "Severity?", Criteria: []any{"only one"}}}, true},
		{"unknown type", Questions{"x": {Type: "freeform", Instructions: "Write"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateQuestions(tc.questions); (err != nil) != tc.wantErr {
				t.Fatalf("ValidateQuestions() = %v, want error=%v", err, tc.wantErr)
			}
		})
	}
}
