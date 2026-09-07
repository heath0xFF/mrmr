// Package config loads and validates the mrmr YAML config. Validation happens
// before anything is activated: an invalid config must fail startup loudly
// rather than produce a runtime that silently misbehaves (a policy with no
// then, a model that doesn't exist). Config is the only place mrmr's behavior
// is described, so it is also the first place to look when behavior surprises.
package config

import (
	"bytes"
	"fmt"
	"math"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/heath0xff/mrmr/internal/filter"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
)

type Server struct {
	Addr string `yaml:"addr"`
}

type DB struct {
	Path string `yaml:"path"`
}

type Interpret struct {
	Model  string       `yaml:"model"`
	Prompt string       `yaml:"prompt"`
	Schema model.Schema `yaml:"schema"`
}

// Agent is a delegation target. v0.1 ships only the generic HTTP adapter,
// so there is no type field yet: every agent is an HTTP endpoint that
// receives {event_id, decision, prompt} and returns 2xx.
type Agent struct {
	Endpoint string `yaml:"endpoint"`
}

type Config struct {
	Server Server `yaml:"server"`
	DB     DB     `yaml:"db"`
	// Filter is the pre-model gate (IMPLEMENTATION.md section 3):
	// deterministic checks that run before any model call. An event
	// that fails any Spec is recorded and ignored before
	// interpretation.
	Filter    filter.List             `yaml:"filter"`
	Models    map[string]model.Config `yaml:"models"`
	Interpret Interpret               `yaml:"interpret"`
	// Policy is inlined so the YAML reads exactly like IMPLEMENTATION.md:
	// a top-level `policy:` list plus a sibling `default:`.
	Policy policy.Policy `yaml:"policy,inline"`
	// Agents are delegation targets referenced by name from policy rules.
	// Endpoints live here, not in policy, so policies stay readable and
	// agent locations change without touching rule logic.
	Agents map[string]Agent `yaml:"agents"`
}

// Load reads, parses, and validates the config at path. Defaults (addr,
// db path) are applied for the values a minimal local setup never needs to
// set; anything the runtime cannot run correctly without stays required.
// Decoding is strict — unknown keys are errors, not warnings — because a
// typo like `api_keyenv:` silently disabling auth is far worse than a
// refused startup.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if c.Server.Addr == "" {
		c.Server.Addr = ":4242"
	}
	if c.DB.Path == "" {
		c.DB.Path = "mrmr.db"
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) Validate() error {
	if c.Interpret.Model == "" {
		return fmt.Errorf("config: interpret.model is required")
	}
	mc, ok := c.Models[c.Interpret.Model]
	if !ok {
		return fmt.Errorf("config: interpret.model %q not defined in models", c.Interpret.Model)
	}
	if mc.Provider != "openai-compatible" {
		return fmt.Errorf("config: model %q: unsupported provider %q (v0.1: openai-compatible)", c.Interpret.Model, mc.Provider)
	}
	if mc.BaseURL == "" {
		return fmt.Errorf("config: model %q: base_url is required", c.Interpret.Model)
	}
	if c.Interpret.Prompt == "" {
		return fmt.Errorf("config: interpret.prompt is required")
	}
	if len(c.Interpret.Schema) == 0 {
		return fmt.Errorf("config: interpret.schema is required")
	}
	for name, f := range c.Interpret.Schema {
		switch f.Type {
		case "string", "number", "boolean":
		default:
			return fmt.Errorf("config: schema field %q: unsupported type %q", name, f.Type)
		}
		if f.Type != "number" && (f.Minimum != nil || f.Maximum != nil) {
			return fmt.Errorf("config: schema field %q: minimum/maximum require type number", name)
		}
		if (f.Minimum != nil && (math.IsNaN(*f.Minimum) || math.IsInf(*f.Minimum, 0))) ||
			(f.Maximum != nil && (math.IsNaN(*f.Maximum) || math.IsInf(*f.Maximum, 0))) {
			return fmt.Errorf("config: schema field %q: minimum/maximum must be finite", name)
		}
		if f.Minimum != nil && f.Maximum != nil && *f.Minimum > *f.Maximum {
			return fmt.Errorf("config: schema field %q: minimum must not exceed maximum", name)
		}
	}
	for i, f := range c.Filter {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("config: filter %d: %w", i+1, err)
		}
	}

	validateThen := func(t policy.Then, where string) error {
		// A rule that sets several outcomes is almost certainly a YAML typo
		// (a leftover ignore under a new notify). Rather than picking a
		// winner, reject it: ambiguity in outcome selection is exactly the
		// kind of thing policy must never have.
		set := 0
		for _, b := range []bool{t.Notify != nil, t.Action != nil, t.Delegate != nil, t.Ignore} {
			if b {
				set++
			}
		}
		if set > 1 {
			return fmt.Errorf("config: %s: set at most one of notify, action, delegate, ignore", where)
		}
		if t.Notify != nil && t.Notify.Via != "stdout" {
			return fmt.Errorf("config: %s: notify.via %q not supported (v0.1: stdout)", where, t.Notify.Via)
		}
		if t.Action != nil {
			switch t.Action.Type {
			case "http":
				if t.Action.URL == "" {
					return fmt.Errorf("config: %s: action.url is required for type http", where)
				}
			case "emit":
				if t.Action.EventType == "" {
					return fmt.Errorf("config: %s: action.event_type is required for type emit", where)
				}
			default:
				return fmt.Errorf("config: %s: action.type %q not supported (v0.1: http, emit)", where, t.Action.Type)
			}
		}
		if t.Delegate != nil {
			if t.Delegate.Agent == "" {
				return fmt.Errorf("config: %s: delegate.agent is required", where)
			}
			// The reference is resolved now, at validation time, so a typo'd
			// agent name fails startup instead of surfacing as a mid-flight
			// "unknown agent" execution error on some future event.
			if _, ok := c.Agents[t.Delegate.Agent]; !ok {
				return fmt.Errorf("config: %s: delegate.agent %q not defined in agents", where, t.Delegate.Agent)
			}
		}
		if set == 0 && !t.Shadow {
			return fmt.Errorf("config: %s: must set notify, action, delegate, ignore: true, or shadow: true", where)
		}
		return nil
	}
	for name, a := range c.Agents {
		if a.Endpoint == "" {
			return fmt.Errorf("config: agent %q: endpoint is required", name)
		}
	}
	for i, r := range c.Policy.Rules {
		if len(r.If) == 0 {
			return fmt.Errorf("config: policy rule %d: empty if", i+1)
		}
		if err := validateThen(r.Then, fmt.Sprintf("policy rule %d", i+1)); err != nil {
			return err
		}
	}
	return validateThen(c.Policy.Default, "default")
}
