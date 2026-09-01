// Package runtime is the mrmr loop: persist Event → interpret → Decision →
// policy → Outcome → adapter, with a trace at every step. This package is
// the choreography; every stage's actual work lives in its own package.
// The one rule that shapes everything here: fail toward ignore. A model
// outage or a garbage result degrades mrmr into a no-op — the safe direction
// — and never into an action nobody authorized.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
	"github.com/heath0xff/mrmr/internal/storage"
)

// Runtime is the assembled pipeline. It is immutable after construction:
// the config that built it is the config it runs, which is what lets
// in-flight events finish under the rules they started with.
type Runtime struct {
	DB       *storage.DB
	Client   *model.Client
	ModelCfg model.Config
	ModelKey string // name in config, recorded on decisions
	Prompt   string
	Schema   model.Schema
	Policy   policy.Policy
}

// TraceStep is one observable moment in an event's journey. The trace is
// mrmr's answer to "why did this happen?" — it must be possible to explain
// any outcome without reading anything but the trace.
type TraceStep struct {
	At    time.Time `json:"at"`
	Stage string    `json:"stage"`
	Msg   string    `json:"msg,omitempty"`
}

// Response is what the caller (HTTP handler) receives: the event id, the
// decision (if any), the outcome, and the full trace. It doubles as the
// stdout log line for milestone 1.
type Response struct {
	EventID   string          `json:"event_id"`
	Duplicate bool            `json:"duplicate,omitempty"`
	Decision  *event.Decision `json:"decision,omitempty"`
	Outcome   string          `json:"outcome,omitempty"`
	Trace     []TraceStep     `json:"trace"`
}

func step(t *[]TraceStep, stage, msg string) {
	*t = append(*t, TraceStep{At: time.Now().UTC(), Stage: stage, Msg: msg})
}

// Ingest is the whole pipeline for one event, synchronously. It never
// returns an error for a "bad" decision or outcome — a schema-invalid model
// or a down endpoint is normal operation, recorded in the decision and trace
// — only for persistence failures, which are the caller's (HTTP handler's)
// 5xx, since an un-persisted event's fate is genuinely unknown.
func (r *Runtime) Ingest(ctx context.Context, e event.Event) (*Response, error) {
	t := &[]TraceStep{}
	step(t, "receive", fmt.Sprintf("type=%s source=%s", e.Type, e.Source))

	rec, err := r.DB.InsertEvent(e)
	if err != nil {
		return nil, fmt.Errorf("persist event: %w", err)
	}
	if rec.Duplicate {
		step(t, "duplicate", "dropped: (source, dedup_key) already seen")
		return &Response{EventID: e.ID, Duplicate: true, Trace: *t}, nil
	}
	step(t, "persist", "event stored")

	dec := &event.Decision{ID: event.NewID("dec_"), EventID: e.ID, Interpreter: r.ModelKey, Model: r.ModelCfg.Model}

	eventJSON, _ := json.Marshal(e)
	result, latency, modelID, err := r.Client.Interpret(ctx, r.ModelCfg, r.ModelKey, r.Prompt, r.Schema, eventJSON)
	dec.Model = modelID
	dec.LatencyMs = latency

	if err != nil {
		if inv, ok := err.(*model.InvalidOutputError); ok {
			dec.Status = "invalid"
			dec.Error = inv.Error()
			step(t, "interpret", "schema-invalid after retry")
		} else {
			dec.Status = "errored"
			dec.Error = err.Error()
			step(t, "interpret", "model error: "+err.Error())
		}
	} else {
		dec.Status = "ok"
		dec.Result = result
		if b, jerr := json.Marshal(result); jerr == nil {
			step(t, "interpret", fmt.Sprintf("model=%s latency_ms=%d result=%s", modelID, latency, b))
		}
	}
	if serr := r.DB.InsertDecision(*dec); serr != nil {
		return nil, fmt.Errorf("persist decision: %w", serr)
	}

	// Fail toward ignore: invalid or errored decisions never reach policy.
	// A model that is down or wrong must not be able to trigger actions —
	// policy evaluates judgment, and there is no judgment to evaluate.
	var then policy.Then
	if dec.Status == "ok" {
		var ruleIdx int
		then, ruleIdx = r.Policy.Evaluate(dec.Result) // assignment, not := — the outer then must be set here
		if ruleIdx < 0 {
			step(t, "policy", "default → "+then.Outcome())
		} else {
			step(t, "policy", fmt.Sprintf("rule %d → %s", ruleIdx+1, then.Outcome()))
		}
	} else {
		then = policy.Then{Ignore: true}
		step(t, "policy", "routed to ignore ("+dec.Status+" decision)")
	}

	execErr := r.execute(then, dec)
	if execErr != "" {
		step(t, "outcome", "error: "+execErr)
	} else {
		step(t, "outcome", "executed "+then.Outcome())
	}
	if serr := r.DB.InsertExecution(event.NewID("exe_"), e.ID, dec.ID, then.Outcome(), adapterFor(then), statusFor(execErr), execErr); serr != nil {
		return nil, fmt.Errorf("persist execution: %w", serr)
	}

	return &Response{EventID: e.ID, Decision: dec, Outcome: then.Outcome(), Trace: *t}, nil
}

func adapterFor(t policy.Then) string {
	if t.Notify != nil {
		return t.Notify.Via
	}
	return ""
}

func statusFor(execErr string) string {
	if execErr == "" {
		return "ok"
	}
	return "error"
}

// execute performs the outcome's side effect and returns "" on success.
// Ignore is a success — most events should end here; suppressing noise is
// half the point of ambient AI.
func (r *Runtime) execute(t policy.Then, dec *event.Decision) string {
	if t.Notify == nil {
		return "" // ignore is a success
	}
	if t.Notify.Via != "stdout" {
		return fmt.Sprintf("unsupported notify via %q", t.Notify.Via) // config validation should prevent this
	}
	fmt.Println(renderMessage(t.Notify.Message, dec))
	return ""
}

// renderMessage applies the notify template over the decision result.
// An empty template renders the full result JSON so a bare `notify: {via: x}`
// is still useful. Template errors degrade to a bracketed note rather than
// dropping the notification — a notification with a hint of what went wrong
// beats silence.
func renderMessage(tmpl string, dec *event.Decision) string {
	if tmpl == "" {
		b, _ := json.Marshal(dec.Result)
		return fmt.Sprintf("[%s] %s", dec.EventID, b)
	}
	t, err := template.New("notify").Parse(tmpl)
	if err != nil {
		return fmt.Sprintf("[%s] (bad notify template: %v)", dec.EventID, err)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, map[string]any{"result": dec.Result, "event": map[string]any{"id": dec.EventID}}); err != nil {
		return fmt.Sprintf("[%s] (template error: %v)", dec.EventID, err)
	}
	return sb.String()
}
