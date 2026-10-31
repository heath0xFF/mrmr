package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/storage"
)

type reviewInfo struct {
	ContractSupported bool                 `json:"contract_supported"`
	Categories        []string             `json:"categories"`
	State             string               `json:"state"`
	Origin            string               `json:"origin"`
	Updated           string               `json:"updated"`
	Label             *storage.EventLabel  `json:"label"`
	History           []storage.EventLabel `json:"history"`
	Revision          string               `json:"revision"`
}

// Browser review uses the existing flat category/action label contract. Do
// not fabricate that shape for unrelated structured question sets.
func reviewContract(spec config.Interpret) (bool, []string) {
	var categories []string
	if len(spec.Questions) > 0 {
		q, action := spec.Questions["category"], spec.Questions["requires_action"]
		if q.Type != "choice" || (action.Type != "noul" && action.Type != "yes_no") {
			return false, categories
		}
		return true, model.ChoiceOptions(q)
	}
	category, action := spec.Schema["category"], spec.Schema["requires_action"]
	if category.Type != "string" || action.Type != "boolean" {
		return false, categories
	}
	for _, value := range category.Enum {
		categories = append(categories, fmt.Sprint(value))
	}
	return true, categories
}

func (m *manager) review(id string) (reviewInfo, error) {
	out := reviewInfo{State: "unreviewed", Origin: "unknown", History: []storage.EventLabel{}}
	ins, err := m.db.Inspect(id)
	if err != nil {
		return out, fmt.Errorf("Event not found.")
	}
	out.Label = ins.Label
	if s := m.current.Load(); s != nil {
		spec := s.cfg.InterpreterForEvent(ins.Event)
		out.ContractSupported, out.Categories = reviewContract(spec)
	}
	err = m.db.QueryRow(`SELECT state,origin,updated_at FROM review_state WHERE event_id=?`, id).Scan(&out.State, &out.Origin, &out.Updated)
	if err != nil && err != sql.ErrNoRows {
		return out, fmt.Errorf("Cannot read review.")
	}
	rows, err := m.db.Query(`SELECT category,requires_action,expected_outcome,labeled_at FROM label_history WHERE event_id=? ORDER BY id DESC LIMIT 30`, id)
	if err != nil {
		return out, fmt.Errorf("Cannot read label history.")
	}
	defer rows.Close()
	for rows.Next() {
		l := storage.EventLabel{EventID: id}
		var at string
		if rows.Scan(&l.Category, &l.RequiresAction, &l.ExpectedOutcome, &at) != nil {
			return out, fmt.Errorf("Cannot decode history.")
		}
		l.LabeledAt, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return out, fmt.Errorf("Cannot decode label history time.")
		}
		out.History = append(out.History, l)
	}
	if rows.Err() != nil {
		return out, fmt.Errorf("Cannot read history.")
	}
	b, _ := json.Marshal([]any{out.Label, out.State, out.Origin, out.Updated})
	out.Revision = revision(string(b))
	return out, nil
}
func (m *manager) reviewRoute(w http.ResponseWriter, r *http.Request) error {
	switch {
	case r.Method == "GET" && r.URL.Path == "/manage/api/review":
		id := r.URL.Query().Get("id")
		if id == "" || len(id) > 200 {
			return fmt.Errorf("Event ID required.")
		}
		out, err := m.review(id)
		if err != nil {
			return err
		}
		return controlJSON(w, out)
	case r.Method == "POST" && r.URL.Path == "/manage/api/review":
		var req struct {
			ID       string `json:"id"`
			Expected string `json:"expected"`
			Category string `json:"category"`
			Action   *bool  `json:"requires_action"`
			Outcome  string `json:"outcome"`
			State    string `json:"state"`
			Origin   string `json:"origin"`
		}
		if err := decodeControl(w, r, &req); err != nil {
			return err
		}
		if req.State != "reviewed" && req.State != "deferred" && req.State != "uncertain" {
			return fmt.Errorf("Choose reviewed, deferred, or uncertain.")
		}
		if req.Origin != "real" && req.Origin != "synthetic" && req.Origin != "unknown" {
			return fmt.Errorf("Choose real, synthetic, or unknown origin.")
		}
		before, err := m.review(req.ID)
		if err != nil {
			return err
		}
		if before.Revision != req.Expected {
			http.Error(w, "Review changed. Reload before saving.", 409)
			return nil
		}
		if req.State == "reviewed" {
			if !before.ContractSupported {
				return fmt.Errorf("This interpretation contract does not support category and action review. Use uncertain or deferred instead.")
			}
			s := m.current.Load()
			if s == nil {
				return fmt.Errorf("Configure the interpreter before labeling.")
			}
			ins, err := m.db.Inspect(req.ID)
			if err != nil {
				return fmt.Errorf("Cannot read event.")
			}
			selected := *s.cfg
			selected.Interpret = s.cfg.InterpreterForEvent(ins.Event)
			req.Category = strings.TrimSpace(req.Category)
			if req.Action == nil || req.Category == "" || len(req.Category) > 200 {
				return fmt.Errorf("Category and expected action are required.")
			}
			if err := validateLabelCategory(&selected, req.Category); err != nil {
				return fmt.Errorf("Category is not supported by this event's interpretation contract.")
			}
			if req.Outcome != "ignore" && req.Outcome != "notify" && req.Outcome != "act" && req.Outcome != "delegate" {
				return fmt.Errorf("Invalid expected outcome.")
			}
		}
		tx, err := m.db.Begin()
		if err != nil {
			return fmt.Errorf("Cannot save review.")
		}
		defer tx.Rollback()
		// Recheck the CLI-owned label inside the write transaction. A browser token
		// must not overwrite a correction made through the CLI after its read.
		var labelAt, reviewAt string
		err = tx.QueryRow(`SELECT labeled_at FROM event_labels WHERE event_id=?`, req.ID).Scan(&labelAt)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("Cannot check current label.")
		}
		err = tx.QueryRow(`SELECT updated_at FROM review_state WHERE event_id=?`, req.ID).Scan(&reviewAt)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("Cannot check current review.")
		}
		expectedAt := ""
		if before.Label != nil {
			expectedAt = before.Label.LabeledAt.Format(time.RFC3339Nano)
		}
		if labelAt != expectedAt || reviewAt != before.Updated {
			http.Error(w, "Label changed. Reload before saving.", 409)
			return nil
		}
		at := time.Now().UTC().Format(time.RFC3339Nano)
		if req.State == "reviewed" {
			if _, err = tx.Exec(`INSERT INTO event_labels(event_id,category,requires_action,expected_outcome,labeled_at) VALUES(?,?,?,?,?) ON CONFLICT(event_id) DO UPDATE SET category=excluded.category,requires_action=excluded.requires_action,expected_outcome=excluded.expected_outcome,labeled_at=excluded.labeled_at`, req.ID, req.Category, *req.Action, req.Outcome, at); err != nil {
				return fmt.Errorf("Cannot write label.")
			}
		}
		if _, err = tx.Exec(`INSERT INTO review_state VALUES(?,?,?,?) ON CONFLICT(event_id) DO UPDATE SET state=excluded.state,origin=excluded.origin,updated_at=excluded.updated_at`, req.ID, req.State, req.Origin, at); err != nil {
			return fmt.Errorf("Cannot write review state.")
		}
		if _, err = tx.Exec(`INSERT INTO control_audit(at,actor,operation,target,result) VALUES(?,'operator','review',?,?)`, at, req.ID, req.State+"; origin="+req.Origin); err != nil {
			return fmt.Errorf("Cannot write review audit.")
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("Cannot commit review.")
		}
		return controlJSON(w, map[string]bool{"saved": true})
	case r.Method == "GET" && r.URL.Path == "/manage/api/evaluations":
		rows, err := m.db.Query(`SELECT id,at,revision,status,report FROM control_evaluations ORDER BY at DESC LIMIT 20`)
		if err != nil {
			return fmt.Errorf("Cannot read evaluations.")
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id, at, rev, status, report string
			if rows.Scan(&id, &at, &rev, &status, &report) != nil {
				return fmt.Errorf("Cannot decode evaluations.")
			}
			var data any
			if err := json.Unmarshal([]byte(report), &data); err != nil {
				return fmt.Errorf("A saved evaluation report cannot be decoded.")
			}
			out = append(out, map[string]any{"id": id, "at": at, "revision": rev, "status": status, "report": data})
		}
		if rows.Err() != nil {
			return fmt.Errorf("Cannot read evaluations.")
		}
		return controlJSON(w, out)
	case r.Method == "POST" && r.URL.Path == "/manage/api/evaluate":
		return m.startEvaluation(w, r)
	case r.Method == "POST" && r.URL.Path == "/manage/api/cancel-evaluation":
		var req struct{}
		if err := decodeControl(w, r, &req); err != nil {
			return err
		}
		m.cancelEvaluation()
		if err := m.audit("cancel-evaluation", "current", "requested"); err != nil {
			return fmt.Errorf("Cancellation requested, but audit failed.")
		}
		return controlJSON(w, map[string]bool{"cancel_requested": true})
	default:
		http.NotFound(w, r)
		return nil
	}
}

