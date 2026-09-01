// These tests pin the model client's contract with OpenAI-compatible
// endpoints: schema-validated structured output despite unreliable local
// models. The invariant that matters most is that *InvalidOutputError is
// distinguishable from transport failure — the runtime routes invalid
// output to a retry-with-feedback path and transport failure to an errored
// decision, so conflating them would break the fail-toward-ignore story.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testConfig(baseURL string) Config {
	return Config{Provider: "openai-compatible", BaseURL: baseURL, Model: "mock-model"}
}

func testSchema() Schema {
	min, max := 0.0, 1.0
	return Schema{
		"category":   {Type: "string", Enum: []any{"important", "unimportant"}},
		"importance": {Type: "number", Minimum: &min, Maximum: &max},
	}
}

// chatBody renders a minimal chat completion response carrying the given
// assistant content. Local model servers vary wildly in response shape;
// this is the one shape the client actually depends on.
func chatBody(content string) map[string]any {
	return map[string]any{
		"choices": []any{
			map[string]any{"message": map[string]any{"content": content}},
		},
	}
}

// contentServer serves the given assistant contents in request order,
// repeating the last one if the client asks more times than expected.
// Each HTTP request decodes to exactly one model attempt, which makes the
// sequence the observable behavior under test.
func contentServer(t *testing.T, contents ...string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := calls
		calls++
		content := contents[len(contents)-1]
		if i < len(contents) {
			content = contents[i]
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(chatBody(content))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestInterpretHappyPath(t *testing.T) {
	srv, calls := contentServer(t, `{"category":"important","importance":0.95}`)
	c := &Client{}
	res, _, model, err := c.Interpret(context.Background(), testConfig(srv.URL), "mock", "classify", testSchema(), []byte(`{}`))
	if err != nil {
		t.Fatalf("Interpret: %v", err)
	}
	if res["category"] != "important" || res["importance"] != 0.95 {
		t.Errorf("result = %v, want category=important importance=0.95", res)
	}
	if model != "mock-model" {
		t.Errorf("model = %q, want mock-model", model)
	}
	if *calls != 1 {
		t.Errorf("endpoint called %d times, want 1 (valid output must not retry)", *calls)
	}
}

func TestInterpretFencedOrProseWrappedJSON(t *testing.T) {
	// Local models routinely wrap JSON in markdown fences or chatter.
	// The client must extract the object rather than fail the parse,
	// otherwise every chatty model looks like a schema violation.
	wrapped := "Sure! Here's my analysis:\n```json\n{\"category\":\"unimportant\",\"importance\":0.1}\n```\nHope that helps."
	srv, _ := contentServer(t, wrapped)
	c := &Client{}
	res, _, _, err := c.Interpret(context.Background(), testConfig(srv.URL), "mock", "classify", testSchema(), []byte(`{}`))
	if err != nil {
		t.Fatalf("Interpret with fenced content: %v", err)
	}
	if res["category"] != "unimportant" || res["importance"] != 0.1 {
		t.Errorf("result = %v, want fenced JSON parsed", res)
	}
}

func TestInterpretInvalidThenCorrectedRetry(t *testing.T) {
	// The self-correction path: the first output fails validation and the
	// client replays the conversation with the validation error appended,
	// giving the model one chance to fix itself. The retry request must
	// carry the failed assistant turn so the endpoint can see what to fix.
	var retryHadAssistantTurn bool
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		json.NewDecoder(r.Body).Decode(&req)
		i := calls
		calls++
		if i == 0 {
			json.NewEncoder(w).Encode(chatBody(`{"category":"not-a-category","importance":"high"}`))
			return
		}
		retryHadAssistantTurn = len(req.Messages) == 4 && req.Messages[2].Role == "assistant"
		json.NewEncoder(w).Encode(chatBody(`{"category":"important","importance":0.9}`))
	}))
	t.Cleanup(srv.Close)

	c := &Client{}
	res, _, _, err := c.Interpret(context.Background(), testConfig(srv.URL), "mock", "classify", testSchema(), []byte(`{}`))
	if err != nil {
		t.Fatalf("Interpret after corrected retry: %v", err)
	}
	if res["category"] != "important" {
		t.Errorf("result = %v, want corrected retry output", res)
	}
	if calls != 2 {
		t.Errorf("endpoint called %d times, want 2 (initial + corrected retry)", calls)
	}
	if !retryHadAssistantTurn {
		t.Error("retry request must include the invalid assistant turn for self-correction")
	}
}

func TestInterpretInvalidTwiceReturnsInvalidOutputError(t *testing.T) {
	// After the corrected retry also fails, the client must give up with
	// *InvalidOutputError — not keep retrying, and not a generic error,
	// since the runtime distinguishes invalid from errored decisions.
	srv, calls := contentServer(t, `{"category":"urgent","importance":0.9}`, `still not json`)
	c := &Client{}
	_, _, _, err := c.Interpret(context.Background(), testConfig(srv.URL), "mock", "classify", testSchema(), []byte(`{}`))
	var inv *InvalidOutputError
	if !errors.As(err, &inv) {
		t.Fatalf("err = %v, want *InvalidOutputError", err)
	}
	if *calls != 2 {
		t.Errorf("endpoint called %d times, want 2 (initial + one retry)", *calls)
	}
}

