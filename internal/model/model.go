// Package model talks to configured interpretation endpoints and returns
// validated structured results. Everything here treats model responses as
// untrusted: provider-specific clients validate the complete response before
// the runtime can persist it or pass it to policy. The caller decides what a
// bad result means — this package only reports it.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config describes one model endpoint. Secrets are referenced by env var
// name only (api_key_env) — never inlined — so config files stay safe to
// commit and inspect.
type Config struct {
	Provider  string `yaml:"provider"` // openai-compatible | typesafe
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	Model     string `yaml:"model"`
}

// Field is the v0 schema subset: flat typed fields with optional enums and numeric bounds.
type Field struct {
	Type    string   `yaml:"type"` // string | number | boolean
	Enum    []any    `yaml:"enum"`
	Minimum *float64 `yaml:"minimum"`
	Maximum *float64 `yaml:"maximum"`
}

type Schema map[string]Field

type Client struct{}

const (
	maxAttempts    = 3 // total HTTP calls per interpretation, all failure modes combined
	requestTimeout = 60 * time.Second
	retryBackoff   = 250 * time.Millisecond // only between transport attempts; a briefly overloaded local model needs a moment
)

type InvalidOutputError struct{ Detail string }

func (e *InvalidOutputError) Error() string { return "invalid model output: " + e.Detail }

// Interpret preserves the original OpenAI-compatible call surface for small
// callers and tests. Runtime code uses InterpretWithQuestions so a TypeSafe
// interpretation can carry its configured primitives.
func (c *Client) Interpret(ctx context.Context, cfg Config, interpreter string, prompt string, schema Schema, eventJSON []byte) (result map[string]any, latencyMs int64, model string, err error) {
	return c.InterpretWithQuestions(ctx, cfg, interpreter, prompt, schema, nil, eventJSON)
}

// InterpretWithQuestions dispatches one event to the selected provider.
// Keeping this concrete avoids a provider framework: there are exactly two
// real protocols, and each validates its response before returning a Decision.
func (c *Client) InterpretWithQuestions(ctx context.Context, cfg Config, interpreter string, prompt string, schema Schema, questions Questions, eventJSON []byte) (result map[string]any, latencyMs int64, model string, err error) {
	switch cfg.Provider {
	case "openai-compatible":
		return c.interpretOpenAI(ctx, cfg, interpreter, prompt, schema, eventJSON)
	case "typesafe":
		return c.interpretTypeSafe(ctx, cfg, questions, eventJSON)
	default:
		return nil, 0, cfg.Model, fmt.Errorf("unsupported model provider %q", cfg.Provider)
	}
}

// interpretOpenAI sends the event to an OpenAI-compatible model. Error is
// *InvalidOutputError when output fails schema validation after one correction;
// other errors are endpoint failures. All retries share maxAttempts, and
// latency sums model calls but not backoff sleeps.
func (c *Client) interpretOpenAI(ctx context.Context, cfg Config, interpreter string, prompt string, schema Schema, eventJSON []byte) (result map[string]any, latencyMs int64, model string, err error) {
	model = cfg.Model
	sys := prompt + "\n\nRespond with a single JSON object with exactly these fields:\n" + schemaJSON(schema)
	req := chatRequest{
		Model:          cfg.Model,
		Messages:       []message{{"system", sys}, {"user", string(eventJSON)}},
		Temperature:    0,
		ResponseFormat: &responseFormat{Type: "json_schema", JSONSchema: &jsonSchemaFmt{Name: "decision", Strict: true, Schema: schemaMap(schema)}},
	}
	hc := &http.Client{Timeout: requestTimeout}

	var (
		transportErr  error
		invalidDetail string
		usedRetry     bool // one self-correction attempt, ever
	)
	// Every post consumes the same budget, including response_format fallback.
	// Once an endpoint rejects the format, keep it disabled for correction
	// and transport retries within this interpretation, not across clients.
	for calls := 0; calls < maxAttempts; calls++ {
		if calls > 0 {
			select {
			case <-ctx.Done():
				return nil, latencyMs, model, ctx.Err()
			case <-time.After(retryBackoff):
			}
		}
		start := time.Now()
		content, err := c.post(ctx, hc, cfg, req)
		latencyMs += time.Since(start).Milliseconds()
		if err != nil {
			transportErr = fmt.Errorf("model call (attempt %d): %w", calls+1, err)
			var se *statusError
			if errors.As(err, &se) && se.Code < 500 {
				if se.Code == http.StatusBadRequest && req.ResponseFormat != nil {
					req.ResponseFormat = nil
					continue
				}
				// Other 4xx failures, including a second 400 without the
				// format, cannot be repaired by another identical request.
				return nil, latencyMs, model, transportErr
			}
			continue // network error or 5xx: retry within the remaining budget
		}
		transportErr = nil
		res, verr := parseAndValidate(content, schema)
		if verr == nil {
			return res, latencyMs, model, nil
		}
		invalidDetail = verr.Error()
		if usedRetry {
			return nil, latencyMs, model, &InvalidOutputError{Detail: invalidDetail}
		}
		// One self-correction hop: show the model its failed output and the
		// validation error so small local models can fix themselves. This
		// path is expected to be routine and must stay bounded.
		usedRetry = true
		req.Messages = append(req.Messages,
			message{"assistant", content},
			message{"user", "Your previous output failed validation: " + verr.Error() + "\nRespond again with a JSON object matching the schema exactly."},
		)
	}
	if transportErr != nil {
		return nil, latencyMs, model, transportErr
	}
	return nil, latencyMs, model, &InvalidOutputError{Detail: invalidDetail}
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type       string         `json:"type"`
	JSONSchema *jsonSchemaFmt `json:"json_schema,omitempty"`
}

