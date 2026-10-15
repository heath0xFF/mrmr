package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/heath0xff/mrmr/internal/policy"
)

func TestDecisionProviderSystemOneNormalization(t *testing.T) {
	for _, provider := range []string{"typesafe", "system-one", "liquid-d1"} {
		t.Run(provider, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req systemOneRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if r.URL.Path != "/v1/systemone" || req.Questions["requires_action"].Type != "noul" {
					t.Error("wrong decision transport")
				}
				json.NewEncoder(w).Encode(typeSafeResponse())
			}))
			defer srv.Close()
			q := testTypeSafeQuestions()
			yes := q["requires_action"]
			yes.Type = "yes_no"
			q["requires_action"] = yes
			p, err := NewDecisionProvider(Config{Role: "decision", Provider: provider, BaseURL: srv.URL + "/v1", Model: "pinned"})
			if err != nil {
				t.Fatal(err)
			}
			j, err := p.Decide(context.Background(), q, []byte(`{"type":"test"}`))
			if err != nil {
				t.Fatal(err)
			}
			choice, action, score := j.Answers["category"], j.Answers["requires_action"], j.Answers["severity"]
			if choice.Choice != "incident" || *choice.Confidence != .9 || choice.Probabilities["incident"] != .95 {
				t.Fatalf("confidence must remain distinct: %+v", choice)
			}
			if action.Type != "yes_no" || !*action.Value || *action.Probability != .88 || *action.Noul != .88 || action.Confidence != nil {
				t.Fatalf("yes/no: %+v", action)
			}
			if *score.Score != 1.7 || *score.Confidence != .6 || len(score.Legend) != 3 {
				t.Fatalf("score: %+v", score)
			}
			result, err := j.result()
			if err != nil {
				t.Fatal(err)
			}
			rules := policy.Policy{Rules: []policy.Rule{{If: map[string]any{"result.category.probabilities.incident": "> .9", "result.requires_action.value": true}, Then: policy.Then{Notify: &policy.Notify{Via: "stdout"}}}}}
			if then, _ := rules.Evaluate(result); then.Outcome() != "notify" {
				t.Fatal("normalized nested probabilities must be addressable by policy")
			}
		})
	}
}

// Liquid's published HTTP endpoint includes a path prefix unlike the local
// recipe. Preserve the configured prefix rather than guessing from hostnames.
func TestLiquidHostedContractPrefix(t *testing.T) {
	t.Setenv("TEST_LIQUID_KEY", "synthetic-key")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/decisions/v1/systemone" || r.Header.Get("Authorization") != "Bearer synthetic-key" {
			t.Error("incorrect hosted System One request")
		}
		response := typeSafeResponse()
		response["model"] = "d1:free"
		json.NewEncoder(w).Encode(response)
	}))
	defer srv.Close()
	p, err := NewDecisionProvider(Config{Role: "decision", Provider: "liquid-d1", BaseURL: srv.URL + "/decisions/v1/", APIKeyEnv: "TEST_LIQUID_KEY", Model: "d1:free"})
	if err != nil {
		t.Fatal(err)
	}
	j, err := p.Decide(context.Background(), testTypeSafeQuestions(), []byte(`{}`))
	if err != nil || j.Model != "d1:free" {
		t.Fatalf("hosted judgment=%+v err=%v", j, err)
	}
}

func TestDecisionProviderOpenAI(t *testing.T) {
	srv, calls := contentServer(t, `{"category":"incident","requires_action":false,"severity":0}`)
	questions := testTypeSafeQuestions()
	// YAML's unquoted boolean criterion keys need normalization before JSON encoding.
	questions["requires_action"] = Question{Type: "yes_no", Instructions: "Act?", Criteria: map[any]any{true: "Yes", false: "No"}}
	p, err := NewDecisionProvider(testConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	j, err := p.Decide(context.Background(), questions, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || j.Answers["category"].Choice != "incident" || *j.Answers["requires_action"].Value || *j.Answers["severity"].Score != 0 {
		t.Fatalf("judgment: %+v", j)
	}
	result, err := j.result()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range result {
		a := value.(map[string]any)
		if a["confidence"] != nil || a["probabilities"] != nil {
			t.Fatal("generative answers must not fabricate calibrated probabilities")
		}
	}
	if result["requires_action"].(map[string]any)["value"] != false || result["severity"].(map[string]any)["score"] != float64(0) {
		t.Fatal("zero answers were lost")
	}
}

func TestDecisionProviderRejectsExecutableChoice(t *testing.T) {
	srv, calls := contentServer(t, `{"category":"http://unconfigured.invalid/execute","requires_action":true,"severity":1}`)
	p, _ := NewDecisionProvider(testConfig(srv.URL))
	if _, err := p.Decide(context.Background(), testTypeSafeQuestions(), []byte(`{}`)); err == nil || *calls != 2 {
		t.Fatalf("invalid choice must fail after bounded correction: calls=%d err=%v", *calls, err)
	}
	if _, err := NewDecisionProvider(Config{Role: "agent", Provider: "openai-compatible"}); err == nil {
		t.Fatal("worker accepted as decision model")
	}
}

func TestDecisionProviderRejectsMalformedNativeData(t *testing.T) {
	for _, mutate := range []func(map[string]any){
		func(a map[string]any) { a["requires_action"] = map[string]any{"type": "noul", "noul": 1.1} },
		func(a map[string]any) { a["category"].(map[string]any)["confidence"] = nil },
		func(a map[string]any) { a["category"].(map[string]any)["choice"] = "noise" },
		func(a map[string]any) { a["category"].(map[string]any)["url"] = "http://unconfigured.invalid" },
		func(a map[string]any) { a["severity"].(map[string]any)["score"] = 0 },
	} {
		response := typeSafeResponse()
		mutate(response["answers"].(map[string]any))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(response) }))
		p, _ := NewDecisionProvider(Config{Provider: "liquid-d1", BaseURL: srv.URL})
		_, err := p.Decide(context.Background(), testTypeSafeQuestions(), []byte(`{}`))
		srv.Close()
		if err == nil {
			t.Fatal("invalid native answer accepted")
		}
	}
}