func TestInterpretResponseFormatFallback(t *testing.T) {
	// Some OpenAI-compatible endpoints reject the response_format
	// parameter with a 400. The client detects that specific failure and
	// retries the same request without response_format, falling back to
	// validation alone to enforce the schema.
	calls := 0
	var lastRequestHadFormat *bool
	hadFormat := false
	lastRequestHadFormat = &hadFormat
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		json.NewDecoder(r.Body).Decode(&req)
		calls++
		if req.ResponseFormat != nil {
			*lastRequestHadFormat = true
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "response_format is not supported")
			return
		}
		*lastRequestHadFormat = false
		json.NewEncoder(w).Encode(chatBody(`{"category":"important","importance":0.85}`))
	}))
	t.Cleanup(srv.Close)

	c := &Client{}
	res, _, _, err := c.Interpret(context.Background(), testConfig(srv.URL), "mock", "classify", testSchema(), []byte(`{}`))
	if err != nil {
		t.Fatalf("Interpret with response_format-rejecting endpoint: %v", err)
	}
	if res["category"] != "important" {
		t.Errorf("result = %v, want fallback (no response_format) result", res)
	}
	if calls != 2 || *lastRequestHadFormat {
		t.Errorf("calls=%d lastHadFormat=%v, want 2 calls with final request omitting response_format", calls, *lastRequestHadFormat)
	}
}

func TestInterpretTransportFailureAfterRetries(t *testing.T) {
	// A hard-down endpoint must surface a transport error (not
	// InvalidOutputError) after the bounded attempt count, with no
	// unbounded retry loop. Attempts are spaced by retryBackoff, so this
	// test costs two backoff periods.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	t.Cleanup(srv.Close)

	c := &Client{}
	_, _, _, err := c.Interpret(context.Background(), testConfig(srv.URL), "mock", "classify", testSchema(), []byte(`{}`))
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	var inv *InvalidOutputError
	if errors.As(err, &inv) {
		t.Fatalf("transport failure must not be reported as *InvalidOutputError, got %v", err)
	}
	if calls != maxAttempts {
		t.Errorf("endpoint called %d times, want %d (bounded attempts)", calls, maxAttempts)
	}
}

func TestInterpretNonRetryable4xxReturnsImmediately(t *testing.T) {
	// A 401 (bad key) or 404 (bad model name) will never succeed on retry;
	// burning the attempt budget on them just triples the noise per event.
	// Only network errors and 5xx consume the retry budget.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "bad key")
	}))
	t.Cleanup(srv.Close)

	c := &Client{}
	_, _, _, err := c.Interpret(context.Background(), testConfig(srv.URL), "mock", "classify", testSchema(), []byte(`{}`))
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if calls != 1 {
		t.Errorf("endpoint called %d times, want 1 (non-retryable)", calls)
	}
}

func TestValidate(t *testing.T) {
	min, max := 0.0, 1.0
	schema := Schema{
		"category":   {Type: "string", Enum: []any{"important", "unimportant"}},
		"importance": {Type: "number", Minimum: &min, Maximum: &max},
		"flag":       {Type: "boolean"},
	}
	tests := []struct {
		name    string
		res     map[string]any
		wantErr string // empty = expect success
	}{
		{
			name: "valid",
			res:  map[string]any{"category": "important", "importance": 0.9, "flag": true},
		},
		{
			name:    "wrong type: string where number",
			res:     map[string]any{"category": "important", "importance": "0.9", "flag": true},
			wantErr: `field "importance": want number`,
		},
		{
			name:    "wrong type: number where string",
			res:     map[string]any{"category": 7, "importance": 0.9, "flag": true},
			wantErr: `field "category": want string`,
		},
		{
			name:    "number below minimum",
			res:     map[string]any{"category": "important", "importance": -0.1, "flag": true},
			wantErr: `field "importance": value -0.1 below minimum 0`,
		},
		{
			name:    "number above maximum",
			res:     map[string]any{"category": "important", "importance": 8.0, "flag": true},
			wantErr: `field "importance": value 8 above maximum 1`,
		},
		{
			name:    "wrong type: string where boolean",
			res:     map[string]any{"category": "important", "importance": 0.9, "flag": "true"},
			wantErr: `field "flag": want boolean`,
		},
		{
			name:    "missing field",
			res:     map[string]any{"importance": 0.9, "flag": true},
			wantErr: `missing field "category"`,
		},
		{
			name:    "enum violation",
			res:     map[string]any{"category": "urgent", "importance": 0.9, "flag": true},
			wantErr: `field "category": value urgent not in enum`,
		},
		{
			name:    "unexpected extra field",
			res:     map[string]any{"category": "important", "importance": 0.9, "flag": true, "extra": "nope"},
			wantErr: `unexpected field "extra"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate(tt.res, schema)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("validate(%v) = %v, want nil", tt.res, err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("validate(%v) = %v, want error %q", tt.res, err, tt.wantErr)
			}
		})
	}
}

func TestSchemaMapIncludesNumberBounds(t *testing.T) {
	importance := schemaMap(testSchema())["properties"].(map[string]any)["importance"].(map[string]any)
	if importance["minimum"] != 0.0 || importance["maximum"] != 1.0 {
		t.Errorf("importance schema = %v, want minimum=0 maximum=1", importance)
	}
}
