package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/heath0xff/mrmr/internal/agent"
	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
)

func delegateRuntime(t *testing.T, endpoint string) *Runtime {
	t.Helper()
	url, _ := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)
	rt.Agents = map[string]agent.Config{"homelab": {Endpoint: endpoint, ReadOnly: true, Capabilities: []string{"journal.read"}, Model: "worker"}}
	rt.Models = map[string]model.Config{"worker": {Role: "agent", Provider: "openai-compatible", BaseURL: "http://worker.invalid/v1", Model: "small-worker"}}
	rt.Policy = policy.Policy{Rules: []policy.Rule{{If: map[string]any{"event.depth": 0}, Then: policy.Then{Delegate: &policy.Delegate{Agent: "homelab", Prompt: "Investigate, do not mutate.", EmitCompletion: true, CompletionEventType: "agent.investigation.completed"}}}}, Default: policy.Then{Ignore: true}}
	return rt
}

func TestDelegateCompletionReentersWithDurableLineage(t *testing.T) {
	var request agent.Request
	var rt *Runtime
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		var status string
		if err := rt.DB.QueryRow(`SELECT status FROM executions WHERE id = ?`, request.Delegation.ExecutionID).Scan(&status); err != nil || status != "pending" {
			t.Errorf("intent not durable before worker: %s %v", status, err)
		}
		fmt.Fprint(w, `{"status":"completed","summary":"Service recovered","evidence":[{"record_id":9007199254740993}],"recommended_action":"no_action"}`)
	}))
	defer srv.Close()
	rt = delegateRuntime(t, srv.URL)
	followURL, followCalls := modelServer(t, `{"category":"no_action"}`)
	rt.Models["judge"] = model.Config{Role: "decision", Provider: "openai-compatible", BaseURL: followURL, Model: "small-judge"}
	rt.InterpretByType = map[string]model.Interpretation{"agent.investigation.completed": {Model: "judge", Questions: model.Questions{"category": {Type: "choice", Instructions: "Classify the evidence", Criteria: map[string]string{"no_action": "Resolved", "notify": "Attention needed"}}}}}
	parent := testEvent("completion")
	resp, err := rt.Ingest(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != "delegate" || followCalls.Load() != 1 {
		t.Fatalf("follow-up did not run: %+v", resp)
	}
	if request.Event.ID != parent.ID || request.Event.Subject != parent.Subject || !request.ReadOnly || len(request.Capabilities) != 1 || request.Worker.Model != "small-worker" || request.Depth != 0 || request.Prompt == "" {
		t.Fatalf("request contract: %+v", request)
	}
	ins, err := rt.DB.Inspect(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins.ChildEventIDs) != 1 || len(ins.Executions) != 1 || ins.Executions[0].Status != "ok" {
		t.Fatalf("parent: %+v", ins)
	}
	child, err := rt.DB.Inspect(ins.ChildEventIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	p := child.Event.Provenance
	if child.Event.Depth != 1 || child.Event.Subject != parent.Subject || p.ParentEventID != parent.ID || p.OriginEventID != parent.ID || p.DecisionID != resp.Decision.ID || p.ExecutionID != ins.Executions[0].ID || p.Agent != "homelab" || p.PolicyRule != "rule:1" || p.PolicyHash != rt.Policy.Version() {
		t.Fatalf("lineage: %+v", child.Event)
	}
	if child.Decisions[0].Interpreter != "judge" || child.Executions[0].Outcome != "ignore" {
		t.Fatalf("follow-up: %+v", child)
	}
	result := child.Event.Data["result"].(map[string]any)
	if result["status"] != "completed" || result["evidence"].([]any)[0].(map[string]any)["record_id"] != json.Number("9007199254740993") {
		t.Fatalf("completion data: %+v", result)
	}
	if _, err := rt.DB.LabelEvent(child.Event.ID, "no_action", false, "ignore"); err != nil {
		t.Fatal(err)
	}
	exported, err := rt.DB.LabeledEvents()
	if err != nil || len(exported) != 1 || exported[0].Event.Depth != 1 || exported[0].Event.Provenance.ExecutionID != p.ExecutionID {
		t.Fatalf("export lost lineage: %+v %v", exported, err)
	}
	replay := child.Event
	replay.ID = event.NewID("evt_")
	dup, err := rt.Ingest(context.Background(), replay)
	if err != nil || !dup.Duplicate || followCalls.Load() != 1 {
		t.Fatal("completion replay spent inference")
	}
}

func TestEmittedDecisionHasLineage(t *testing.T) {
	url, _ := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, url)
	rt.Policy = policy.Policy{Default: policy.Then{Action: &policy.Action{Type: "emit", EventType: "decision.emitted"}}}
	parent := testEvent("emit-lineage")
	parent.Depth = maxDepth
	resp, err := rt.Ingest(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := rt.DB.Inspect(parent.ID)
	if err != nil || len(ins.ChildEventIDs) != 1 {
		t.Fatalf("parent inspection=%+v err=%v", ins, err)
	}
	child, err := rt.DB.Inspect(ins.ChildEventIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	p := child.Event.Provenance
	if p.ParentEventID != parent.ID || p.DecisionID != resp.Decision.ID || p.ExecutionID != ins.Executions[0].ID || child.Event.Depth != maxDepth+1 {
		t.Fatalf("emit lost provenance: %+v", child.Event)
	}
}

func TestDelegateCompletionDepthCap(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{"status":"completed"}`) }))
	defer srv.Close()
	rt := delegateRuntime(t, srv.URL)
	rt.Policy = policy.Policy{Default: rt.Policy.Rules[0].Then}
	parent := testEvent("depth-loop")
	if _, err := rt.Ingest(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != maxDepth+1 {
		t.Fatalf("agent calls=%d", calls.Load())
	}
	var id string
	if err := rt.DB.QueryRow(`SELECT id FROM events WHERE depth = ?`, maxDepth+1).Scan(&id); err != nil {
		t.Fatal(err)
	}
	ins, err := rt.DB.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins.Decisions) != 0 || ins.Executions[0].Outcome != "ignore" || ins.Event.Provenance.OriginEventID != parent.ID {
		t.Fatalf("cap failed: %+v", ins)
	}
}

func TestDelegateShadowAndIntentFailureSuppressWork(t *testing.T) {
	for _, fault := range []string{"shadow", "intent"} {
		t.Run(fault, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{"status":"completed"}`) }))
			defer srv.Close()
			rt := delegateRuntime(t, srv.URL)
			if fault == "shadow" {
				rt.Policy.Rules[0].Then.Shadow = true
			} else {
				if _, err := rt.DB.Exec(`CREATE TRIGGER reject_intent BEFORE INSERT ON executions BEGIN SELECT RAISE(FAIL, 'test'); END`); err != nil {
					t.Fatal(err)
				}
			}
			resp, err := rt.Ingest(context.Background(), testEvent(fault))
			if err != nil {
				t.Fatal(err)
			}
			ins, err := rt.DB.Inspect(resp.EventID)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 || len(ins.ChildEventIDs) != 0 {
				t.Fatal("suppressed outcome caused work")
			}
			if fault == "shadow" && ins.Executions[0].Status != "shadow" {
				t.Fatal("shadow intent not recorded")
			}
		})
	}
}

