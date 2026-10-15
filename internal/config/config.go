// Package config loads and validates the mrmr YAML config. Validation happens
// before anything is activated: an invalid config must fail startup loudly
// rather than produce a runtime that silently misbehaves (a policy with no
// then, a model that doesn't exist). Config is the only place mrmr's behavior
// is described, so it is also the first place to look when behavior surprises.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/heath0xff/mrmr/internal/agent"
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

type Interpret = model.Interpretation

// Source configures a built-in pull adapter. Common fields name its events
// and schedule; adapter-specific fields are rejected on the wrong type so
// a misplaced option cannot silently broaden ingestion. Secrets are env
// references, never values stored in this configuration.
type Source struct {
	Name           string        `yaml:"name"`
	Type           string        `yaml:"type"`
	URL            string        `yaml:"url"`              // may reference {{ .cursor }}
	Every          time.Duration `yaml:"every"`            // min 1s: protects the polled target
	BearerTokenEnv string        `yaml:"bearer_token_env"` // optional
	ItemPath       string        `yaml:"item_path"`        // optional dot-path to the record array
	CursorField    string        `yaml:"cursor_field"`     // optional; dedup id + watermark
	SubjectField   string        `yaml:"subject_field"`    // optional
	TimestampField string        `yaml:"timestamp_field"`  // optional; RFC3339
	EventType      string        `yaml:"event_type"`
	Units          []string      `yaml:"units"`    // systemd-journal: exact system unit names
	Priority       string        `yaml:"priority"` // systemd-journal: this severity or higher
}