// A failed terminal write must be visible without logging event content or
// provider errors. Startup reconciles any remaining running intent as interrupted.
func (m *manager) finishEvaluation(id, status string) {
	if _, err := m.db.Exec(`UPDATE control_evaluations SET status=? WHERE id=?`, status, id); err != nil {
		log.Printf("mrmr: evaluation %s terminal status could not be persisted", id)
	}
}

func (m *manager) cancelEvaluation() {
	m.evalMu.Lock()
	defer m.evalMu.Unlock()
	if m.evalCancel != nil {
		m.evalCancel()
	}
}

type evaluationRow struct {
	EventID         string             `json:"event_id"`
	Label           storage.EventLabel `json:"label"`
	Category        string             `json:"category"`
	Outcome         string             `json:"outcome"`
	BaselineOutcome string             `json:"baseline_outcome"`
	Correct         bool               `json:"correct"`
	FalseIgnore     bool               `json:"false_ignore"`
	Status          string             `json:"status"`
	LatencyMs       int64              `json:"latency_ms"`
}

func (m *manager) startEvaluation(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		YAML    string `json:"yaml"`
		Mode    string `json:"mode"`
		Confirm bool   `json:"confirm"`
	}
	if err := decodeControl(w, r, &req); err != nil {
		return err
	}
	if !req.Confirm {
		return fmt.Errorf("Confirm evaluation scope and data destination.")
	}
	if req.Mode != "policy" && req.Mode != "interpreter" {
		return fmt.Errorf("Choose policy or interpreter evaluation.")
	}
	candidate, err := managedParse(req.YAML)
	if err != nil {
		return err
	}
	active := m.current.Load()
	if active == nil {
		return fmt.Errorf("Setup must complete before evaluation.")
	}
	// Model calls may use only endpoints already authorized by active config;
	// a draft cannot turn evaluation into an arbitrary credential-bearing fetch.
	for name, mc := range candidate.Models {
		if current, ok := active.cfg.Models[name]; !ok || current != mc {
			return fmt.Errorf("Evaluation models must match the active configured endpoints and identities.")
		}
	}
	rows, err := m.db.Query(`SELECT e.id FROM events e JOIN review_state r ON r.event_id=e.id JOIN event_labels l ON l.event_id=e.id WHERE r.state='reviewed' AND r.origin='real' ORDER BY e.ingest_time DESC,e.id DESC LIMIT 50`)
	if err != nil {
		return fmt.Errorf("Cannot select reviewed dataset.")
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			rows.Close()
			return fmt.Errorf("Cannot read dataset.")
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("Cannot read dataset.")
	}
	if len(ids) == 0 {
		return fmt.Errorf("No reviewed events with real origin. Review events first; test events do not count.")
	}
	cases := []evalCase{}
	inspections := []*storage.Inspection{}
	for _, id := range ids {
		ins, err := m.db.Inspect(id)
		if err != nil || ins.Label == nil {
			return fmt.Errorf("Dataset changed. Retry selection.")
		}
		spec := candidate.InterpreterForEvent(ins.Event)
		selected := *candidate
		selected.Interpret = spec
		if err := validateLabelCategory(&selected, ins.Label.Category); err != nil {
			return fmt.Errorf("A selected label is incompatible with the candidate contract.")
		}
		if req.Mode == "policy" {
			var historicalYAML string
			if ins.ConfigRevision == "" || m.db.QueryRow(`SELECT yaml FROM control_revisions WHERE id=?`, ins.ConfigRevision).Scan(&historicalYAML) != nil {
				return fmt.Errorf("Policy comparison needs recorded interpretation contracts. This dataset includes events without a retained configuration revision; use interpreter evaluation instead.")
			}
			historical, err := config.Parse([]byte(historicalYAML))
			if err != nil || !reflect.DeepEqual(spec, historical.InterpreterForEvent(ins.Event)) || candidate.Models[spec.Model] != historical.Models[spec.Model] {
				return fmt.Errorf("Policy comparison requires the event's original interpretation contract and model identity.")
			}
		}
		tc := evalCase{Name: id, Event: ins.Event}
		tc.Expected.Category = ins.Label.Category
		action := ins.Label.RequiresAction
		tc.Expected.RequiresAction = &action
		tc.Expected.Outcome = ins.Label.ExpectedOutcome
		cases = append(cases, tc)
		inspections = append(inspections, ins)
	}
	m.evalMu.Lock()
	defer m.evalMu.Unlock()
	if m.evalCancel != nil {
		http.Error(w, "An evaluation is already running.", 409)
		return nil
	}
	id := event.NewID("eval_")
	rev := revision(req.YAML)
	if _, err = m.db.Exec(`INSERT OR IGNORE INTO control_revisions(id,yaml,created_at,status,base) VALUES(?,?,?,'evaluated',?)`, rev, req.YAML, time.Now().UTC().Format(time.RFC3339Nano), active.revision); err != nil {
		return fmt.Errorf("Cannot retain evaluation configuration.")
	}
	dataset := make([]storage.EventLabel, 0, len(inspections))
	for _, ins := range inspections {
		dataset = append(dataset, *ins.Label)
	}
	initial, _ := json.Marshal(map[string]any{"dataset": dataset, "baseline_revision": active.revision, "mode": req.Mode, "total": len(cases), "scope": "Interpreter and policy only; ingestion filters, depth, deduplication, and adapter delivery are excluded."})
	if _, err = m.db.Exec(`INSERT INTO control_evaluations VALUES(?,?,?,'running',?)`, id, time.Now().UTC().Format(time.RFC3339Nano), rev, string(initial)); err != nil {
		return fmt.Errorf("Cannot persist evaluation intent.")
	}
	if err = m.audit("evaluate", id, "started"); err != nil {
		m.finishEvaluation(id, "failed")
		return fmt.Errorf("Cannot audit evaluation.")
	}
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
	m.evalCancel = cancel
	m.evalWG.Add(1)
	go func() {
		defer m.evalWG.Done()
		defer cancel()
		defer func() { m.evalMu.Lock(); m.evalCancel = nil; m.evalMu.Unlock() }()
		report := []evaluationRow{}
		falseIgnores, correct, eligible, positive := 0, 0, 0, 0
		status := "complete"
		for i, tc := range cases {
			if ctx.Err() != nil {
				status = "interrupted"
				break
			}
			ins := inspections[i]
			row := evaluationRow{EventID: tc.Event.ID, Label: *ins.Label, Status: "ok"}
			if req.Mode == "policy" {
				if len(ins.Decisions) == 0 || ins.Decisions[len(ins.Decisions)-1].Status != "ok" {
					row.Status = "no-compatible-decision"
				} else {
					d := ins.Decisions[len(ins.Decisions)-1]
					spec := candidate.InterpreterForEvent(ins.Event)
					if d.Interpreter != spec.Model || d.Model != candidate.Models[spec.Model].Model {
						row.Status = "incompatible-model"
					} else {
						then, _ := candidate.PolicyForEvent(tc.Event).EvaluateEvent(d.Result, tc.Event)
						base, _ := active.cfg.PolicyForEvent(tc.Event).EvaluateEvent(d.Result, tc.Event)
						row.Outcome = then.Outcome()
						row.BaselineOutcome = base.Outcome()
						row.Category = evaluatedCategory(d.Result)
						row.Correct = row.Outcome == tc.Expected.Outcome
						row.FalseIgnore = row.Outcome == "ignore" && tc.Expected.Outcome != "ignore"
					}
				}
			} else {
				// Reuse the CLI evaluator. It calls no adapters and writes no operational
				// Events; progress goes to discard so raw provider errors are not stored.
				summary := evaluate(ctx, candidate, []evalCase{tc}, io.Discard)
				row.Correct = summary.OutcomeCorrect == 1
				row.FalseIgnore = summary.FalseIgnores == 1
				if summary.ValidDecisions == 0 {
					row.Status = "model-error"
				}
				if len(summary.Results) > 0 {
					result := summary.Results[0]
					row.Category, row.Outcome, row.LatencyMs = result.Category, result.Outcome, result.LatencyMs
				}
			}
			if row.Correct {
				correct++
			}
			if row.FalseIgnore {
				falseIgnores++
			}
			if row.Status == "ok" || row.Status == "model-error" {
				eligible++
				if tc.Expected.Outcome != "ignore" {
					positive++
				}
			}
			var accuracy, falseIgnoreRate *float64
			if eligible > 0 {
				v := float64(correct) / float64(eligible)
				accuracy = &v
			}
			if positive > 0 {
				v := float64(falseIgnores) / float64(positive)
				falseIgnoreRate = &v
			}
			report = append(report, row)
			b, _ := json.Marshal(map[string]any{"dataset": dataset, "eligible": eligible, "expected_non_ignore": positive, "outcome_accuracy": accuracy, "false_ignore_rate": falseIgnoreRate, "baseline_revision": active.revision, "mode": req.Mode, "total": len(cases), "completed": len(report), "correct_outcomes": correct, "false_ignores": falseIgnores, "events": report, "scope": "Excludes ingestion gates and adapter execution. No effects were performed."})
			if _, err := m.db.Exec(`UPDATE control_evaluations SET report=? WHERE id=?`, string(b), id); err != nil {
				status = "storage-error"
				break
			}
		}
		if ctx.Err() != nil {
			status = "interrupted"
		}
		m.finishEvaluation(id, status)
	}()
	return controlJSON(w, map[string]any{"id": id, "total": len(cases), "mode": req.Mode})
}
