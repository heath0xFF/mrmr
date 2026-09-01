package storage

import (
	"strings"
	"testing"

	"github.com/heath0xff/mrmr/internal/event"
)

func TestEventLabelReviewAndExport(t *testing.T) {
	d := openTestDB(t)
	first := testEvent("label-1", map[string]any{"message": "needs review"})
	second := testEvent("label-2", map[string]any{"message": "still unlabeled"})
	if _, err := d.InsertEvent(first); err != nil {
		t.Fatalf("insert first event: %v", err)
	}
	if _, err := d.InsertEvent(second); err != nil {
		t.Fatalf("insert second event: %v", err)
	}
	dec := event.Decision{
		ID: "dec_review", EventID: second.ID, Interpreter: "mock", Model: "mock-model",
		Status: "ok", Result: map[string]any{"category": "noise"}, LatencyMs: 4,
	}
	if err := d.InsertDecision(dec); err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	if err := d.InsertExecution("exe_review", second.ID, dec.ID, "ignore", "", "ok", ""); err != nil {
		t.Fatalf("insert execution: %v", err)
	}

	label, err := d.LabelEvent(first.ID, "incident", true, "notify")
	if err != nil {
		t.Fatalf("LabelEvent: %v", err)
	}
	if label.Category != "incident" || !label.RequiresAction || label.ExpectedOutcome != "notify" {
		t.Fatalf("label = %+v, want incident/action/notify", label)
	}

	unlabeled, err := d.ListEvents(10, true)
	if err != nil {
		t.Fatalf("ListEvents unlabeled: %v", err)
	}
	if len(unlabeled) != 1 || unlabeled[0].Event.ID != second.ID {
		t.Fatalf("unlabeled = %+v, want only %s", unlabeled, second.ID)
	}
	if unlabeled[0].Decision == nil || unlabeled[0].Decision.ID != dec.ID || unlabeled[0].Outcome != "ignore" {
		t.Fatalf("review decision/outcome = %+v/%q, want %s/ignore", unlabeled[0].Decision, unlabeled[0].Outcome, dec.ID)
	}

	if _, err := d.LabelEvent(first.ID, "noise", false, "ignore"); err != nil {
		t.Fatalf("relabel event: %v", err)
	}
	exported, err := d.LabeledEvents()
	if err != nil {
		t.Fatalf("LabeledEvents: %v", err)
	}
	if len(exported) != 1 || exported[0].Event.ID != first.ID || exported[0].Event.Data["message"] != "needs review" {
		t.Fatalf("exported events = %+v, want original labeled event", exported)
	}
	if exported[0].Label.Category != "noise" || exported[0].Label.RequiresAction || exported[0].Label.ExpectedOutcome != "ignore" {
		t.Fatalf("exported label = %+v, want replacement noise/false/ignore", exported[0].Label)
	}
}

func TestLabelEventRejectsInvalidInput(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.LabelEvent("missing", "incident", true, "notify"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing event error = %v, want not found", err)
	}
	if _, err := d.LabelEvent("missing", "incident", true, "act"); err == nil || !strings.Contains(err.Error(), "ignore or notify") {
		t.Fatalf("invalid outcome error = %v, want ignore or notify", err)
	}
}
