// This test pins the `mrmr inspect` wiring: config → database → one JSON
// object on stdout. The query itself is covered in internal/storage; what
// matters here is that the command reaches the right database and prints
// the durable trace, because that is the only way to answer "why did this
// happen?" once the ingest response is gone.
package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/storage"
)

// inspectFixture writes a config whose db.path points at a temp database,
// stores one fully-processed event in it, and returns the config path and
// the event id.
func inspectFixture(t *testing.T) (configPath, eventID string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "inspect.db")
	configPath = filepath.Join(dir, "mrmr.yaml")
	// db.path is the only field this command reads, but the config must
	// still be a valid one — inspect refuses to guess at a broken runtime
	// definition just because it is only reading.
	yaml := "models:\n" +
		"  m:\n" +
		"    provider: openai-compatible\n" +
		"    base_url: http://localhost:1/v1\n" +
		"    model: mock\n" +
		"db:\n" +
		"  path: " + dbPath + "\n" +
		"interpret:\n" +
		"  model: m\n" +
		"  prompt: classify this event\n" +
		"  schema:\n" +
		"    category:\n" +
		"      type: string\n" +
		"policy:\n" +
		"  - if:\n" +
		"      result.category: incident\n" +
		"    then:\n" +
		"      notify:\n" +
		"        via: stdout\n" +
		"default:\n" +
		"  ignore: true\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	defer db.Close()

	e := event.Event{
		ID: event.NewID("evt_"), Type: "monitor.alert", Source: "monitoring",
		Timestamp: time.Now().UTC(), Data: map[string]any{"message": "service unavailable"},
	}
	if _, err := db.InsertEvent(e); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	dec := event.Decision{
		ID: event.NewID("dec_"), EventID: e.ID, Interpreter: "m", Model: "mock",
		Status: "ok", Result: map[string]any{"category": "incident"},
	}
	if err := db.InsertDecision(dec); err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	if err := db.InsertExecution(event.NewID("exe_"), e.ID, dec.ID, "notify", "stdout", "ok", ""); err != nil {
		t.Fatalf("insert execution: %v", err)
	}
	if err := db.InsertTrace(e.ID, []event.TraceStep{
		{At: time.Now().UTC(), Stage: "receive", Msg: "type=monitor.alert source=monitoring"},
		{At: time.Now().UTC(), Stage: "policy", Msg: "rule 1 → notify"},
	}); err != nil {
		t.Fatalf("insert trace: %v", err)
	}
	return configPath, e.ID
}

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what
// it wrote. inspect prints its result rather than returning it, so the
// output is the contract under test.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	// Drain concurrently: a pipe holds ~64 KiB, and an inspection of a long
	// trace would otherwise block the writer forever waiting on a reader
	// that only runs after fn returns.
	done := make(chan []byte, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- out
	}()
	fnErr := fn()
	os.Stdout = saved
	w.Close()
	out := <-done
	r.Close()
	return string(out), fnErr
}

func TestInspectPrintsStoredTrace(t *testing.T) {
	configPath, eventID := inspectFixture(t)

	out, err := captureStdout(t, func() error {
		return inspect([]string{eventID, "-config", configPath})
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}

	var got storage.Inspection
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("inspect output is not one JSON object: %v\n%s", err, out)
	}
	if got.Event.ID != eventID {
		t.Errorf("event id = %q, want %q", got.Event.ID, eventID)
	}
	if len(got.Decisions) != 1 || got.Decisions[0].Result["category"] != "incident" {
		t.Errorf("decisions = %+v, want the stored decision", got.Decisions)
	}
	if len(got.Executions) != 1 || got.Executions[0].Outcome != "notify" {
		t.Errorf("executions = %+v, want the stored notify execution", got.Executions)
	}
	if len(got.Trace) != 2 || got.Trace[0].Stage != "receive" || got.Trace[1].Stage != "policy" {
		t.Errorf("trace = %+v, want the stored steps in order", got.Trace)
	}
}

func TestInspectRejectsMissingArguments(t *testing.T) {
	if err := inspect(nil); err == nil {
		t.Error("inspect with no event id must fail with usage")
	}
	configPath, _ := inspectFixture(t)
	err := inspect([]string{"evt_missing", "-config", configPath})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("inspect of an unknown event = %v, want a not-found error", err)
	}
}
