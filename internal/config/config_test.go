// These tests pin the config loader's contract: an invalid config must
// never validate, defaults must apply exactly where documented, and strict
// decoding must reject unknown keys rather than silently ignore a typo
// like `api_keyenv:`. Config is the only place mrmr's behavior is
// described, so every way it can be wrong has to fail loudly here before
// the runtime ever trusts it.
package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/filter"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
)

// validConfig is the smallest configuration Validate accepts. Every error
// case mutates exactly one field so a failure points at the rule under
// test, not at a broken fixture.
func validConfig() *Config {
	return &Config{
		Models: map[string]model.Config{
			"m": {Provider: "openai-compatible", BaseURL: "http://localhost:1/v1", Model: "mock"},
		},
		Filter: filter.List{{Field: "source", Op: filter.OpNeq, Value: "noise-bot"}},
		Interpret: Interpret{
			Model:  "m",
			Prompt: "classify this event",
			Schema: model.Schema{
				"category":   {Type: "string"},
				"importance": {Type: "number"},
			},
		},
		Policy: policy.Policy{
			Rules: []policy.Rule{{
				If:   map[string]any{"result.importance": "> 0.8"},
				Then: policy.Then{Notify: &policy.Notify{Via: "stdout"}},
			}},
			Default: policy.Then{Ignore: true},
		},
		Agents: map[string]Agent{"triage": {Endpoint: "http://localhost:9000/tasks"}},
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string // substring of the expected error
	}{
		{
			name:   "missing interpret.model",
			mutate: func(c *Config) { c.Interpret.Model = "" },
			want:   "interpret.model is required",
		},
		{
			name:   "interpret.model not defined in models",
			mutate: func(c *Config) { c.Interpret.Model = "nope" },
			want:   `interpret.model "nope" not defined in models`,
		},
		{
			name: "unsupported provider",
			mutate: func(c *Config) {
				m := c.Models["m"]
				m.Provider = "anthropic"
				c.Models["m"] = m
			},
			want: `unsupported provider "anthropic"`,
		},
		{
			name: "missing base_url",
			mutate: func(c *Config) {
				m := c.Models["m"]
				m.BaseURL = ""
				c.Models["m"] = m
			},
			want: "base_url is required",
		},
		{
			name:   "missing prompt",
			mutate: func(c *Config) { c.Interpret.Prompt = "" },
			want:   "interpret.prompt is required",
		},
		{
			name:   "empty schema",
			mutate: func(c *Config) { c.Interpret.Schema = nil },
			want:   "interpret.schema is required",
		},
		{
			name:   "schema field with unsupported type",
			mutate: func(c *Config) { c.Interpret.Schema["category"] = model.Field{Type: "array"} },
			want:   `schema field "category": unsupported type "array"`,
		},
		{
			name: "bounds on non-number field",
			mutate: func(c *Config) {
				min := 0.0
				c.Interpret.Schema["category"] = model.Field{Type: "string", Minimum: &min}
			},
			want: `schema field "category": minimum/maximum require type number`,
		},
		{
			name: "non-finite bound",
			mutate: func(c *Config) {
				min := math.NaN()
				c.Interpret.Schema["importance"] = model.Field{Type: "number", Minimum: &min}
			},
			want: `schema field "importance": minimum/maximum must be finite`,
		},
		{
			name: "minimum exceeds maximum",
			mutate: func(c *Config) {
				min, max := 2.0, 1.0
				c.Interpret.Schema["importance"] = model.Field{Type: "number", Minimum: &min, Maximum: &max}
			},
			want: `schema field "importance": minimum must not exceed maximum`,
		},
		{
			name:   "policy rule with empty if",
			mutate: func(c *Config) { c.Policy.Rules[0].If = nil },
			want:   "policy rule 1: empty if",
		},
		{
			name: "rule with unsupported notify.via",
			mutate: func(c *Config) {
				c.Policy.Rules[0].Then = policy.Then{Notify: &policy.Notify{Via: "email"}}
			},
			want: `notify.via "email" not supported`,
		},
		{
			name: "default with unsupported notify.via",
			mutate: func(c *Config) {
				c.Policy.Default = policy.Then{Notify: &policy.Notify{Via: "sms"}}
			},
			want: `default: notify.via "sms" not supported`,
		},
		{
			name: "rule with neither outcome nor shadow",
			mutate: func(c *Config) {
				c.Policy.Rules[0].Then = policy.Then{}
			},
			want: "policy rule 1: must set notify, action, delegate, ignore: true, or shadow: true",
		},
		{
			name: "rule with both notify and ignore",
			mutate: func(c *Config) {
				c.Policy.Rules[0].Then = policy.Then{Notify: &policy.Notify{Via: "stdout"}, Ignore: true}
			},
			want: "policy rule 1: set at most one of notify, action, delegate, ignore",
		},
		{
			name: "rule with unsupported action type",
			mutate: func(c *Config) {
				c.Policy.Rules[0].Then = policy.Then{Action: &policy.Action{Type: "exec", URL: "http://x"}}
			},
			want: `policy rule 1: action.type "exec" not supported`,
		},
		{
			name: "rule with http action and no url",
			mutate: func(c *Config) {
				c.Policy.Rules[0].Then = policy.Then{Action: &policy.Action{Type: "http"}}
			},
			want: "policy rule 1: action.url is required for type http",
		},
		{
			name: "rule with emit action and no event_type",
			mutate: func(c *Config) {
				c.Policy.Rules[0].Then = policy.Then{Action: &policy.Action{Type: "emit"}}
			},
			want: "policy rule 1: action.event_type is required for type emit",
		},
		{
			name: "rule delegating to an unknown agent",
			mutate: func(c *Config) {
				c.Policy.Rules[0].Then = policy.Then{Delegate: &policy.Delegate{Agent: "nope"}}
			},
			want: `policy rule 1: delegate.agent "nope" not defined in agents`,
		},
		{
			name: "agent without endpoint",
			mutate: func(c *Config) {
				c.Agents["triage"] = Agent{}
			},
			want: `config: agent "triage": endpoint is required`,
		},
		{
			name: "default with both notify and ignore",
			mutate: func(c *Config) {
				c.Policy.Default = policy.Then{Notify: &policy.Notify{Via: "stdout"}, Ignore: true}
			},
			want: "default: set at most one of notify, action, delegate, ignore",
		},
		{
			name: "default with neither outcome nor shadow",
			mutate: func(c *Config) {
				c.Policy.Default = policy.Then{}
			},
			want: "default: must set notify, action, delegate, ignore: true, or shadow: true",
		},
		{
			name:   "filter with unknown op",
			mutate: func(c *Config) { c.Filter[0].Op = "lt" },
			want:   `op "lt" not supported`,
		},
		{
			name:   "filter with unknown field root",
			mutate: func(c *Config) { c.Filter[0].Field = "foo.bar" },
			want:   `unknown root "foo"`,
		},
		{
			name:   "filter with bare data root",
			mutate: func(c *Config) { c.Filter[0].Field = "data" },
			want:   "data requires a sub-path",
		},
		{
			name:   "filter with sub-path under scalar root",
			mutate: func(c *Config) { c.Filter[0].Field = "type.x" },
			want:   "type has no sub-paths",
		},
		{
			name:   "filter with missing value",
			mutate: func(c *Config) { c.Filter[0].Value = nil },
			want:   "value is required",
		},
		{
			name:   "filter with empty field",
			mutate: func(c *Config) { c.Filter[0].Field = "" },
			want:   "field is required",
		},
		{
			name:   "filter with double event prefix",
			mutate: func(c *Config) { c.Filter[0].Field = "event.event.data.author" },
			want:   `unknown root "event"`,
		},
		{
			name:   "filter with list value",
			mutate: func(c *Config) { c.Filter[0].Value = []any{"a", "b"} },
			want:   "value must be a string, number, or boolean",
		},
		{
			name:   "filter with map value",
			mutate: func(c *Config) { c.Filter[0].Value = map[string]any{"a": "b"} },
			want:   "value must be a string, number, or boolean",
		},
		{
			name:   "filter with NaN value",
			mutate: func(c *Config) { c.Filter[0].Value = math.NaN() },
			want:   "value must be a string, number, or boolean",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() error = %q, want containing %q", err.Error(), tt.want)
			}
		})
	}
}

