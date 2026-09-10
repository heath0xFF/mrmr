// Poller tests run the full loop against real HTTP servers: a feed server
// standing in for the polled target and the standard mock OpenAI-compatible
// model behind a real SQLite database. The invariants that matter: items
// become events exactly once across repeated polls (dedup, not the cursor,
// is the correctness mechanism), the cursor is persisted and substituted
// into subsequent fetches, and malformed items are skipped without killing
// the poll.
package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
	"github.com/heath0xff/mrmr/internal/runtime"
	"github.com/heath0xff/mrmr/internal/storage"
)

// modelMock returns the standard canned OpenAI-compatible response. Every
// ingested item scores unimportant so the pipeline ends in ignore and the
// tests stay quiet.
func modelMock(t *testing.T) (url string, calls *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": `{"category":"unimportant","importance":0.1}`},
			}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

func testRuntime(t *testing.T, modelURL string) *runtime.Runtime {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "poller.db"))
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &runtime.Runtime{
		DB:       db,
		Client:   &model.Client{},
		ModelCfg: model.Config{Provider: "openai-compatible", BaseURL: modelURL, Model: "mock-model"},
		ModelKey: "mock",
		Prompt:   "classify this event",
		Schema: model.Schema{
			"category":   {Type: "string", Enum: []any{"unimportant"}},
			"importance": {Type: "number"},
		},
		Policy: mustPolicy(),
	}
}

// mustPolicy keeps the struct literal out of every test runtime construction.
func mustPolicy() policy.Policy {
	return policy.Policy{Default: policy.Then{Ignore: true}}
}

func pollerCfg(url string) config.Source {
	return config.Source{
		Name:        "feed",
		Type:        "http-poller",
		URL:         url,
		Every:       time.Minute, // Run is never entered in tests; poll is called directly
		ItemPath:    "items",
		CursorField: "id",
		EventType:   "feed.item.published",
	}
}

func feedServer(t *testing.T, pages ...string) (url string, gotQuery *atomic.Value) {
	t.Helper()
	var n atomic.Int32
	var q atomic.Value
	q.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		q.Store(r.URL.RawQuery)
		page := pages[len(pages)-1]
		if i < len(pages) {
			page = pages[i]
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, page)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &q
}

