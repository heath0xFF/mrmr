package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/heath0xff/mrmr/internal/agent"
	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/policy"
)

func (r *Runtime) delegate(ctx context.Context, spec policy.Delegate, dec *event.Decision, parent event.Event, lineage event.Provenance, tr *[]TraceStep) string {
	cfg, ok := r.Agents[spec.Agent]
	if !ok {
		return fmt.Sprintf("unknown agent %q", spec.Agent)
	}
	lineage.Agent = spec.Agent
	request := agent.Request{EventID: parent.ID, Event: parent, Decision: dec.Result, Prompt: spec.Prompt,
		Delegation: lineage, Depth: parent.Depth, EmitCompletion: spec.EmitCompletion,
		ReadOnly: cfg.ReadOnly, Capabilities: cfg.Capabilities}
	if cfg.Model != "" {
		mc, ok := r.Models[cfg.Model]
		if !ok || mc.Role != "agent" {
			return "delegation requires a configured agent-role model"
		}
		request.Worker = &agent.Worker{Provider: mc.Provider, BaseURL: mc.BaseURL, Model: mc.Model}
	}
	step(tr, "delegate", "agent="+spec.Agent+" execution="+lineage.ExecutionID)
	completion, err := agent.Run(ctx, r.httpClient(), cfg, request)
	if err != nil {
		return err.Error()
	}
	if completion == nil {
		return ""
	}
	// Decode the validated contract into JSON-shaped data so filters and policy
	// see the same values before and after a database round trip. No raw response
	// or ancestor payload is stored in this event.
	b, err := json.Marshal(completion)
	if err != nil {
		return "encode completion failed"
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return "normalize completion failed"
	}
	typ := spec.CompletionEventType
	if typ == "" {
		typ = "agent.completed"
	}
	child := event.Event{ID: event.NewID("evt_"), Type: typ, Source: "mrmr.agent", Subject: parent.Subject,
		Timestamp: time.Now().UTC(), Depth: parent.Depth + 1, Provenance: &lineage,
		Data:     map[string]any{"agent": spec.Agent, "result": result},
		Metadata: map[string]any{"source_event_id": lineage.ExecutionID}}
	step(tr, "completion", "emitted "+child.ID+" type="+typ+" depth="+fmt.Sprint(child.Depth))
	// An external terminal result, not an agent's next-step instruction, starts
	// this hop. Ingest persists/deduplicates it and applies the ordinary depth cap.
	resp, err := r.Ingest(ctx, child)
	if err != nil {
		return fmt.Sprintf("ingest agent completion: %v", err)
	}
	step(tr, "completion", "child outcome "+resp.Outcome)
	return ""
}