// TestValidateAcceptsBothFieldPathForms pins the two documented
// spellings of a filter field: IMPLEMENTATION.md section 3 uses
// data.author and the flow example uses event.data.author. Both must
// validate, or a documented example fails strict config validation.
func TestValidateAcceptsBothFieldPathForms(t *testing.T) {
	for _, field := range []string{"data.author", "event.data.author"} {
		c := validConfig()
		c.Filter = filter.List{{Field: field, Op: filter.OpNeq, Value: "x"}}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() for %q = %v, want nil", field, err)
		}
	}
}

// TestExampleConfigQuickstartEventPassesFilters pins the quickstart
// contract: the README event has no data.author, and the fail-closed
// missing-field rule would stop it before interpretation if the
// shipped example filtered on an optional field. The example filters
// on a required root instead, so the quickstart must pass.
func TestExampleConfigQuickstartEventPassesFilters(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "mrmr.example.yaml"))
	if err != nil {
		t.Fatalf("Load example config: %v", err)
	}
	if len(cfg.Filter) == 0 {
		t.Fatal("example config has no filter: nothing to check")
	}
	e := event.Event{
		Type:   "test.message",
		Source: "curl",
		Data:   map[string]any{"message": "The production API has returned 500 errors for five minutes."},
	}
	if ok, reason := cfg.Filter.Evaluate(e); !ok {
		t.Fatalf("quickstart event fails the example filters: %s", reason)
	}
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for a fully valid config", err)
	}
}

