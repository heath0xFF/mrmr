package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/event"
)

// Drafts may contain examples while an operator edits. Testing and activation
// must not mistake those examples, or an absent credential, for a ready model.
func managementModelReady(c *config.Config) error {
	for _, m := range c.Models {
		if m.APIKeyEnv != "" && os.Getenv(m.APIKeyEnv) == "" {
			return fmt.Errorf("A model credential is missing from the runtime environment. Provision the named environment variable on the runtime host, then restart mrmr.")
		}
	}
	interpretations := []config.Interpret{c.Interpret}
	for _, spec := range c.InterpretByType {
		interpretations = append(interpretations, spec)
	}
	for _, monitor := range c.Monitors {
		interpretations = append(interpretations, monitor.Interpret)
	}
	for _, spec := range interpretations {
		m := c.Models[spec.Model]
		u, err := url.Parse(m.BaseURL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("Use an HTTP or HTTPS model base URL without credentials, query parameters, or a fragment.")
		}
		identity, host := strings.TrimSpace(m.Model), strings.ToLower(u.Hostname())
		if identity == "" || strings.EqualFold(identity, "replace-with-your-model") || host == "invalid" || strings.HasSuffix(host, ".invalid") {
			return fmt.Errorf("The selected interpreter still uses an example model. Set its real identity and endpoint before testing or activating.")
		}
	}
	return nil
}

// The setup check uses fixed synthetic input, not stored Events or caller-
// supplied text. It calls the ordinary interpreter/evaluator but never sources
// or adapters. A successful connection is not a real-event quality gate.
func (m *manager) testModel(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		YAML    string `json:"yaml"`
		Source  string `json:"source"`
		Confirm bool   `json:"confirm"`
	}
	if err := decodeControl(w, r, &req); err != nil {
		return err
	}
	if !req.Confirm {
		return fmt.Errorf("Confirm sending two synthetic examples to the selected model. No private Events or effects are involved.")
	}
	c, err := managedParse(req.YAML)
	if err != nil {
		return err
	}
	if err := managementModelReady(c); err != nil {
		return err
	}
	// Keep old single-source clients working, but never guess among monitors.
	if req.Source == "" && len(c.Sources) == 1 {
		req.Source = c.Sources[0].Name
	}
	var source config.Source
	for _, s := range c.Sources {
		if s.Name == req.Source {
			source = s
		}
	}
	if source.Type != "systemd-journal" {
		return fmt.Errorf("Select a configured system-journal source for this test. Use Review for other evaluation workflows.")
	}
	spec := c.InterpreterForEvent(event.Event{Type: source.EventType, Source: source.Name})
	if q, ok := spec.Questions["category"]; !ok || q.Type != "choice" {
		return fmt.Errorf("This setup test requires the service monitor's category questions.")
	}
	cases := []evalCase{}
	for _, sample := range []struct {
		category, message, outcome string
		action                     bool
	}{
		{"incident", "The service is unavailable. Every request fails. It repeatedly crashes and cannot recover. Operator intervention is required.", "notify", true},
		{"noise", "Scheduled maintenance completed successfully. The service is healthy. No action is required.", "ignore", false},
	} {
		tc := evalCase{Name: "synthetic-" + sample.category, Event: event.Event{Type: source.EventType, Source: source.Name, Subject: "synthetic-setup.service", Data: map[string]any{"message": sample.message}}}
		tc.Expected.Category = sample.category
		tc.Expected.RequiresAction = &sample.action
		tc.Expected.Outcome = sample.outcome
		cases = append(cases, tc)
	}
	id := revision(req.YAML)
	// One document can contain several monitors; audit which contract was tested.
	target := id + ":" + source.Name
	if err := m.audit("test-model", target, "intent"); err != nil {
		return fmt.Errorf("Cannot record test intent; no model request was made.")
	}
	// The request owns this work. Shutdown/client cancellation and the deadline
	// stop retries; no job, goroutine, or transaction survives the request.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	report := evaluate(ctx, c, cases, io.Discard)
	ready := report.ValidDecisions == len(cases)
	result := "failed"
	if ready {
		result = "valid-responses"
	}
	if err := m.audit("test-model", target, result); err != nil {
		return fmt.Errorf("Test finished, but its audit could not be saved. No configuration was activated.")
	}
	return controlJSON(w, map[string]any{"revision": id, "source": source.Name, "ready": ready, "report": report, "notice": "Synthetic interpreter and policy check only. No source reads, operational Events, or effects. Real-event quality and delivery remain untested."})
}
