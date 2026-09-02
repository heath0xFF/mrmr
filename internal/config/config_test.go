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
			name: "rule with neither notify nor ignore",
			mutate: func(c *Config) {
				c.Policy.Rules[0].Then = policy.Then{}
			},
			want: "policy rule 1: must set notify or ignore",
		},
		{
			name: "rule with both notify and ignore",
			mutate: func(c *Config) {
				c.Policy.Rules[0].Then = policy.Then{Notify: &policy.Notify{Via: "stdout"}, Ignore: true}
			},
			want: "policy rule 1: set either notify or ignore, not both",
		},
		{
			name: "default with both notify and ignore",
			mutate: func(c *Config) {
				c.Policy.Default = policy.Then{Notify: &policy.Notify{Via: "stdout"}, Ignore: true}
			},
			want: "default: set either notify or ignore, not both",
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

	// The shipped example must route an actionable event at the configured
	// threshold to notify. Pin both sides of the boundary in the file users
	// copy: importance 0.8 must notify (a strict "> 0.8" drops it into
	// default ignore) and 0.79 must not (the threshold is 0.8, not lower).
	// This catches a yaml revert that the policy package's operator test
	// cannot see.
	if then, _ := cfg.Policy.Evaluate(map[string]any{"importance": 0.8, "requires_action": true}); then.Outcome() != "notify" {
		t.Errorf("importance 0.8 + requires_action = %s, want notify at the inclusive threshold", then.Outcome())
	}
	if then, _ := cfg.Policy.Evaluate(map[string]any{"importance": 0.79, "requires_action": true}); then.Outcome() != "ignore" {
		t.Errorf("importance 0.79 + requires_action = %s, want ignore below the threshold", then.Outcome())
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