// writeConfig persists YAML text to a temp file so every Load test gets an
// isolated file; no test writes inside the repo.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mrmr.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// minimalYAML omits server.addr and db.path so the defaults case and the
// error-case mutations all start from the same textual baseline.
const minimalYAML = `
models:
  m:
    provider: openai-compatible
    base_url: http://localhost:1/v1
    model: mock
interpret:
  model: m
  prompt: classify this event
  schema:
    category:
      type: string
policy:
  - if:
      result.category: incident
    then:
      notify:
        via: stdout
default:
  ignore: true
`

func TestLoadExampleConfig(t *testing.T) {
	// The shipped example must always load: it is the first thing a new
	// user runs, so any drift between it and Validate is a bug in one of them.
	cfg, err := Load("../../mrmr.example.yaml")
	if err != nil {
		t.Fatalf("Load(mrmr.example.yaml): %v", err)
	}
	if cfg.Server.Addr != ":4242" || cfg.DB.Path != "mrmr.db" {
		t.Errorf("example addr/db = %q/%q, want :4242/mrmr.db", cfg.Server.Addr, cfg.DB.Path)
	}
	if cfg.Interpret.Model != "fast-local" {
		t.Errorf("interpret.model = %q, want fast-local", cfg.Interpret.Model)
	}
	importance := cfg.Interpret.Schema["importance"]
	if importance.Minimum == nil || *importance.Minimum != 0 || importance.Maximum == nil || *importance.Maximum != 1 {
		t.Errorf("importance bounds = %v/%v, want 0/1", importance.Minimum, importance.Maximum)
	}

	// The shipped example must route an incident with confidence above the
	// configured threshold to notify, and an incident below it to default
	// ignore. Pin both sides of the boundary in the file users copy.
	if then, _ := cfg.Policy.Evaluate(map[string]any{"category": "incident", "confidence": 0.86}); then.Outcome() != "notify" {
		t.Errorf("incident 0.86 = %s, want notify above the threshold", then.Outcome())
	}
	if then, _ := cfg.Policy.Evaluate(map[string]any{"category": "incident", "confidence": 0.84}); then.Outcome() != "ignore" {
		t.Errorf("incident 0.84 = %s, want ignore below the threshold", then.Outcome())
	}
	if len(cfg.Agents) != 1 || cfg.Agents["triage"].Endpoint != "http://localhost:9000/tasks" {
		t.Errorf("example agents = %v, want one triage agent", cfg.Agents)
	}
}

