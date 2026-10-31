package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Dashboard queries return bounded projections, never entire payloads in the
// timeline. Detail stays in InspectContext; none of these readers writes state.
const DashboardWindow = 1000
const DashboardPageSize = 50

type EventSummary struct {
	ID              string `json:"id"`
	Source          string `json:"source"`
	Type            string `json:"type"`
	Subject         string `json:"subject"`
	IngestedAt      string `json:"ingested_at"`
	EventTime       string `json:"event_time"`
	Message         string `json:"message"`
	DecisionStatus  string `json:"decision_status"`
	Model           string `json:"model"`
	LatencyMs       *int64 `json:"latency_ms"`
	Outcome         string `json:"outcome"`
	ExecutionStatus string `json:"execution_status"`
	Labeled         bool   `json:"labeled"`
}

type EventPage struct {
	Events []EventSummary `json:"events"`
	More   bool           `json:"more"`
	Before string         `json:"before"`
}

// Search uses bound values and instr rather than LIKE, so user text is literal.
// Views use a fixed allowlist; no client-supplied SQL fragments reach the query.
func (d *DB) DashboardEvents(ctx context.Context, query, view string, offset int) (EventPage, error) {
	return d.DashboardEventsAt(ctx, query, view, offset, "")
}

// A fixed receive-time boundary keeps new ingestion from shifting an operator's
// pages. It does not freeze later Decisions or labels for existing Events.
func (d *DB) DashboardEventsAt(ctx context.Context, query, view string, offset int, before string) (EventPage, error) {
	out := EventPage{Events: []EventSummary{}}
	if before == "" {
		before = time.Now().UTC().Format(time.RFC3339Nano)
	}
	boundary, err := time.Parse(time.RFC3339Nano, before)
	if err != nil {
		return out, fmt.Errorf("invalid snapshot time")
	}
	out.Before = boundary.UTC().Format(time.RFC3339Nano)
	// Stored RFC3339Nano values omit trailing zeros. Pad fractional seconds
	// before comparison; SQLite's date functions otherwise round to milliseconds.
	before = boundary.UTC().Format("2006-01-02T15:04:05.000000000")
	views := map[string]string{
		"all": "1", "unlabeled": "l.event_id IS NULL",
		"shadow": "x.status = 'shadow'", "ignore": "x.outcome = 'ignore'",
		"pending": "x.status = 'pending'",
		"errors":  "(d.status IN ('errored','invalid') OR x.status = 'error')",
	}
	where, ok := views[view]
	if !ok || offset < 0 || offset > 100000 || len(query) > 200 {
		return out, fmt.Errorf("invalid event query")
	}
	rows, err := d.QueryContext(ctx, `SELECT e.id,e.source,e.type,e.subject,e.ingest_time,e.event_time,
		COALESCE(json_extract(e.data,'$.message'),''),
		COALESCE(d.status,''),COALESCE(d.model,''),d.latency_ms,
		COALESCE(x.outcome,''),COALESCE(x.status,''),l.event_id IS NOT NULL
		FROM events e
		LEFT JOIN decisions d ON d.id=(SELECT id FROM decisions WHERE event_id=e.id ORDER BY created_at DESC,id DESC LIMIT 1)
		LEFT JOIN executions x ON x.id=(SELECT id FROM executions WHERE event_id=e.id ORDER BY created_at DESC,id DESC LIMIT 1)
		LEFT JOIN event_labels l ON l.event_id=e.id
		WHERE `+where+` AND (substr(e.ingest_time,1,19) || '.' || substr(substr(rtrim(e.ingest_time,'Z'),21) || '000000000',1,9))<=? AND (?='' OR instr(lower(e.subject || ' ' || e.source || ' ' || e.type || ' ' || e.id || ' ' || COALESCE(e.data,'')),lower(?))>0)
		ORDER BY e.ingest_time DESC,e.id DESC LIMIT ? OFFSET ?`, before, query, query, DashboardPageSize+1, offset)
	if err != nil {
		return out, fmt.Errorf("dashboard events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e EventSummary
		if err := rows.Scan(&e.ID, &e.Source, &e.Type, &e.Subject, &e.IngestedAt, &e.EventTime, &e.Message, &e.DecisionStatus, &e.Model, &e.LatencyMs, &e.Outcome, &e.ExecutionStatus, &e.Labeled); err != nil {
			return out, fmt.Errorf("scan dashboard event: %w", err)
		}
		out.Events = append(out.Events, e)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("read dashboard events: %w", err)
	}
	out.More = len(out.Events) > DashboardPageSize
	if out.More {
		out.Events = out.Events[:DashboardPageSize]
	}
	return out, nil
}

type ModelTiming struct {
	Interpreter string `json:"interpreter"`
	Model       string `json:"model"`
	Count       int    `json:"count"`
	Successes   int    `json:"successes"`
	Errors      int    `json:"errors"`
	Timeouts    int    `json:"timeouts"`
	MedianMs    *int64 `json:"median_ms"`
	P95Ms       *int64 `json:"p95_ms"`
	LastAt      string `json:"last_at"`
	LastStatus  string `json:"last_status"`
	LastMs      int64  `json:"last_ms"`
	LastSuccess string `json:"last_success"`
	LastError   string `json:"last_error"`
}

type Repeat struct {
	Source  string `json:"source"`
	Subject string `json:"subject"`
	Message string `json:"message"`
	Count   int    `json:"count"`
}

type DashboardSummary struct {
	Events    int           `json:"events"`
	Unlabeled int           `json:"unlabeled"`
	Shadow    int           `json:"shadow"`
	Ignored   int           `json:"ignored"`
	Pending   int           `json:"pending"`
	Window    int           `json:"window"`
	Models    []ModelTiming `json:"models"`
	Repeats   []Repeat      `json:"repeats"`
	Delivered []Delivery    `json:"delivered"`
}

// Delivery is stored evidence that a notify outcome ran for a source. Only a real
// attempt counts: shadow and pending rows prove nothing about the adapter.
type Delivery struct {
	Source     string `json:"source"`
	Sent       int    `json:"sent"`
	LastSent   string `json:"last_sent"`
	LastAt     string `json:"last_at"`
	LastStatus string `json:"last_status"`
}

func (d *DB) dashboardDeliveries(ctx context.Context) ([]Delivery, error) {
	rows, err := d.QueryContext(ctx, `SELECT e.source, x.status, x.created_at FROM executions x
		JOIN events e ON e.id=x.event_id
		WHERE x.outcome='notify' AND x.status IN ('ok','error') ORDER BY x.created_at`)
	if err != nil {
		return nil, fmt.Errorf("dashboard deliveries: %w", err)
	}
	defer rows.Close()
	out, index := []Delivery{}, map[string]int{}
	for rows.Next() {
		var source, status, at string
		if err := rows.Scan(&source, &status, &at); err != nil {
			return nil, fmt.Errorf("dashboard deliveries: %w", err)
		}
		i, ok := index[source]
		if !ok {
			i = len(out)
			index[source] = i
			out = append(out, Delivery{Source: source})
		}
		out[i].LastAt, out[i].LastStatus = at, status
		if status == "ok" {
			out[i].Sent++
			out[i].LastSent = at
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dashboard deliveries: %w", err)
	}
	return out, nil
}

func (d *DB) DashboardSummary(ctx context.Context) (DashboardSummary, error) {
	s := DashboardSummary{Window: DashboardWindow, Models: []ModelTiming{}, Repeats: []Repeat{}}
	if err := d.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM events),
		(SELECT count(*) FROM events e WHERE NOT EXISTS(SELECT 1 FROM event_labels l WHERE l.event_id=e.id)),
		(SELECT count(*) FROM executions WHERE status='shadow'),
		(SELECT count(*) FROM executions WHERE outcome='ignore'),
		(SELECT count(*) FROM executions WHERE status='pending')`).Scan(&s.Events, &s.Unlabeled, &s.Shadow, &s.Ignored, &s.Pending); err != nil {
		return s, fmt.Errorf("dashboard counts: %w", err)
	}
	var err error
	if s.Delivered, err = d.dashboardDeliveries(ctx); err != nil {
		return s, err
	}
	rows, err := d.QueryContext(ctx, `SELECT interpreter,model,status,latency_ms,created_at,COALESCE(error,'') FROM decisions ORDER BY created_at DESC,id DESC LIMIT ?`, DashboardWindow)
	if err != nil {
		return s, fmt.Errorf("dashboard timings: %w", err)
	}
	// Release the sole SQLite connection before querying repetitions. Bounded
	// samples keep percentile memory fixed even after years of collection.
	indices := map[[2]string]int{}
	timings := map[int][]int64{}
	for rows.Next() {
		var interpreter, model, status, at, detail string
		var ms int64
		if err = rows.Scan(&interpreter, &model, &status, &ms, &at, &detail); err != nil {
			break
		}
		key := [2]string{interpreter, model}
		i, exists := indices[key]
		if !exists {
			i = len(s.Models)
			indices[key] = i
			s.Models = append(s.Models, ModelTiming{Interpreter: interpreter, Model: model, LastAt: at, LastStatus: status, LastMs: ms})
		}
		m := &s.Models[i]
		m.Count++
		if status == "ok" {
			m.Successes++
			timings[i] = append(timings[i], ms)
			if m.LastSuccess == "" {
				m.LastSuccess = at
			}
		} else {
			m.Errors++
			if m.LastError == "" {
				m.LastError = at
			}
			// Historical decisions predate structured failure kinds. Count only
			// recorded timeout/deadline errors, never infer failure from latency.
			if IsTimeout(detail) {
				m.Timeouts++
			}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return s, fmt.Errorf("read dashboard timings: %w", err)
	}
	for i := range s.Models {
		values := timings[i]
		sort.Slice(values, func(a, b int) bool { return values[a] < values[b] })
		if len(values) > 0 {
			// Nearest-rank percentiles are defined even for a single observation.
			median, p95 := values[(len(values)-1)/2], values[(95*len(values)+99)/100-1]
			s.Models[i].MedianMs, s.Models[i].P95Ms = &median, &p95
		}
	}
	rows, err = d.QueryContext(ctx, `SELECT source,subject,message,count(*) n FROM
		(SELECT source,type,subject,COALESCE(json_extract(data,'$.message'),'') message FROM events ORDER BY ingest_time DESC,id DESC LIMIT ?)
		WHERE message!='' GROUP BY source,type,subject,message HAVING count(*)>1 ORDER BY n DESC,source,subject LIMIT 10`, DashboardWindow)
	if err != nil {
		return s, fmt.Errorf("dashboard repeats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r Repeat
		if err := rows.Scan(&r.Source, &r.Subject, &r.Message, &r.Count); err != nil {
			return s, fmt.Errorf("scan repeat: %w", err)
		}
		s.Repeats = append(s.Repeats, r)
	}
	if err := rows.Err(); err != nil {
		return s, fmt.Errorf("read repeats: %w", err)
	}
	return s, nil
}

func IsTimeout(detail string) bool {
	detail = strings.ToLower(detail)
	return strings.Contains(detail, "timeout") || strings.Contains(detail, "deadline exceeded")
}
