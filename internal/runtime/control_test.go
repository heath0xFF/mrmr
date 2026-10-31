package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/policy"
)

func TestReturnToShadowSuppressesInFlightEffect(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": `{"category":"important","importance":1}`}}}})
	}))
	defer modelServer.Close()
	var calls atomic.Int32
	adapter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer adapter.Close()
	rt := newTestRuntime(t, modelServer.URL)
	rt.Policy.Rules[0].Then = policy.Then{Action: &policy.Action{Type: "http", URL: adapter.URL, Method: "POST"}}
	rt.ConfigRevision = "test-revision"
	var blocked atomic.Bool
	rt.EffectsBlocked = blocked.Load
	e := testEvent("")
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _, err := rt.Ingest(ctx, e); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("model did not start")
	}
	blocked.Store(true)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("effect dispatched after operator safety gate")
	}
	ins, err := rt.DB.Inspect(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ins.ConfigRevision != "test-revision" || len(ins.Executions) != 1 || ins.Executions[0].Status != "shadow" {
		t.Fatalf("missing safety evidence: %+v", ins)
	}
	found := false
	for _, step := range ins.Trace {
		if step.Stage == "safety" {
			found = true
		}
	}
	if !found {
		t.Fatal("operator suppression missing from trace")
	}
}