func TestDelegateInvalidCompletionDoesNotReenter(t *testing.T) {
	for _, body := range []string{``, `accepted`, `null`, `{}`, `{"status":"accepted"}`, `{"status":"completed","url":"http://unconfigured.invalid"}`, `{"status":"completed"} {}`, `{"status":"completed","summary":` + fmt.Sprintf("%q", strings.Repeat("x", 1<<20)) + `}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		rt := delegateRuntime(t, srv.URL)
		resp, err := rt.Ingest(context.Background(), testEvent("bad"))
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		ins, err := rt.DB.Inspect(resp.EventID)
		if err != nil {
			t.Fatal(err)
		}
		if len(ins.ChildEventIDs) != 0 || ins.Executions[0].Status != "error" {
			t.Fatalf("bad completion accepted: %+v", ins)
		}
	}
}

func TestDecisionFailureNeverDelegates(t *testing.T) {
	for _, provider := range []string{"liquid-d1", "openai-compatible"} {
		t.Run(provider, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
			defer srv.Close()
			rt := delegateRuntime(t, "http://never-call.invalid")
			rt.ModelCfg = model.Config{Role: "decision", Provider: provider, BaseURL: srv.URL, Model: "judge"}
			rt.Schema, rt.Prompt = nil, ""
			rt.Questions = model.Questions{"category": {Type: "choice", Instructions: "Classify", Criteria: map[string]string{"a": "A", "b": "B"}}}
			resp, err := rt.Ingest(context.Background(), testEvent(provider))
			if err != nil {
				t.Fatal(err)
			}
			if resp.Decision.Status != "errored" || resp.Outcome != "ignore" {
				t.Fatalf("provider outage reached policy: %+v", resp)
			}
		})
	}
}

func TestModelCannotReplaceActionDestination(t *testing.T) {
	var untrusted, allowed atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { untrusted.Add(1) }))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed.Add(1)
		http.Redirect(w, r, bad.URL, http.StatusTemporaryRedirect)
	}))
	defer good.Close()
	content, _ := json.Marshal(map[string]any{"url": bad.URL, "command": "restart-everything", "recommended_action": "restart_service"})
	url, _ := modelServer(t, string(content))
	rt := newTestRuntime(t, url)
	rt.Schema = model.Schema{"url": {Type: "string"}, "command": {Type: "string"}, "recommended_action": {Type: "string"}}
	rt.Policy = policy.Policy{Rules: []policy.Rule{{If: map[string]any{"result.recommended_action": "restart_service"}, Then: policy.Then{Action: &policy.Action{Type: "http", URL: good.URL}}}}, Default: policy.Then{Ignore: true}}
	if _, err := rt.Ingest(context.Background(), testEvent("authority")); err != nil {
		t.Fatal(err)
	}
	if allowed.Load() != 1 || untrusted.Load() != 0 {
		t.Fatal("model or redirect selected an executable destination")
	}
}
