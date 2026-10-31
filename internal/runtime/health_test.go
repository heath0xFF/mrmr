package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
	"net/http"
	"net/http/httptest"
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

// The ring must stay bounded in count and size, because a chatty service would
// otherwise grow memory for as long as the runtime runs.
func TestRecentSeenIsBoundedAndNewestFirst(t *testing.T) {
	r := &Runtime{}
	if got := r.RecentSeen("none"); got == nil || len(got) != 0 {
		t.Fatalf("empty result must be a non-nil array: %#v", got)
	}
	for i := 0; i < seenRecordLimit+5; i++ {
		r.RecordSeen("s", SeenRecord{Priority: i, Message: strings.Repeat("é", seenMessageLimit)})
	}
	got := r.RecentSeen("s")
	if len(got) != seenRecordLimit || got[0].Priority != seenRecordLimit+4 {
		t.Fatalf("len %d newest %d", len(got), got[0].Priority)
	}
	if len(got[0].Message) > seenMessageLimit || !utf8.ValidString(got[0].Message) {
		t.Fatalf("message not safely bounded: %d bytes", len(got[0].Message))
	}
	r.RecordPoll("s", PollCounts{Scanned: 3, Watched: 2, Selected: 1, Backlogged: true})
	r.RecordPoll("s", PollCounts{Scanned: 4})
	h := r.PollHealth("s")
	if h.Polls != 2 || h.Scanned != 7 || h.Watched != 2 || h.LastScanned != 4 || h.LastWatched != 0 || h.Backlogged {
		t.Fatalf("health = %+v", h)
	}
}
