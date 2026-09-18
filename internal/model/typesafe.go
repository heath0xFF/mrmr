package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Questions is the configured set of independent System One judgments. The
// criteria shape follows TypeSafe's API: a map for Choice/Noul and a list for
// Score. It remains any at the YAML boundary because those wire shapes differ;
// ValidateQuestions normalizes and rejects every other shape before startup.
type Questions map[string]Question

type Question struct {
	Type         string `yaml:"type"`
	Instructions string `yaml:"instructions"`
	Criteria     any    `yaml:"criteria"`
}

// ValidateQuestions validates the public YAML contract and, importantly,
// reserves dots for policy traversal. A question or option containing a dot
// would make result.category.choice ambiguous at policy time.
func ValidateQuestions(questions Questions) error {
	if len(questions) == 0 {
		return fmt.Errorf("questions are required")
	}
	for id, q := range questions {
		if !safeQuestionID(id) {
			return fmt.Errorf("question %q: id must contain only letters, numbers, underscore, or hyphen", id)
		}
		if _, err := normalizeQuestion(q); err != nil {
			return fmt.Errorf("question %q: %w", id, err)
		}
	}
	return nil
}

func safeQuestionID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

type systemOneQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

func normalizeQuestion(q Question) (systemOneQuestion, error) {
	if strings.TrimSpace(q.Instructions) == "" {
		return systemOneQuestion{}, fmt.Errorf("instructions are required")
	}
	out := systemOneQuestion{Type: q.Type, Instructions: q.Instructions}
	switch q.Type {
	case "choice":
		criteria, err := stringMap(q.Criteria)
		if err != nil || len(criteria) < 2 || len(criteria) > 255 {
			return out, fmt.Errorf("choice criteria must be a map of 2 to 255 string options")
		}
		for option, description := range criteria {
			if !safeQuestionID(option) || strings.TrimSpace(description) == "" {
				return out, fmt.Errorf("choice options must have path-safe names and non-empty string descriptions")
			}
		}
		out.Criteria = criteria
	case "noul":
		if q.Criteria == nil {
			return out, nil
		}
		criteria, err := stringMap(q.Criteria)
		if err != nil || len(criteria) != 2 || strings.TrimSpace(criteria["true"]) == "" || strings.TrimSpace(criteria["false"]) == "" {
			return out, fmt.Errorf("noul criteria must contain non-empty true and false strings")
		}
		out.Criteria = criteria
	case "score":
		criteria, err := stringList(q.Criteria)
		if err != nil || len(criteria) < 2 || len(criteria) > 10 {
			return out, fmt.Errorf("score criteria must be a list of 2 to 10 strings")
		}
		for _, level := range criteria {
			if strings.TrimSpace(level) == "" {
				return out, fmt.Errorf("score criteria must not contain empty levels")
			}
		}
		out.Criteria = criteria
	default:
		return out, fmt.Errorf("unsupported type %q (choice, noul, score)", q.Type)
	}
	return out, nil
}

func stringMap(v any) (map[string]string, error) {
	switch m := v.(type) {
	case map[string]string:
		return m, nil
	case map[string]any:
		out := make(map[string]string, len(m))
		for key, value := range m {
			s, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("%q is not a string", key)
			}
			out[key] = s
		}
		return out, nil
	case map[any]any:
		out := make(map[string]string, len(m))
		for key, value := range m {
			ks, ok := key.(string)
			if !ok {
				// YAML may decode unquoted true/false keys as booleans.
				if b, isBool := key.(bool); isBool {
					ks = strconv.FormatBool(b)
				} else {
					return nil, fmt.Errorf("criterion key is not a string")
				}
			}
			vs, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("%q is not a string", ks)
			}
			out[ks] = vs
		}
		return out, nil
	default:
		return nil, fmt.Errorf("criteria is not a map")
	}
}

func stringList(v any) ([]string, error) {
	switch list := v.(type) {
	case []string:
		return list, nil
	case []any:
		out := make([]string, len(list))
		for i, value := range list {
			s, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("level %d is not a string", i)
			}
			out[i] = s
		}
		return out, nil
	default:
		return nil, fmt.Errorf("criteria is not a list")
	}
}

