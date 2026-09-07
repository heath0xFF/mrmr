// Package source hosts source adapters: the code that watches the outside
// world and emits normalized Events. v0.1's only ingress was POST /api/events;
// the HTTP poller adds pull-based ingestion for feeds and APIs that have no
// webhooks. A poller is deliberately dumb: it fetches, normalizes items into
// Events, and hands each one to the ordinary Ingest pipeline — it never
// interprets, filters, or routes.
//
// Concurrency contract: one goroutine per source, owned by Run, bounded by
// the context, exited when the context is canceled. Polls within one source
// are strictly serial, so cursor state needs no locking beyond SQLite's.
package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/runtime"
	"github.com/heath0xff/mrmr/internal/storage"
)

// HTTPPoller polls a JSON endpoint on an interval and ingests each record
// as an Event. Cursor semantics are lazy on purpose: every item is offered
// to InsertEvent on every poll and dedup drops what was seen, so correctness
// never depends on ordering assumptions. The persisted cursor exists for
// cursor-based APIs (URL substitution) and as a restart watermark, not as
// the dedup mechanism.
type HTTPPoller struct {
	cfg    config.Source
	urlTpl *template.Template
	client *http.Client
	// bearer is resolved once from the environment at construction; it is
	// never persisted, logged, or copied anywhere but the request header.
	bearer string
}

// NewHTTPPoller validates and prepares a poller. The bearer token is read
// from the environment once, here: it must exist at startup (fail loudly)
// and must never be persisted or logged.
func NewHTTPPoller(cfg config.Source) (*HTTPPoller, error) {
	tpl, err := template.New("url").Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("source %s: bad url template: %w", cfg.Name, err)
	}
	bearer, err := bearerFromEnv(cfg.BearerTokenEnv)
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", cfg.Name, err)
	}
	return &HTTPPoller{
		cfg:    cfg,
		urlTpl: tpl,
		// Outbound polls must be bounded: a hung target must not stall the
		// poll loop's single goroutine forever.
		client: &http.Client{Timeout: 15 * time.Second},
		bearer: bearer,
	}, nil
}

// Run polls until ctx is canceled. The first poll is immediate so a fresh
// config produces data without waiting one interval.
func (p *HTTPPoller) Run(ctx context.Context, rt *runtime.Runtime) {
	ticker := time.NewTicker(p.cfg.Every)
	defer ticker.Stop()

	p.poll(ctx, rt)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(ctx, rt)
		}
	}
}

// poll is one fetch-normalize-ingest cycle. Every error path is a log line,
// never a crash: a broken feed must not take down the runtime, and the next
// tick retries anyway.
func (p *HTTPPoller) poll(ctx context.Context, rt *runtime.Runtime) {
	// If the fetch itself is canceled by shutdown, nothing was ingested and
	// the cursor is unchanged — safe to abandon mid-flight.
	items, err := p.fetch(ctx, rt.DB)
	if err != nil {
		log.Printf("mrmr: poll %s: fetch failed: %v", p.cfg.Name, err)
		return
	}

	var ingested, dups, cursorAdvanced int
	newCursor := ""
	for i, item := range items {
		if ctx.Err() != nil {
			return // shutdown: remaining items wait for the next poll
		}
		obj, ok := item.(map[string]any)
		if !ok {
			log.Printf("mrmr: poll %s: item %d is not an object, skipped", p.cfg.Name, i)
			continue
		}
		if c := cursorValue(obj, p.cfg.CursorField); c != "" {
			if c > newCursor { // ponytail: string max; numeric ordering only matters for {{.cursor}} substitution
				newCursor = c
			}
		}
		e, err := p.normalize(obj)
		if err != nil {
			log.Printf("mrmr: poll %s: item %d: %v", p.cfg.Name, i, err)
			continue
		}
		resp, err := rt.Ingest(ctx, e)
		if err != nil {
			log.Printf("mrmr: poll %s: ingest %s failed: %v", p.cfg.Name, e.ID, err)
			continue
		}
		if resp.Duplicate {
			dups++
		} else {
			ingested++
		}
	}

	// Advance the watermark only after the batch is offered: a crash
	// mid-poll re-polls the same window, and dedup makes that harmless.
	if newCursor != "" && newCursor != p.cursor(rt.DB) {
		if err := rt.DB.SetCursor(p.cfg.Name, newCursor); err != nil {
			log.Printf("mrmr: poll %s: persist cursor failed: %v", p.cfg.Name, err)
		} else {
			cursorAdvanced = 1
		}
	}
	log.Printf("mrmr: poll %s: %d items: %d new, %d duplicate, cursor=%s",
		p.cfg.Name, len(items), ingested, dups, map[bool]string{true: newCursor, false: "unchanged"}[cursorAdvanced == 1])
}

