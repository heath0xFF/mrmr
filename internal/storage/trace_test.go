// These tests pin what `mrmr inspect` depends on: a trace is stored whole
// and read back in the order the pipeline produced it, an inspection
// gathers every record written about one event, and a trace can never
// dangle — an audit trail pointing at an event that was never stored would
// be worse than no audit trail at all.
package storage

import (
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/event"
)

func TestInsertTraceOrderSurvivesEqualTimestamps(t *testing.T) {
	d := openTestDB(t)
	e := testEvent("trace-order", map[string]any{"sha": "abc"})
	if _, err := d.InsertEvent(e); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	// Every step shares one timestamp, which is the realistic worst case:
	// the pipeline can run several stages inside a single clock tick. Only
	// the stored sequence can recover the true order.
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	want := []string{"receive", "persist", "filter", "interpret", "policy", "outcome"}
	steps := make([]event.TraceStep, len(want))
	for i, stage := range want {
		steps[i] = event.TraceStep{At: at, Stage: stage}
	}
	if err := d.InsertTrace(e.ID, steps); err != nil {
		t.Fatalf("InsertTrace: %v", err)
	}

	got, err := d.EventTrace(e.ID)
	if err != nil {
		t.Fatalf("EventTrace: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d steps, want %d", len(got), len(want))
	}
	for i, stage := range want {
		if got[i].Stage != stage {
			t.Fatalf("step %d = %q, want %q (trace order not preserved)", i, got[i].Stage, stage)
		}
		if !got[i].At.Equal(at) {
			t.Errorf("step %d time = %s, want %s", i, got[i].At, at)
		}
	}
}

func TestInsertTraceRejectsUnknownEvent(t *testing.T) {
	d := openTestDB(t)
	// The foreign key is the guarantee that an inspected trace always
	// belongs to a real event; without it a duplicate or a failed insert
	// could leave an explanation for something that never happened.
	err := d.InsertTrace("evt_never_stored", []event.TraceStep{{At: time.Now().UTC(), Stage: "receive"}})
	if err == nil {
		t.Fatal("InsertTrace on an unknown event id must fail")
	}
}

func TestInspectGathersEverythingStored(t *testing.T) {
	d := openTestDB(t)
	e := testEvent("inspect-1", map[string]any{"msg": "service unavailable"})
	if _, err := d.InsertEvent(e); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	dec := event.Decision{
		ID: event.NewID("dec_"), EventID: e.ID, Interpreter: "mock", Model: "mock-model",
		Status: "ok", Result: map[string]any{"category": "incident", "importance": 0.9}, LatencyMs: 42,
	}
	if err := d.InsertDecision(dec); err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	execID := event.NewID("exe_")
	if err := d.InsertExecution(execID, e.ID, dec.ID, "notify", "stdout", "ok", ""); err != nil {
		t.Fatalf("insert execution: %v", err)
	}
	if err := d.InsertTrace(e.ID, []event.TraceStep{
		{At: time.Now().UTC(), Stage: "receive", Msg: "type=commit.pushed source=github"},
		{At: time.Now().UTC(), Stage: "outcome", Msg: "executed notify"},
	}); err != nil {
		t.Fatalf("insert trace: %v", err)
	}
	if _, err := d.LabelEvent(e.ID, "incident", true, "notify"); err != nil {
		t.Fatalf("label event: %v", err)
	}

	ins, err := d.Inspect(e.ID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if ins.Event.ID != e.ID || ins.Event.Data["msg"] != "service unavailable" {
		t.Errorf("event = %+v, want the stored event with its payload", ins.Event)
	}
	if len(ins.Decisions) != 1 || ins.Decisions[0].ID != dec.ID || ins.Decisions[0].Status != "ok" {
		t.Fatalf("decisions = %+v, want the one stored decision", ins.Decisions)
	}
	if ins.Decisions[0].Result["category"] != "incident" || ins.Decisions[0].LatencyMs != 42 {
		t.Errorf("decision = %+v, want the stored result and latency", ins.Decisions[0])
	}
	if len(ins.Executions) != 1 {
		t.Fatalf("executions = %+v, want the one stored execution", ins.Executions)
	}
	x := ins.Executions[0]
	if x.ID != execID || x.DecisionID != dec.ID || x.Outcome != "notify" || x.Adapter != "stdout" || x.Status != "ok" {
		t.Errorf("execution = %+v, want notify via stdout linked to the decision", x)
	}
	if x.CreatedAt.IsZero() {
		t.Error("execution created_at must be decoded")
	}
	if len(ins.Trace) != 2 || ins.Trace[0].Stage != "receive" || ins.Trace[1].Stage != "outcome" {
		t.Errorf("trace = %+v, want the stored steps in order", ins.Trace)
	}
	if ins.Label == nil || ins.Label.Category != "incident" || !ins.Label.RequiresAction {
		t.Errorf("label = %+v, want the human label attached", ins.Label)
	}
}

func TestInspectFilterExcludedEventHasNoDecision(t *testing.T) {
	d := openTestDB(t)
	e := testEvent("inspect-filtered", map[string]any{"msg": "noise"})
	if _, err := d.InsertEvent(e); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	// A filter exclusion records an execution with no decision: the model
	// never ran. Inspect must report that honestly rather than inventing a
	// link, which is the whole reason decision_id is nullable.
	if err := d.InsertExecution(event.NewID("exe_"), e.ID, "", "ignore", "", "filtered", ""); err != nil {
		t.Fatalf("insert execution: %v", err)
	}

	ins, err := d.Inspect(e.ID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(ins.Decisions) != 0 {
		t.Errorf("decisions = %+v, want none for a filter-excluded event", ins.Decisions)
	}
	if len(ins.Executions) != 1 || ins.Executions[0].DecisionID != "" || ins.Executions[0].Status != "filtered" {
		t.Errorf("executions = %+v, want one filtered execution with no decision", ins.Executions)
	}
	if ins.Label != nil {
		t.Errorf("label = %+v, want nil for an unlabeled event", ins.Label)
	}
}

func TestInspectUnknownEvent(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.Inspect("evt_missing"); err == nil {
		t.Fatal("Inspect on an unknown event id must fail")
	}
}
