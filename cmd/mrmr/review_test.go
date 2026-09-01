package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/storage"
)

func TestWriteDatasetMatchesEvalFormat(t *testing.T) {
	row := storage.LabeledEvent{
		Event: event.Event{
			ID: "evt_real", Type: "monitor.alert", Source: "monitoring",
			Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			Data:      map[string]any{"message": "service unavailable"},
		},
		Label: storage.EventLabel{
			EventID: "evt_real", Category: "incident", RequiresAction: true, ExpectedOutcome: "notify",
		},
	}
	var out bytes.Buffer
	if err := writeDataset(&out, []storage.LabeledEvent{row}); err != nil {
		t.Fatalf("writeDataset: %v", err)
	}
	var got evalCase
	if err := json.NewDecoder(&out).Decode(&got); err != nil {
		t.Fatalf("decode exported case: %v", err)
	}
	if got.Name != row.Event.ID || got.Event.Data["message"] != "service unavailable" || got.Expected.Category != "incident" || got.Expected.RequiresAction == nil || !*got.Expected.RequiresAction || got.Expected.Outcome != "notify" {
		t.Fatalf("exported case = %+v, want labeled event in eval format", got)
	}
}
