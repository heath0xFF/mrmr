package source

import (
	"context"
	"testing"
)

func TestJournalEmptyPollAndFailureHealth(t *testing.T) {
	j := testJournal(t)
	modelURL, calls := modelMock(t)
	rt := testRuntime(t, modelURL)
	ctx := context.Background()
	if err := j.prepare(ctx, rt.DB); err != nil {
		t.Fatal(err)
	}
	// No records after the checkpoint: the cursor does not advance, but the
	// completed poll is still fresh evidence of a working source.
	if err := j.poll(ctx, rt); err != nil {
		t.Fatal(err)
	}
	h := rt.PollHealth(j.cfg.Name)
	if h.Failed || h.Running || h.LastSuccess.IsZero() || calls.Load() != 0 {
		t.Fatalf("empty poll: %+v", h)
	}
	journalFile(t, j, "fail", "")
	if err := j.poll(ctx, rt); err == nil {
		t.Fatal("expected journal failure")
	}
	failed := rt.PollHealth(j.cfg.Name)
	if !failed.Failed || failed.Running || !failed.LastSuccess.Equal(h.LastSuccess) {
		t.Fatalf("failure: %+v", failed)
	}
}

func TestHTTPPollHealthPartialAndEmptyBatches(t *testing.T) {
	modelURL, _ := modelMock(t)
	rt := testRuntime(t, modelURL)
	feedURL, _ := feedServer(t, `{"items":[null]}`, `{"items":[]}`)
	p, err := NewHTTPPoller(pollerCfg(feedURL))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.poll(context.Background(), rt); err == nil {
		t.Fatal("partial batch must report failure")
	}
	if h := rt.PollHealth(p.cfg.Name); !h.Failed || h.Running || !h.LastSuccess.IsZero() {
		t.Fatalf("partial health: %+v", h)
	}
	if err := p.poll(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	if h := rt.PollHealth(p.cfg.Name); h.Failed || h.Running || h.LastSuccess.IsZero() {
		t.Fatalf("empty health: %+v", h)
	}
	rt.DB.Close()
	if err := p.poll(context.Background(), rt); err == nil {
		t.Fatal("cursor read failure must not look healthy")
	}
	if !rt.PollHealth(p.cfg.Name).Failed {
		t.Fatal("storage failure not reported")
	}
}
