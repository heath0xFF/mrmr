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

type Config struct {
	Server    Server                  `yaml:"server"`
	DB        DB                      `yaml:"db"`
	Models    map[string]model.Config `yaml:"models"`
	Interpret Interpret               `yaml:"interpret"`
	// Policy is inlined so the YAML reads exactly like IMPLEMENTATION.md:
	// a top-level `policy:` list plus a sibling `default:`.
	Policy policy.Policy `yaml:"policy,inline"`
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
	validateThen := func(t policy.Then, where string) error {
		// A rule that sets both notify and ignore is almost certainly a YAML
		// typo (a leftover ignore under a new notify). Rather than picking a
		// winner, reject it: ambiguity in outcome selection is exactly the
		// kind of thing policy must never have.
		if t.Notify != nil && t.Ignore {
			return fmt.Errorf("config: %s: set either notify or ignore, not both", where)
		}
		if t.Notify != nil && t.Notify.Via != "stdout" {
			return fmt.Errorf("config: %s: notify.via %q not supported (v0.1: stdout)", where, t.Notify.Via)
		}
		if t.Notify == nil && !t.Ignore {
			return fmt.Errorf("config: %s: must set notify or ignore: true", where)
		}
		return nil
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