// fetch retrieves and extracts the item array. The cursor is substituted
// into the URL template when present so cursor-based APIs page correctly;
// plain feeds ignore it.
func (p *HTTPPoller) fetch(ctx context.Context, db *storage.DB) ([]any, error) {
	var urlBuf bytes.Buffer
	if err := p.urlTpl.Execute(&urlBuf, map[string]any{"cursor": p.cursor(db)}); err != nil {
		return nil, fmt.Errorf("render url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlBuf.String(), nil)
	if err != nil {
		return nil, err
	}
	if p.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+p.bearer)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Feeds are small; 4 MiB caps a hostile or misconfigured target.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	items := doc
	if p.cfg.ItemPath != "" {
		items = doc
		for _, part := range strings.Split(p.cfg.ItemPath, ".") {
			m, ok := items.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("item_path %q: %s is not an object", p.cfg.ItemPath, part)
			}
			items = m[part]
		}
	}
	list, ok := items.([]any)
	if !ok {
		if items == nil {
			return nil, nil // absent path is an empty poll, not an error
		}
		return nil, fmt.Errorf("item_path %q did not resolve to an array", p.cfg.ItemPath)
	}
	return list, nil
}

// normalize turns one fetched record into an Event. The whole record becomes
// data — the core makes no assumptions about source payloads, and dropping
// fields here would silently erase exactly the context the interpreter needs.
// The cursor field doubles as source_event_id, which is what makes dedup work.
func (p *HTTPPoller) normalize(item map[string]any) (event.Event, error) {
	e := event.Event{
		ID:        event.NewID("evt_"),
		Type:      p.cfg.EventType,
		Source:    p.cfg.Name,
		Timestamp: time.Now().UTC(),
		Data:      item,
	}
	if p.cfg.SubjectField != "" {
		if v, ok := item[p.cfg.SubjectField]; ok {
			e.Subject = fmt.Sprint(v)
		}
	}
	if p.cfg.TimestampField != "" {
		if v, ok := item[p.cfg.TimestampField].(string); ok {
			if ts, perr := time.Parse(time.RFC3339, v); perr == nil {
				e.Timestamp = ts.UTC()
			}
			// Unparseable timestamps keep "now": a wrong-but-present event
			// beats a dropped one for ambient feeds.
		}
	}
	if p.cfg.CursorField != "" {
		if c := cursorValue(item, p.cfg.CursorField); c != "" {
			e.Metadata = map[string]any{"source_event_id": c}
		} else {
			return e, fmt.Errorf("cursor_field %q missing; item cannot be deduplicated", p.cfg.CursorField)
		}
	}
	return e, nil
}

func cursorValue(item map[string]any, field string) string {
	if field == "" {
		return ""
	}
	if v, ok := item[field]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

// cursor reads the persisted watermark. An unknown source is a fresh poller:
// empty cursor, and the URL template renders without one.
func (p *HTTPPoller) cursor(db *storage.DB) string {
	c, err := db.Cursor(p.cfg.Name)
	if err != nil {
		log.Printf("mrmr: poll %s: read cursor: %v", p.cfg.Name, err)
	}
	return c
}

// bearerFromEnv resolves the bearer_token_env reference at startup.
func bearerFromEnv(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return "", fmt.Errorf("env %s (bearer_token_env) is not set", name)
	}
	return v, nil
}
