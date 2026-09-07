// Package runtime is the mrmr loop: persist Event → filter → interpret →
// Decision → policy → Outcome → adapter, with a trace at every step. The
// filter stage is deterministic and may short-circuit: an excluded event
// produces no Decision and no model call.
// This package is the choreography; every stage's actual work lives in its
// own package.
// The one rule that shapes everything here: fail toward ignore. A model
// outage or a garbage result degrades mrmr into a no-op — the safe direction
// — and never into an action nobody authorized.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/filter"
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
	Filters  filter.List
	// AgentEndpoints maps a policy delegate.agent name to its HTTP endpoint.
	// Resolved at startup from config so policy rules never carry URLs.
	AgentEndpoints map[string]string
	// HTTP is the client for action/delegate side effects. Nil means a
	// default 10s timeout — outbound effects must be bounded, never hang
	// the synchronous ingest path.
	HTTP *http.Client
}

// maxDepth caps emit-event recursion: a causal chain longer than this is a
// loop, and loops get recorded and ignored, not executed.
const maxDepth = 10

// TraceStep is one observable moment in an event's journey. It is an alias,
// not a distinct type: the trace is persisted next to the event it explains,
// so the shape belongs to the domain package that storage also speaks, and
// the runtime is merely where the steps are produced.
type TraceStep = event.TraceStep

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
	resp, err := r.ingest(ctx, e, t)
	if err != nil {
		return nil, err
	}
	// The trace is persisted once, here, rather than at each of ingest's
	// exits — one write, and every path is covered by construction.
	//
	// A duplicate is the exception: it never inserted an events row under
	// this id, so the trace's foreign key would have nothing to point at,
	// and there is nothing to inspect anyway — the original event's trace
	// already tells that story. A trace write failure is reported like the
	// pipeline's other persistence failures (the caller's 5xx): the outcome
	// has already happened, and an outcome nobody can explain afterwards is
	// exactly the state this table exists to prevent.
	if !resp.Duplicate {
		if serr := r.DB.InsertTrace(e.ID, *t); serr != nil {
			return nil, fmt.Errorf("persist trace: %w", serr)
		}
	}
	return resp, nil
}

// ingest runs the pipeline and appends to the trace as it goes. It is
// separate from Ingest so that trace persistence has exactly one call site.
func (r *Runtime) ingest(ctx context.Context, e event.Event, t *[]TraceStep) (*Response, error) {
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

	// Recursion guard for mrmr-emitted events. The event is durable and
	// traceable, but past the depth cap it is forced to ignore: a flow pair
	// emitting into each other must surface as ignore rows here, not as an
	// infinite model-calling loop.
	if e.Depth > maxDepth {
		step(t, "depth", fmt.Sprintf("causal depth %d exceeds max %d; recorded but forced to ignore", e.Depth, maxDepth))
		if serr := r.DB.InsertExecution(event.NewID("exe_"), e.ID, "", "ignore", "", "ok", "max causal depth exceeded"); serr != nil {
			return nil, fmt.Errorf("persist execution: %w", serr)
		}
		step(t, "outcome", "executed ignore")
		return &Response{EventID: e.ID, Outcome: "ignore", Trace: *t}, nil
	}

	// Filters run after the event is durable and before any model
	// call: they are the cost gate. An excluded event produces no
	// Decision (the model never ran, and a Decision records model
	// judgment) but does produce an Execution row: the audit trail
	// must show where the event stopped. The adapter is empty because
	// a filter is a gate, not an Adapter under the project's domain
	// boundary; the filter trace stage names the stop point.
	if len(r.Filters) > 0 {
		passed, reason := r.Filters.Evaluate(e)
		if !passed {
			step(t, "filter", "excluded: "+reason)
			if serr := r.DB.InsertExecution(event.NewID("exe_"), e.ID, "", "ignore", "", "filtered", ""); serr != nil {
				return nil, fmt.Errorf("persist execution: %w", serr)
			}
			step(t, "outcome", "executed ignore")
			return &Response{EventID: e.ID, Outcome: "ignore", Trace: *t}, nil
		}
		step(t, "filter", fmt.Sprintf("%d checks passed", len(r.Filters)))
	}

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

	// Shadow records the outcome as if it had fired but executes nothing —
	// how a new flow earns trust before going live. Promotion is later a
	// config change, not a rewrite.
	execErr := ""
	if then.Shadow {
		step(t, "outcome", "shadow: "+then.Outcome()+" recorded but not executed")
	} else {
		execErr = r.execute(ctx, then, dec, e, t)
	}
	if execErr != "" {
		step(t, "outcome", "error: "+execErr)
	} else if !then.Shadow {
		step(t, "outcome", "executed "+then.Outcome())
	}
	status := "ok"
	switch {
	case execErr != "":
		status = "error"
	case then.Shadow:
		status = "shadow"
	}
	if serr := r.DB.InsertExecution(event.NewID("exe_"), e.ID, dec.ID, then.Outcome(), adapterFor(then), status, execErr); serr != nil {
		return nil, fmt.Errorf("persist execution: %w", serr)
	}

	return &Response{EventID: e.ID, Decision: dec, Outcome: then.Outcome(), Trace: *t}, nil
}