type jsonSchemaFmt struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []message       `json:"messages"`
	Temperature    float64         `json:"temperature"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// statusError distinguishes endpoint HTTP failures from transport failures.
// The distinction is load-bearing: a 400 to a schema-constrained request
// means the endpoint doesn't support response_format and is worth retrying
// without it, while a 500 is an outage that only counts against the retry
// budget.
type statusError struct {
	Code int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("endpoint returned %d", e.Code)
}

func (c *Client) post(ctx context.Context, hc *http.Client, cfg Config, req chatRequest) (string, error) {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if cfg.APIKeyEnv != "" {
		if key := os.Getenv(cfg.APIKeyEnv); key != "" {
			httpReq.Header.Set("Authorization", "Bearer "+key)
		}
	}
	resp, err := hc.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Error pages can echo authorization headers or private prompts.
		// Keep only the status: these errors reach SQLite, logs, and callers.
		// A bounded drain permits connection reuse without retaining content.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		return "", &statusError{Code: resp.StatusCode}
	}
	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("empty choices in response")
	}
	return cr.Choices[0].Message.Content, nil
}

// schemaJSON renders the schema as the instruction block for the prompt.
func schemaJSON(s Schema) string {
	b, _ := json.MarshalIndent(schemaMap(s), "", "  ")
	return string(b)
}

// schemaMap converts the flat Schema into a JSON-Schema object map for
// constrained decoding (llama.cpp grammars / vLLM guided JSON / etc.).
func schemaMap(s Schema) map[string]any {
	props := map[string]any{}
	required := []string{}
	for name, f := range s {
		p := map[string]any{"type": f.Type}
		if len(f.Enum) > 0 {
			p["enum"] = f.Enum
		}
		if f.Minimum != nil {
			p["minimum"] = *f.Minimum
		}
		if f.Maximum != nil {
			p["maximum"] = *f.Maximum
		}
		props[name] = p
		required = append(required, name)
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

// parseAndValidate extracts a JSON object from model output. Small local
// models routinely wrap JSON in markdown fences or lead with prose even when
// told not to, so we scan for the outermost braces instead of trusting the
// whole string to be JSON. If the braces contain garbage, Unmarshal catches it.
func parseAndValidate(content string, schema Schema) (map[string]any, error) {
	// Tolerate models that wrap JSON in fences or prose.
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object found in output")
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(content[start:end+1]), &res); err != nil {
		return nil, fmt.Errorf("unparseable JSON: %v", err)
	}
	return res, validate(res, schema)
}

// validate checks a parsed result against the flat schema. It enforces
// exact shape — every field present, correct type, enum membership, no
// extras — because downstream policy conditions are silent when a field is
// missing, and a silently-missing field is indistinguishable from a
// deliberate route to the default outcome.
func validate(res map[string]any, schema Schema) error {
	for name, f := range schema {
		v, ok := res[name]
		if !ok {
			return fmt.Errorf("missing field %q", name)
		}
		switch f.Type {
		case "string":
			if _, ok := v.(string); !ok {
				return fmt.Errorf("field %q: want string", name)
			}
		case "number":
			n, ok := v.(float64)
			if !ok {
				return fmt.Errorf("field %q: want number", name)
			}
			if f.Minimum != nil && n < *f.Minimum {
				return fmt.Errorf("field %q: value %v below minimum %v", name, n, *f.Minimum)
			}
			if f.Maximum != nil && n > *f.Maximum {
				return fmt.Errorf("field %q: value %v above maximum %v", name, n, *f.Maximum)
			}
		case "boolean":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("field %q: want boolean", name)
			}
		default:
			return fmt.Errorf("schema field %q: unsupported type %q", name, f.Type)
		}
		if len(f.Enum) > 0 {
			found := false
			for _, e := range f.Enum {
				if fmt.Sprint(e) == fmt.Sprint(v) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("field %q: value %v not in enum", name, v)
			}
		}
	}
	for name := range res {
		if _, ok := schema[name]; !ok {
			return fmt.Errorf("unexpected field %q", name)
		}
	}
	return nil
}
