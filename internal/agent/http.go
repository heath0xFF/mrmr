// Package agent defines the bounded HTTP contract with external workers.
// It performs no policy evaluation and never dispatches returned recommendations.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/heath0xff/mrmr/internal/event"
)

type Config struct {
	Endpoint     string        `yaml:"endpoint"`
	Timeout      time.Duration `yaml:"timeout"` // zero keeps the existing 10-second bound
	Model        string        `yaml:"model"`   // optional reference to a role: agent model
	Capabilities []string      `yaml:"capabilities"`
	ReadOnly     bool          `yaml:"read_only"`
}

// Worker is a configured endpoint hint, never a credential or model-selected
// destination. The external harness owns its client and tool enforcement.
type Worker struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	Model    string `json:"model"`
}

type Request struct {
	EventID        string           `json:"event_id"` // legacy fields remain available
	Decision       map[string]any   `json:"decision"`
	Prompt         string           `json:"prompt"`
	Event          event.Event      `json:"event"`
	Delegation     event.Provenance `json:"delegation"`
	Depth          int              `json:"depth"`
	EmitCompletion bool             `json:"emit_completion"`
	ReadOnly       bool             `json:"read_only"`
	Capabilities   []string         `json:"capabilities,omitempty"`
	Worker         *Worker          `json:"worker,omitempty"`
}

// Completion contains data only. Unknown envelope fields are rejected so an
// endpoint cannot silently extend this contract with execution instructions.
// Evidence and metadata stay opaque; URLs within them are never fetched.
type Completion struct {
	Status            string         `json:"status"`
	Summary           string         `json:"summary,omitempty"`
	Findings          []any          `json:"findings,omitempty"`
	Evidence          []any          `json:"evidence,omitempty"`
	RecommendedAction string         `json:"recommended_action,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
	StartedAt         *time.Time     `json:"started_at,omitempty"`
	CompletedAt       *time.Time     `json:"completed_at,omitempty"`
}

func Run(ctx context.Context, client *http.Client, cfg Config, request Request) (*Completion, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode delegation: %w", err)
	}
	// Bound even injected clients; redirects must not choose another effect target.
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	if timeout < time.Second || timeout > 5*time.Minute {
		return nil, fmt.Errorf("agent timeout must be between 1s and 5m")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("invalid agent endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	hc := *client
	hc.Timeout = timeout
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent request failed (transport or deadline)")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("agent endpoint returned %d", resp.StatusCode)
	}
	if !request.EmitCompletion {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, nil
	}
	// A 202 acknowledges future work, not completion. There is no callback
	// protocol yet; reject it instead of inventing a successful result event.
	if resp.StatusCode == http.StatusAccepted {
		return nil, fmt.Errorf("agent accepted work but synchronous completion is required")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, fmt.Errorf("agent completion unreadable or exceeds 1 MiB")
	}
	var result Completion
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return nil, fmt.Errorf("invalid agent completion object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one agent completion object")
	}
	switch result.Status {
	case "completed", "failed", "cancelled":
	default:
		return nil, fmt.Errorf("agent completion status must be completed, failed, or cancelled")
	}
	if result.StartedAt != nil && result.CompletedAt != nil && result.CompletedAt.Before(*result.StartedAt) {
		return nil, fmt.Errorf("agent completion precedes start")
	}
	return &result, nil
}
