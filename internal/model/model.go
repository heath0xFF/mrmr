// Package model talks to OpenAI-compatible chat completion endpoints and
// returns schema-validated structured results. Everything here treats the
// model as an untrusted producer of JSON: output is parsed defensively,
// validated against the configured schema, and retried exactly once with the
// validation error appended so small local models can self-correct. The
// caller (the runtime) decides what a bad result means — this package only
// reports it.
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
	Provider  string `yaml:"provider"` // openai-compatible (only one for now)
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

// Interpret sends the event to the configured model and returns the
// validated result map. Error is *InvalidOutputError when the output failed
// schema validation after its one self-correction retry; other errors are
// transport/endpoint failures. The distinction matters to the caller: invalid
// output is the model's fault and is recorded on the decision; transport
// failure is an endpoint outage and must not be retried indefinitely by anyone.
//
// Two bounds apply together on a single call budget of maxAttempts: a
// schema-invalid output is retried exactly once with the validation error
// appended (IMPLEMENTATION.md pins this), and transport failures (network
// errors, 5xx) consume the remaining budget rather than aborting mid-loop,
// so one blip during self-correction doesn't fail an event that had calls
// left. Non-retryable client errors (401 wrong key, 404 wrong model) return
// immediately — retrying those is pure noise. Latency sums every call: the
// trace cares how long interpretation took in total, corrections included.
func (c *Client) Interpret(ctx context.Context, cfg Config, interpreter string, prompt string, schema Schema, eventJSON []byte) (result map[string]any, latencyMs int64, model string, err error) {
	model = cfg.Model
	sys := prompt + "\n\nRespond with a single JSON object with exactly these fields:\n" + schemaJSON(schema)
	messages := []message{{"system", sys}, {"user", string(eventJSON)}}

	var (
		transportErr  error
		invalidDetail string
		usedRetry     bool // one self-correction attempt, ever
	)
	for calls := 0; calls < maxAttempts; {
		if calls > 0 {
			select {
			case <-ctx.Done():
				return nil, latencyMs, model, ctx.Err()
			case <-time.After(retryBackoff):
			}
		}
		content, lat, err := c.complete(ctx, cfg, schema, messages)
		calls++
		latencyMs += lat
		if err != nil {
			transportErr = fmt.Errorf("model call (attempt %d): %w", calls, err)
			var se *statusError
			if errors.As(err, &se) && se.Code < 500 {
				// 4xx won't get better by retrying. (A response_format 400 was
				// already handled and exhausted inside complete.)
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
		messages = append(messages,
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
	Body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("endpoint returned %d: %s", e.Code, e.Body)
}

// complete performs one chat completion. It first asks for schema-constrained
// decoding; endpoints that reject response_format (HTTP 400) are retried once
// without it, falling back to validation alone. This keeps the client
// compatible with llama.cpp/vLLM/Ollama endpoints that support grammar
// constraints as well as plain ones that don't.
func (c *Client) complete(ctx context.Context, cfg Config, schema Schema, messages []message) (content string, latencyMs int64, err error) {
	hc := &http.Client{Timeout: requestTimeout}
	req := chatRequest{
		Model:          cfg.Model,
		Messages:       messages,
		Temperature:    0,
		ResponseFormat: &responseFormat{Type: "json_schema", JSONSchema: &jsonSchemaFmt{Name: "decision", Strict: true, Schema: schemaMap(schema)}},
	}
	for i := 0; i < 2; i++ { // 0: with response_format, 1: without (endpoint lacks schema support)
		start := time.Now()
		content, err = c.post(ctx, hc, cfg, req)
		latencyMs += time.Since(start).Milliseconds()
		var se *statusError
		if err == nil || !(errors.As(err, &se) && se.Code == http.StatusBadRequest) {
			return content, latencyMs, err
		}
		req.ResponseFormat = nil
	}
	return
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
		// Cap the body we read: endpoint error pages can be arbitrarily large
		// and only the first line or two is ever useful in a trace.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", &statusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(b))}
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
