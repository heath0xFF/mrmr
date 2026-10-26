package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
)

func TestPollHealthLifecycleAndConcurrentSnapshots(t *testing.T) {
	rt := &Runtime{}
	if h := rt.PollHealth("source"); !h.LastSuccess.IsZero() || h.Running {
		t.Fatal("new runtime must be unknown")
	}
	rt.StartPoll("source")
	if h := rt.PollHealth("source"); !h.Running || h.StartedAt.IsZero() {
		t.Fatal("missing active poll")
	}
	rt.FinishPoll("source", nil)
	success := rt.PollHealth("source").LastSuccess
	rt.StartPoll("source")
	rt.FinishPoll("source", errors.New("private error"))
	if h := rt.PollHealth("source"); h.Running || !h.Failed || !h.LastSuccess.Equal(success) {
		t.Fatalf("failed poll lost evidence: %+v", h)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			rt.StartPoll("source")
			rt.FinishPoll("source", nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			rt.PollHealth("source")
		}
	}()
	wg.Wait()
	if rt.PollHealth("source").Failed {
		t.Fatal("successful poll must clear failure")
	}
}

func TestD1LatencyPersistsAndLogsWithoutPayload(t *testing.T) {
	// A local native-protocol fixture exercises the same path as a journal
	// event without depending on d1 credentials, hardware, or the internet.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		io.WriteString(w, `{"model":"test-d1","answers":{"attention":{"type":"noul","noul":0.1}}}`)
	}))
	defer server.Close()
	rt := newTestRuntime(t, server.URL)
	rt.ModelCfg = model.Config{Provider: "liquid-d1", BaseURL: server.URL, Model: "test-d1"}
	rt.Prompt = ""
	rt.Schema = nil
	rt.Questions = model.Questions{"attention": {Type: "yes_no", Instructions: "Does this need attention?"}}
	rt.Policy = policy.Policy{Default: policy.Then{Ignore: true}}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	e := testEvent("latency")
	e.Data = map[string]any{"message": "private-event-message"}
	resp, err := rt.Ingest(context.Background(), e)
	if err != nil || resp.Decision.Status != "ok" || resp.Decision.LatencyMs < 15 {
		t.Fatalf("response: %+v %v", resp, err)
	}
	ins, err := rt.DB.Inspect(e.ID)
	if err != nil || ins.Decisions[0].LatencyMs != resp.Decision.LatencyMs {
		t.Fatalf("stored timing: %+v %v", ins, err)
	}
	if !strings.Contains(logs.String(), "latency_ms=") || !strings.Contains(logs.String(), e.ID) || strings.Contains(logs.String(), "private-event-message") || strings.Contains(logs.String(), server.URL) {
		t.Fatalf("unsafe or missing timing log: %s", logs.String())
	}
}