type systemOneRequest struct {
	State     json.RawMessage              `json:"state"`
	Model     string                       `json:"model"`
	Questions map[string]systemOneQuestion `json:"questions"`
}

type systemOneResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type typeSafeStatusError struct {
	Code       int
	RetryAfter time.Duration
}

func (e *typeSafeStatusError) Error() string { return fmt.Sprintf("endpoint returned %d", e.Code) }

// interpretTypeSafe sends every configured question in one request. The API
// evaluates them independently, so splitting calls would only resend private
// event state and add cost. Unlike a generative model there is no correction
// prompt: a malformed 200 response is invalid, while transport/429/5xx errors
// consume the same bounded three-attempt budget used by the other provider.
func (c *Client) interpretTypeSafe(ctx context.Context, cfg Config, questions Questions, eventJSON []byte) (map[string]any, int64, string, error) {
	if err := ValidateQuestions(questions); err != nil {
		return nil, 0, cfg.Model, &InvalidOutputError{Detail: err.Error()}
	}
	normalized := make(map[string]systemOneQuestion, len(questions))
	for id, question := range questions {
		normalized[id], _ = normalizeQuestion(question)
	}
	req := systemOneRequest{State: json.RawMessage(eventJSON), Model: cfg.Model, Questions: normalized}
	hc := &http.Client{Timeout: requestTimeout}

	var latencyMs int64
	delay := retryBackoff
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, latencyMs, cfg.Model, ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
		}
		started := time.Now()
		response, err := c.postSystemOne(ctx, hc, cfg, req)
		latencyMs += time.Since(started).Milliseconds()
		if err != nil {
			var status *typeSafeStatusError
			if errors.As(err, &status) && status.Code != http.StatusTooManyRequests && status.Code < 500 {
				return nil, latencyMs, cfg.Model, fmt.Errorf("model call (attempt %d): %w", attempt+1, err)
			}
			if status != nil && status.RetryAfter > delay {
				delay = status.RetryAfter
			}
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
			if attempt == maxAttempts-1 {
				return nil, latencyMs, cfg.Model, fmt.Errorf("model call (attempt %d): %w", attempt+1, err)
			}
			continue
		}
		result, modelID, err := validateSystemOneResponse(response, questions)
		if err != nil {
			return nil, latencyMs, cfg.Model, &InvalidOutputError{Detail: err.Error()}
		}
		return result, latencyMs, modelID, nil
	}
	panic("unreachable")
}

func (c *Client) postSystemOne(ctx context.Context, hc *http.Client, cfg Config, req systemOneRequest) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.BaseURL, "/")+"/systemone", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if key := os.Getenv(cfg.APIKeyEnv); key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		return nil, &typeSafeStatusError{Code: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return body, nil
}

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if delay := time.Until(at); delay > 0 {
			return delay
		}
	}
	return 0
}

func validateSystemOneResponse(body []byte, questions Questions) (map[string]any, string, error) {
	var response systemOneResponse
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return nil, "", fmt.Errorf("decode response: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, "", fmt.Errorf("decode response: expected one JSON object")
	}
	if response.Model == "" {
		return nil, "", fmt.Errorf("response model is required")
	}
	if len(response.Answers) != len(questions) {
		return nil, "", fmt.Errorf("answer count %d does not match question count %d", len(response.Answers), len(questions))
	}
	result := make(map[string]any, len(questions))
	for id, question := range questions {
		raw, ok := response.Answers[id]
		if !ok {
			return nil, "", fmt.Errorf("missing answer %q", id)
		}
		answer, err := validateSystemOneAnswer(raw, question)
		if err != nil {
			return nil, "", fmt.Errorf("answer %q: %w", id, err)
		}
		result[id] = answer
	}
	return result, response.Model, nil
}