func adapterFor(t policy.Then) string {
	switch {
	case t.Notify != nil:
		return t.Notify.Via
	case t.Action != nil:
		return t.Action.Type
	case t.Delegate != nil:
		return t.Delegate.Agent
	}
	return ""
}

// execute performs the outcome's side effect and returns "" on success.
// Ignore is a success — most events should end here; suppressing noise is
// half the point of ambient AI. Emit recursion is synchronous by design:
// it is depth-capped, so no goroutines, no queue, nothing to leak.
func (r *Runtime) execute(ctx context.Context, t policy.Then, dec *event.Decision, parent event.Event, tr *[]TraceStep) string {
	switch {
	case t.Notify != nil:
		if t.Notify.Via != "stdout" {
			return fmt.Sprintf("unsupported notify via %q", t.Notify.Via) // config validation should prevent this
		}
		fmt.Println(renderMessage(t.Notify.Message, dec))
		return ""

	case t.Action != nil:
		switch t.Action.Type {
		case "http":
			method := t.Action.Method
			if method == "" {
				method = http.MethodPost
			}
			// Body template over the decision; empty body is the full result
			// JSON, same default as notify, so a bare action is still useful.
			return r.do(ctx, method, t.Action.URL, renderMessage(t.Action.Body, dec))
		case "emit":
			// The decision result becomes the child's data — the child flow's
			// interpreter judges meaning, it does not re-parse the parent's
			// decision row. ponytail: (source, source_event_id) = (mrmr,
			// parent id) means one emit per parent; multiple emit rules on one
			// flow would dedup-collide. Split flows if that's ever needed.
			child := event.Event{
				ID:        event.NewID("evt_"),
				Type:      t.Action.EventType,
				Source:    "mrmr",
				Timestamp: time.Now().UTC(),
				Data:      dec.Result,
				Metadata:  map[string]any{"source_event_id": parent.ID},
				Depth:     parent.Depth + 1,
			}
			step(tr, "emit", "emitted "+child.ID+" type="+child.Type+" depth="+fmt.Sprint(child.Depth))
			resp, err := r.Ingest(ctx, child)
			if err != nil {
				return fmt.Sprintf("emit: %v", err)
			}
			step(tr, "emit", "child outcome "+resp.Outcome)
			return ""
		}
		return fmt.Sprintf("unsupported action type %q", t.Action.Type) // config validation should prevent this

	case t.Delegate != nil:
		endpoint, ok := r.AgentEndpoints[t.Delegate.Agent]
		if !ok {
			return fmt.Sprintf("unknown agent %q", t.Delegate.Agent) // config validation should prevent this
		}
		// Generic HTTP agent contract: one POST, JSON payload, 2xx means
		// accepted. The agent's work is its own business; mrmr records that
		// it delegated, to whom, and why.
		payload, _ := json.Marshal(struct {
			EventID  string         `json:"event_id"`
			Decision map[string]any `json:"decision"`
			Prompt   string         `json:"prompt"`
		}{dec.EventID, dec.Result, t.Delegate.Prompt})
		return r.do(ctx, http.MethodPost, endpoint, string(payload))
	}
	return "" // ignore is a success
}

// do sends one bounded HTTP side effect and returns "" on a 2xx. The body
// is drained (bounded) so the connection is reusable; response content is
// not interpreted — actions are fire-and-record, not request/response.
func (r *Runtime) do(ctx context.Context, method, url, body string) string {
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		return fmt.Sprintf("build request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return fmt.Sprintf("http %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Sprintf("http %s %s: status %d", method, url, resp.StatusCode)
	}
	return ""
}

func (r *Runtime) httpClient() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
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