// A destination secret must be a literal environment reference, not a URL
// or model template that could change the adapter's authority.
var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Exact names only: no patterns, journalctl options, paths, or user-unit
// selectors. This intentionally accepts a conservative subset of systemd
// names; escaped unit names can be added when a real source needs them.
var journalUnitName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@:-]*\.(service|socket|timer|target|mount|automount|path|slice|scope|swap|device)$`)

// JournalPriority follows syslog numbering: smaller means more severe.
// Configuration and the source use the same mapping to avoid a filter that
// silently interprets "warning" differently at the two boundaries.
func JournalPriority(name string) (int, bool) {
	for i, p := range []string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"} {
		if p == name {
			return i, true
		}
	}
	return 0, false
}

type Agent = agent.Config

type Config struct {
	Server Server `yaml:"server"`
	DB     DB     `yaml:"db"`
	// Filter is the pre-model gate (IMPLEMENTATION.md section 3):
	// deterministic checks that run before any model call. An event
	// that fails any Spec is recorded and ignored before
	// interpretation.
	Filter          filter.List             `yaml:"filter"`
	Models          map[string]model.Config `yaml:"models"`
	Interpret       Interpret               `yaml:"interpret"`
	InterpretByType map[string]Interpret    `yaml:"interpret_by_type"`
	// Policy is inlined so the YAML reads exactly like IMPLEMENTATION.md:
	// a top-level `policy:` list plus a sibling `default:`.
	Policy policy.Policy `yaml:"policy,inline"`
	// Agents are delegation targets referenced by name from policy rules.
	// Endpoints live here, not in policy, so policies stay readable and
	// agent locations change without touching rule logic.
	Agents map[string]Agent `yaml:"agents"`
	// Sources are pull-based ingesters; each runs its own goroutine and
	// feeds the same pipeline as POST /api/events.
	Sources []Source `yaml:"sources"`
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

// InterpreterFor selects an exact event-type override; there are no patterns,
// branching workflows, or model-controlled routing decisions here.
func (c *Config) InterpreterFor(eventType string) Interpret {
	if spec, ok := c.InterpretByType[eventType]; ok {
		return spec
	}
	return c.Interpret
}

func (c *Config) validateInterpretation() error {
	if c.Interpret.Model == "" {
		return fmt.Errorf("config: interpret.model is required")
	}
	mc, ok := c.Models[c.Interpret.Model]
	if !ok {
		return fmt.Errorf("config: interpret.model %q not defined in models", c.Interpret.Model)
	}
	if mc.Role == "agent" {
		return fmt.Errorf("config: interpret.model must have decision role, not agent")
	}
	if mc.BaseURL == "" {
		return fmt.Errorf("config: model %q: base_url is required", c.Interpret.Model)
	}
	switch mc.Provider {
	case "openai-compatible":
		if len(c.Interpret.Questions) != 0 {
			if c.Interpret.Prompt != "" || len(c.Interpret.Schema) != 0 {
				return fmt.Errorf("config: questions cannot be mixed with prompt/schema")
			}
			return model.ValidateQuestions(c.Interpret.Questions)
		}
		if c.Interpret.Prompt == "" {
			return fmt.Errorf("config: interpret.prompt is required for openai-compatible models")
		}
		if len(c.Interpret.Schema) == 0 {
			return fmt.Errorf("config: interpret.schema is required for openai-compatible models")
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
	case "typesafe", "system-one", "liquid-d1":
		if mc.Provider == "typesafe" && mc.APIKeyEnv == "" {
			return fmt.Errorf("config: model %q: api_key_env is required for typesafe", c.Interpret.Model)
		}
		if c.Interpret.Prompt != "" || len(c.Interpret.Schema) != 0 {
			return fmt.Errorf("config: interpret.prompt/schema are not supported for System One providers; use questions")
		}
		if err := model.ValidateQuestions(c.Interpret.Questions); err != nil {
			return fmt.Errorf("config: interpret.questions: %w", err)
		}
	default:
		return fmt.Errorf("config: model %q: unsupported provider %q (openai-compatible, typesafe, system-one, liquid-d1)", c.Interpret.Model, mc.Provider)
	}
	return nil
}

func (c *Config) Validate() error {
	for name, mc := range c.Models {
		if mc.Role != "" && mc.Role != "decision" && mc.Role != "agent" {
			return fmt.Errorf("config: model %q: role must be decision or agent", name)
		}
		if mc.Role == "agent" && mc.Provider != "openai-compatible" {
			return fmt.Errorf("config: agent model %q requires openai-compatible provider", name)
		}
		if mc.Role == "agent" {
			if err := staticHTTPURL(mc.BaseURL); err != nil || mc.Model == "" {
				return fmt.Errorf("config: agent model %q requires model and static HTTP base_url", name)
			}
		}
	}
	if err := c.validateInterpretation(); err != nil {
		return err
	}
	for typ, spec := range c.InterpretByType {
		if strings.TrimSpace(typ) == "" {
			return fmt.Errorf("config: interpret_by_type requires nonempty event types")
		}
		candidate := *c
		candidate.Interpret = spec
		if err := candidate.validateInterpretation(); err != nil {
			return fmt.Errorf("config: interpret_by_type %q: %w", typ, err)
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
		if t.Notify != nil {
			switch t.Notify.Via {
			case "stdout":
				if t.Notify.WebhookEnv != "" {
					return fmt.Errorf("config: %s: webhook_env requires notify.via discord", where)
				}
			case "discord":
				if !environmentName.MatchString(t.Notify.WebhookEnv) {
					return fmt.Errorf("config: %s: discord requires a literal webhook_env variable name", where)
				}
			default:
				return fmt.Errorf("config: %s: notify.via %q not supported (stdout, discord)", where, t.Notify.Via)
			}
		}
		if t.Action != nil {
			switch t.Action.Type {
			case "http":
				if t.Action.URL == "" {
					return fmt.Errorf("config: %s: action.url is required for type http", where)
				}
				if err := staticHTTPURL(t.Action.URL); err != nil {
					return fmt.Errorf("config: %s: action.url: %w", where, err)
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
			if t.Delegate.CompletionEventType != "" && !t.Delegate.EmitCompletion {
				return fmt.Errorf("config: %s: completion_event_type requires emit_completion", where)
			}
			if strings.ContainsAny(t.Delegate.CompletionEventType, "{} \t\r\n") {
				return fmt.Errorf("config: %s: completion_event_type must be a literal event type", where)
			}
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
		if a.Timeout != 0 && (a.Timeout < time.Second || a.Timeout > 5*time.Minute) {
			return fmt.Errorf("config: agent %q: timeout must be between 1s and 5m", name)
		}
		if a.Endpoint == "" {
			return fmt.Errorf("config: agent %q: endpoint is required", name)
		}
		if err := staticHTTPURL(a.Endpoint); err != nil {
			return fmt.Errorf("config: agent %q: %w", name, err)
		}
		if a.Model != "" {
			mc, ok := c.Models[a.Model]
			if !ok || mc.Role != "agent" {
				return fmt.Errorf("config: agent %q: model must reference a role: agent model", name)
			}
		}
		for _, capability := range a.Capabilities {
			if strings.TrimSpace(capability) == "" || strings.ContainsAny(capability, "{}\r\n") {
				return fmt.Errorf("config: agent %q: capabilities must be literal nonempty names", name)
			}
		}
	}
	seenSources := map[string]bool{}
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.Name == "" {
			return fmt.Errorf("config: source %d: name is required", i+1)
		}
		// Source names become Event.Source and cursor keys: uniqueness is
		// what keeps two pollers from silently sharing dedup identity.
		if seenSources[s.Name] {
			return fmt.Errorf("config: source %q: duplicate name", s.Name)
		}
		seenSources[s.Name] = true
		switch s.Type {
		case "http-poller":
			if s.URL == "" {
				return fmt.Errorf("config: source %q: url is required", s.Name)
			}
			if len(s.Units) != 0 || s.Priority != "" {
				return fmt.Errorf("config: source %q: units and priority require systemd-journal", s.Name)
			}
		case "systemd-journal":
			if s.URL != "" || s.BearerTokenEnv != "" || s.ItemPath != "" || s.CursorField != "" || s.SubjectField != "" || s.TimestampField != "" {
				return fmt.Errorf("config: source %q: HTTP fields are not supported for systemd-journal", s.Name)
			}
			if len(s.Units) == 0 || len(s.Units) > 32 {
				return fmt.Errorf("config: source %q: units must contain 1 to 32 exact system unit names", s.Name)
			}
			seen := map[string]bool{}
			for _, unit := range s.Units {
				if len(unit) > 255 || !journalUnitName.MatchString(unit) || strings.Contains(unit, "..") || seen[unit] {
					return fmt.Errorf("config: source %q: invalid or duplicate system unit name", s.Name)
				}
				seen[unit] = true
			}
			if _, ok := JournalPriority(s.Priority); !ok {
				return fmt.Errorf("config: source %q: priority must be emerg, alert, crit, err, warning, notice, info, or debug", s.Name)
			}
		default:
			return fmt.Errorf("config: source %q: type %q not supported (http-poller, systemd-journal)", s.Name, s.Type)
		}
		// The floor protects the polled target from a typo like `every: 1ms`
		// turning into a self-inflicted denial of service.
		if s.Every < time.Second {
			return fmt.Errorf("config: source %q: every must be at least 1s", s.Name)
		}
		if s.EventType == "" {
			return fmt.Errorf("config: source %q: event_type is required", s.Name)
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
	// Policy identity must be stable and serializable before executions can
	// reference it; YAML-only map keys or non-finite values would lose identity.
	if _, err := json.Marshal(c.Policy); err != nil {
		return fmt.Errorf("config: policy must be JSON-compatible: %w", err)
	}
	return validateThen(c.Policy.Default, "default")
}

// Effect destinations are fixed configuration, never templates or credentials.
func staticHTTPURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(value, "{}") {
		return fmt.Errorf("must be a static HTTP(S) URL without credentials or fragment")
	}
	return nil
}