func countEvents(t *testing.T, rt *runtime.Runtime) int {
	t.Helper()
	var n int
	if err := rt.DB.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// TestPollerIngestsEachItemOnce is the core loop: two items become two
// events with a model call each; repolling the same feed adds nothing
// because dedup drops them before the model; a changed feed adds only the
// new item. The cursor must be persisted across the polls.
func TestPollerIngestsEachItemOnce(t *testing.T) {
	modelURL, modelCalls := modelMock(t)
	feedURL, _ := feedServer(t, `{"items":[
		{"id":"a","title":"first"},
		{"id":"b","title":"second"}]}`,
		`{"items":[
		{"id":"a","title":"first"},
		{"id":"b","title":"second"},
		{"id":"c","title":"third"}]}`)

	rt := testRuntime(t, modelURL)
	p, err := NewHTTPPoller(pollerCfg(feedURL))
	if err != nil {
		t.Fatalf("NewHTTPPoller: %v", err)
	}
	ctx := context.Background()

	p.poll(ctx, rt)
	if got := countEvents(t, rt); got != 2 {
		t.Errorf("events after first poll = %d, want 2", got)
	}
	if modelCalls.Load() != 2 {
		t.Errorf("model calls after first poll = %d, want 2", modelCalls.Load())
	}
	if c, _ := rt.DB.Cursor("feed"); c != "b" {
		t.Errorf("cursor after first poll = %q, want b", c)
	}

	p.poll(ctx, rt)
	if got := countEvents(t, rt); got != 3 {
		t.Errorf("events after second poll = %d, want 3 (only the new item)", got)
	}
	// Dedup happens at InsertEvent, before any model call — repolling a
	// known feed must cost no inference.
	if modelCalls.Load() != 3 {
		t.Errorf("model calls after second poll = %d, want 3", modelCalls.Load())
	}
	if c, _ := rt.DB.Cursor("feed"); c != "c" {
		t.Errorf("cursor after second poll = %q, want c", c)
	}
}

// TestPollerSubstitutesCursorIntoURL pins the cursor-based-API case: the
// first fetch has no cursor, later fetches carry the persisted one.
func TestPollerSubstitutesCursorIntoURL(t *testing.T) {
	modelURL, _ := modelMock(t)
	feedURL, gotQuery := feedServer(t, `{"items":[{"id":"a"}]}`)

	rt := testRuntime(t, modelURL)
	cfg := pollerCfg(feedURL + "?after={{ .cursor }}")
	p, err := NewHTTPPoller(cfg)
	if err != nil {
		t.Fatalf("NewHTTPPoller: %v", err)
	}

	p.poll(context.Background(), rt)
	if q := gotQuery.Load().(string); q != "after=" {
		t.Errorf("first poll query = %q, want empty after", q)
	}
	p.poll(context.Background(), rt)
	if q := gotQuery.Load().(string); q != "after=a" {
		t.Errorf("second poll query = %q, want after=a", q)
	}
}

// TestPollerNormalizesItemFields covers the mapping knobs: subject and
// RFC3339 timestamp come from the item, source_event_id comes from the
// cursor field, and the whole item is preserved as data.
func TestPollerNormalizesItemFields(t *testing.T) {
	modelURL, _ := modelMock(t)
	feedURL, _ := feedServer(t, `{"items":[
		{"id":"x1","title":"Disk full","published":"2026-09-07T12:00:00Z","disk":"nvme0"}]}`)

	rt := testRuntime(t, modelURL)
	cfg := pollerCfg(feedURL)
	cfg.SubjectField = "title"
	cfg.TimestampField = "published"
	p, err := NewHTTPPoller(cfg)
	if err != nil {
		t.Fatalf("NewHTTPPoller: %v", err)
	}

	p.poll(context.Background(), rt)
	// QueryRow, not Query: the pool is a single connection (SetMaxOpenConns(1)),
	// so an open rows iterator would deadlock Inspect below.
	var id string
	if err := rt.DB.QueryRow(`SELECT id FROM events WHERE source = 'feed' LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("query event: %v", err)
	}
	ins, err := rt.DB.Inspect(id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	e := ins.Event
	if e.Subject != "Disk full" {
		t.Errorf("subject = %q, want Disk full", e.Subject)
	}
	if e.Type != "feed.item.published" {
		t.Errorf("type = %q, want feed.item.published", e.Type)
	}
	if want := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC); !e.Timestamp.Equal(want) {
		t.Errorf("timestamp = %v, want item's %v", e.Timestamp, want)
	}
	if e.Data["disk"] != "nvme0" {
		t.Errorf("data = %v, want the whole item preserved", e.Data)
	}
}

// TestPollerSkipsItemsWithoutCursorField pins the fail-closed dedup rule:
// when cursor_field is configured, an item without it cannot be identified
// and must not be ingested (payload-hash dedup would not catch a changing
// payload, so ingest would duplicate).
func TestPollerSkipsItemsWithoutCursorField(t *testing.T) {
	modelURL, modelCalls := modelMock(t)
	feedURL, _ := feedServer(t, `{"items":[
		{"title":"no id here"},
		{"id":"ok","title":"fine"}]}`)

	rt := testRuntime(t, modelURL)
	p, err := NewHTTPPoller(pollerCfg(feedURL))
	if err != nil {
		t.Fatalf("NewHTTPPoller: %v", err)
	}

	p.poll(context.Background(), rt)
	if got := countEvents(t, rt); got != 1 {
		t.Errorf("events = %d, want 1 (only the identified item)", got)
	}
	if modelCalls.Load() != 1 {
		t.Errorf("model calls = %d, want 1", modelCalls.Load())
	}
}

// TestPollerFetchFailureKeepsState: a dead target must not crash, ingest
// nothing, or move the cursor; the next tick retries.
func TestPollerFetchFailureKeepsState(t *testing.T) {
	modelURL, _ := modelMock(t)
	rt := testRuntime(t, modelURL)
	rt.DB.SetCursor("feed", "old")

	p, err := NewHTTPPoller(pollerCfg("http://127.0.0.1:1/feed"))
	if err != nil {
		t.Fatalf("NewHTTPPoller: %v", err)
	}
	p.poll(context.Background(), rt) // port 1: connection refused

	if got := countEvents(t, rt); got != 0 {
		t.Errorf("events = %d, want 0", got)
	}
	if c, _ := rt.DB.Cursor("feed"); c != "old" {
		t.Errorf("cursor = %q, want unchanged old", c)
	}
}

// TestPollerIncompleteBatchKeepsCursor exercises failures between two valid
// records. Advancing to either successful record could hide the failed one
// from an incremental API, so the whole batch must retain its old watermark.
func TestPollerIncompleteBatchKeepsCursor(t *testing.T) {
	for _, tc := range []struct {
		name string
		item string
	}{
		{"event write failure", `{"id":"c"}`},
		{"non-object", `null`},
		{"missing id", `{"title":"missing id"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modelURL, modelCalls := modelMock(t)
			feedURL, gotQuery := feedServer(t,
				`{"items":[{"id":"b"},`+tc.item+`,{"id":"d"}]}`,
				`{"items":[{"id":"b"},{"id":"c"},{"id":"d"}]}`)
			rt := testRuntime(t, modelURL)
			if err := rt.DB.SetCursor("feed", "a"); err != nil {
				t.Fatal(err)
			}
			if tc.name == "event write failure" {
				// A SQLite trigger fails just the middle insert without a mock
				// storage layer or making the cursor table unwritable too.
				if _, err := rt.DB.Exec(`CREATE TRIGGER fail_event BEFORE INSERT ON events
					WHEN NEW.dedup_key = 'c' BEGIN SELECT RAISE(FAIL, 'test failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			p, err := NewHTTPPoller(pollerCfg(feedURL + "?after={{ .cursor }}"))
			if err != nil {
				t.Fatal(err)
			}
			p.poll(context.Background(), rt)
			if c, err := rt.DB.Cursor("feed"); err != nil || c != "a" {
				t.Errorf("cursor after incomplete batch = %q, %v, want a", c, err)
			}
			if got := countEvents(t, rt); got != 2 {
				t.Errorf("events = %d, want both valid records stored", got)
			}
			if modelCalls.Load() != 2 {
				t.Errorf("model calls = %d, want 2", modelCalls.Load())
			}

			// The next page corrects malformed data; removing the trigger
			// simulates recovery from a transient event persistence failure.
			if _, err := rt.DB.Exec(`DROP TRIGGER IF EXISTS fail_event`); err != nil {
				t.Fatal(err)
			}
			p.poll(context.Background(), rt)
			if q := gotQuery.Load().(string); q != "after=a" {
				t.Errorf("retry query = %q, want original after=a", q)
			}
			if c, err := rt.DB.Cursor("feed"); err != nil || c != "d" {
				t.Errorf("cursor after recovery = %q, %v, want d", c, err)
			}
			if got := countEvents(t, rt); got != 3 {
				t.Errorf("events after recovery = %d, want 3", got)
			}
			if modelCalls.Load() != 3 {
				t.Errorf("model calls after recovery = %d, want 3 (successful records dedup)", modelCalls.Load())
			}
		})
	}
}

// TestPollerCanceledLastItemKeepsCursor covers cancellation inside Ingest:
// there is no next loop iteration to notice it, and a canceled model call
// is recorded as an errored decision rather than returned as an ingest error.
func TestPollerCanceledLastItemKeepsCursor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
	}))
	defer modelServer.Close()
	feedURL, _ := feedServer(t, `{"items":[{"id":"b"}]}`)
	rt := testRuntime(t, modelServer.URL)
	if err := rt.DB.SetCursor("feed", "a"); err != nil {
		t.Fatal(err)
	}
	p, err := NewHTTPPoller(pollerCfg(feedURL))
	if err != nil {
		t.Fatal(err)
	}
	p.poll(ctx, rt)
	if c, err := rt.DB.Cursor("feed"); err != nil || c != "a" {
		t.Errorf("cursor after cancellation = %q, %v, want a", c, err)
	}
}

// TestCursorSurvivesReopen pins the restart-safety story: the watermark
// lives in SQLite, so a restarted runtime resumes where it left off.
func TestCursorSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.db")
	db1, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db1.SetCursor("feed", "42"); err != nil {
		t.Fatal(err)
	}
	db1.Close()

	db2, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if c, err := db2.Cursor("feed"); err != nil || c != "42" {
		t.Errorf("cursor after reopen = %q, %v, want 42", c, err)
	}
	if c, err := db2.Cursor("unknown"); err != nil || c != "" {
		t.Errorf("unknown source cursor = %q, %v, want empty, nil", c, err)
	}
}

// TestRunStopsOnContextCancel is the goroutine-contract check: Run owns one
// goroutine, exits when ctx is canceled, and does it promptly.
func TestRunStopsOnContextCancel(t *testing.T) {
	modelURL, _ := modelMock(t)
	feedURL, _ := feedServer(t, `{"items":[]}`)
	rt := testRuntime(t, modelURL)
	cfg := pollerCfg(feedURL)
	cfg.Every = 50 * time.Millisecond // fast ticks; feed returns no items
	p, err := NewHTTPPoller(cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx, rt); close(done) }()
	time.Sleep(120 * time.Millisecond) // let at least one tick pass
	cancel()

	select {
	case <-done:
		// stopped promptly
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancel")
	}
}