func TestLoadDefaultsWhenOmitted(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":4242" {
		t.Errorf("server.addr = %q, want default :4242", cfg.Server.Addr)
	}
	if cfg.DB.Path != "mrmr.db" {
		t.Errorf("db.path = %q, want default mrmr.db", cfg.DB.Path)
	}
}

func TestLoadStrictUnknownKeys(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			// `api_keyenv` is the exact typo that would silently disable
			// auth header injection if unknown keys were ignored.
			name: "misspelled api_key_env in model",
			yaml: `
models:
  m:
    provider: openai-compatible
    base_url: http://localhost:1/v1
    model: mock
    api_keyenv: MRMR_MODEL_API_KEY
interpret:
  model: m
  prompt: p
  schema:
    category:
      type: string
policy: []
default:
  ignore: true
`,
			want: "api_keyenv",
		},
		{
			name: "stray top-level key",
			yaml: minimalYAML + "\nfoo: bar\n",
			want: "foo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.yaml))
			if err == nil {
				t.Fatalf("Load = nil, want error for unknown key")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load error = %q, want it naming the unknown key %q", err.Error(), tt.want)
			}
		})
	}
}

func TestLoadBadYAMLSyntax(t *testing.T) {
	_, err := Load(writeConfig(t, "server: [unclosed\n  bad: {"))
	if err == nil {
		t.Fatal("Load = nil, want parse error for invalid YAML")
	}
}

// TestValidateSourceErrors covers the source list: names are required and
// unique (they become Event.Source and cursor keys), the type is gated, and
// the interval floor protects the polled target.
func TestValidateSourceErrors(t *testing.T) {
	valid := Source{
		Name: "feed", Type: "http-poller", URL: "http://localhost/feed",
		Every: time.Minute, CursorField: "id", EventType: "feed.item.published",
	}
	tests := []struct {
		name   string
		mutate func(*Source)
		want   string
	}{
		{
			name:   "missing name",
			mutate: func(s *Source) { s.Name = "" },
			want:   "source 1: name is required",
		},
		{
			name:   "unsupported type",
			mutate: func(s *Source) { s.Type = "mcp-poller" },
			want:   `source "feed": type "mcp-poller" not supported`,
		},
		{
			name:   "missing url",
			mutate: func(s *Source) { s.URL = "" },
			want:   `source "feed": url is required`,
		},
		{
			name:   "interval below the floor",
			mutate: func(s *Source) { s.Every = 500 * time.Millisecond },
			want:   `source "feed": every must be at least 1s`,
		},
		{
			name:   "missing event_type",
			mutate: func(s *Source) { s.EventType = "" },
			want:   `source "feed": event_type is required`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			s := valid
			tt.mutate(&s)
			c.Sources = []Source{s}
			err := c.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() error = %q, want containing %q", err.Error(), tt.want)
			}
		})
	}

	// Duplicate names fail even when both sources are otherwise valid.
	c := validConfig()
	c.Sources = []Source{valid, valid}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), `duplicate name`) {
		t.Errorf("Validate() = %v, want duplicate name error", err)
	}
}

// TestLoadSourceYAML pins the wire format, including the duration string
// parsing ("every: 2m"), so the documented example keeps loading.
func TestLoadSourceYAML(t *testing.T) {
	yamlText := minimalYAML + `
sources:
  - name: blog
    type: http-poller
    url: "http://localhost/feed.json?after={{ .cursor }}"
    every: 2m
    item_path: items
    cursor_field: id
    subject_field: title
    event_type: rss.item.published
`
	path := writeConfig(t, yamlText)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(cfg.Sources))
	}
	s := cfg.Sources[0]
	if s.Name != "blog" || s.Every != 2*time.Minute || s.ItemPath != "items" ||
		s.CursorField != "id" || s.SubjectField != "title" || s.EventType != "rss.item.published" {
		t.Errorf("source = %+v, want the documented example", s)
	}
}