func validateSystemOneAnswer(raw json.RawMessage, question Question) (map[string]any, error) {
	decode := func(dst any) error {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(dst); err != nil {
			return err
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return fmt.Errorf("expected one JSON object")
		}
		return nil
	}
	switch question.Type {
	case "noul":
		var answer struct {
			Type string   `json:"type"`
			Noul *float64 `json:"noul"`
		}
		if err := decode(&answer); err != nil {
			return nil, err
		}
		if answer.Type != "noul" || answer.Noul == nil || !unitProbability(*answer.Noul) {
			return nil, fmt.Errorf("invalid noul answer")
		}
		return map[string]any{"type": answer.Type, "noul": *answer.Noul}, nil
	case "choice":
		var answer struct {
			Type          string             `json:"type"`
			Choice        string             `json:"choice"`
			Probabilities map[string]float64 `json:"probabilities"`
			Confidence    *float64           `json:"confidence"`
		}
		if err := decode(&answer); err != nil {
			return nil, err
		}
		criteria, _ := stringMap(question.Criteria)
		if answer.Type != "choice" || answer.Confidence == nil || !unitProbability(*answer.Confidence) {
			return nil, fmt.Errorf("invalid choice answer")
		}
		if _, ok := criteria[answer.Choice]; !ok {
			return nil, fmt.Errorf("choice %q is not configured", answer.Choice)
		}
		if err := validateProbabilities(answer.Probabilities, mapKeys(criteria)); err != nil {
			return nil, err
		}
		for option, probability := range answer.Probabilities {
			if probability > answer.Probabilities[answer.Choice]+1e-9 {
				return nil, fmt.Errorf("choice %q is not the highest-probability option (got %q)", answer.Choice, option)
			}
		}
		return map[string]any{"type": answer.Type, "choice": answer.Choice, "probabilities": answer.Probabilities, "confidence": *answer.Confidence}, nil
	case "score":
		var answer struct {
			Type          string             `json:"type"`
			Score         *float64           `json:"score"`
			Legend        map[string]string  `json:"legend"`
			Probabilities map[string]float64 `json:"probabilities"`
			Confidence    *float64           `json:"confidence"`
		}
		if err := decode(&answer); err != nil {
			return nil, err
		}
		levels, _ := stringList(question.Criteria)
		if answer.Type != "score" || answer.Score == nil || answer.Confidence == nil || !unitProbability(*answer.Confidence) || math.IsNaN(*answer.Score) || math.IsInf(*answer.Score, 0) || *answer.Score < 0 || *answer.Score > float64(len(levels)-1) {
			return nil, fmt.Errorf("invalid score answer")
		}
		keys := make([]string, len(levels))
		for i, level := range levels {
			key := strconv.Itoa(i)
			keys[i] = key
			if answer.Legend[key] != level {
				return nil, fmt.Errorf("legend level %s does not match configured criteria", key)
			}
		}
		if len(answer.Legend) != len(levels) {
			return nil, fmt.Errorf("legend contains unexpected levels")
		}
		if err := validateProbabilities(answer.Probabilities, keys); err != nil {
			return nil, err
		}
		var expected float64
		for i := range levels {
			expected += float64(i) * answer.Probabilities[strconv.Itoa(i)]
		}
		if math.Abs(expected-*answer.Score) > 0.01 {
			return nil, fmt.Errorf("score %v does not match probability-weighted value %v", *answer.Score, expected)
		}
		return map[string]any{"type": answer.Type, "score": *answer.Score, "legend": answer.Legend, "probabilities": answer.Probabilities, "confidence": *answer.Confidence}, nil
	default:
		return nil, fmt.Errorf("unsupported question type %q", question.Type)
	}
}

func unitProbability(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func validateProbabilities(probabilities map[string]float64, keys []string) error {
	if len(probabilities) != len(keys) {
		return fmt.Errorf("probability keys do not match criteria")
	}
	var sum float64
	for _, key := range keys {
		value, ok := probabilities[key]
		if !ok || !unitProbability(value) {
			return fmt.Errorf("invalid probability for %q", key)
		}
		sum += value
	}
	if math.Abs(sum-1) > 0.01 {
		return fmt.Errorf("probabilities sum to %v, want 1", sum)
	}
	return nil
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
