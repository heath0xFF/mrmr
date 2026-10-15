package config

import (
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
)

func ambientConfig() *Config {
	c := validConfig()
	c.Models["judge"] = model.Config{Role: "decision", Provider: "liquid-d1", BaseURL: "http://localhost:8091/v1", Model: "pinned-d1"}
	c.Models["worker"] = model.Config{Role: "agent", Provider: "openai-compatible", BaseURL: "http://localhost:8081/v1", Model: "small-worker"}
	c.Interpret = Interpret{Model: "judge", Questions: model.Questions{"category": {Type: "choice", Instructions: "Classify", Criteria: map[string]string{"routine": "Routine", "investigate": "Investigate"}}}}
	c.InterpretByType = map[string]Interpret{"agent.completed": c.Interpret}
	c.Agents["triage"] = Agent{Endpoint: "http://localhost:9000/tasks", Model: "worker", ReadOnly: true, Capabilities: []string{"journal.read"}}
	c.Policy.Rules[0].Then = policy.Then{Delegate: &policy.Delegate{Agent: "triage", EmitCompletion: true}}
	return c
}

func TestAmbientConfigBoundaries(t *testing.T) {
	if err := ambientConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"unbounded agent timeout": func(c *Config) { a := c.Agents["triage"]; a.Timeout = 6 * time.Minute; c.Agents["triage"] = a },
		"negative agent timeout":  func(c *Config) { a := c.Agents["triage"]; a.Timeout = -time.Second; c.Agents["triage"] = a },
		"agent used as judge":     func(c *Config) { c.Interpret.Model = "worker" },
		"unknown role":            func(c *Config) { m := c.Models["judge"]; m.Role = "tool"; c.Models["judge"] = m },
		"judge used as worker":    func(c *Config) { a := c.Agents["triage"]; a.Model = "judge"; c.Agents["triage"] = a },
		"missing worker":          func(c *Config) { delete(c.Models, "worker") },
		"templated agent URL": func(c *Config) {
			a := c.Agents["triage"]
			a.Endpoint = "http://localhost/{{ .result.target }}"
			c.Agents["triage"] = a
		},
		"templated action URL": func(c *Config) {
			c.Policy.Default = policy.Then{Action: &policy.Action{Type: "http", URL: "{{ .result.url }}"}}
		},
		"credential URL": func(c *Config) {
			a := c.Agents["triage"]
			a.Endpoint = "http://user:password@localhost/tasks"
			c.Agents["triage"] = a
		},
		"completion without opt-in": func(c *Config) {
			d := c.Policy.Rules[0].Then.Delegate
			d.EmitCompletion = false
			d.CompletionEventType = "agent.completed"
		},
		"templated event type": func(c *Config) { c.Policy.Rules[0].Then.Delegate.CompletionEventType = "{{ .result.type }}" },
		"invalid override":     func(c *Config) { c.InterpretByType["agent.completed"] = Interpret{Model: "missing"} },
		"mixed contracts": func(c *Config) {
			m := c.Models["judge"]
			m.Provider = "openai-compatible"
			c.Models["judge"] = m
			c.Interpret.Prompt = "mixed"
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := ambientConfig()
			mutate(c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	c := ambientConfig()
	m := c.Models["judge"]
	m.Provider = "openai-compatible"
	c.Models["judge"] = m
	if err := c.Validate(); err != nil {
		t.Fatalf("portable question path: %v", err)
	}
}

func TestHomelabAmbientRecipe(t *testing.T) {
	cfg, err := Load("../../recipes/homelab-ambient/mrmr.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range cfg.Policy.Rules {
		if rule.Then.Outcome() != "ignore" && !rule.Then.Shadow {
			t.Fatal("recipe enables an effect")
		}
	}
	if cfg.InterpreterFor("agent.investigation.completed").Questions["category"].Type != "choice" {
		t.Fatal("missing follow-up judgment")
	}
	if len(cfg.InterpretByType) != 1 || !cfg.Policy.Default.Ignore {
		t.Fatal("recipe must be bounded and fail toward ignore")
	}
}
