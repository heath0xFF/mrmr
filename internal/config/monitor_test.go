package config

import (
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/policy"
)

func monitorConfig() *Config {
	c := validConfig()
	c.Sources = []Source{{Name: "plex", Type: "systemd-journal", Units: []string{"plex.service"}, Priority: "warning", Every: time.Second, EventType: "service.warning", Paused: true}}
	local := c.Interpret
	local.Prompt = "Source-local contract"
	c.Monitors = map[string]Monitor{"plex": {Interpret: local, Policy: policy.Policy{Default: policy.Then{Ignore: true}}}}
	return c
}

func TestMonitorContractRoundTripAndPrecedence(t *testing.T) {
	c := monitorConfig()
	override := c.Interpret
	override.Prompt = "Type override"
	c.InterpretByType = map[string]Interpret{"service.warning": override}
	b, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ source, typ, prompt string }{
		{"plex", "service.warning", "Source-local contract"},
		{"plex", "other.type", "Source-local contract"},
		{"other", "service.warning", "Type override"},
		{"other", "other.type", c.Interpret.Prompt},
	} {
		e := event.Event{Source: tc.source, Type: tc.typ}
		if got := parsed.InterpreterForEvent(e); got.Prompt != tc.prompt {
			t.Fatalf("%+v selected %q", tc, got.Prompt)
		}
		want := parsed.Policy
		if tc.source == "plex" {
			want = parsed.Monitors["plex"].Policy
		}
		if got := parsed.PolicyForEvent(e); !reflect.DeepEqual(got, want) {
			t.Fatalf("%+v selected wrong policy", tc)
		}
	}
}

func TestMonitorValidationIncludesPausedSources(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"unknown source":   func(c *Config) { c.Sources = nil },
		"unknown model":    func(c *Config) { m := c.Monitors["plex"]; m.Interpret.Model = "missing"; c.Monitors["plex"] = m },
		"invalid contract": func(c *Config) { m := c.Monitors["plex"]; m.Interpret.Schema = nil; c.Monitors["plex"] = m },
		"missing default":  func(c *Config) { m := c.Monitors["plex"]; m.Policy.Default = policy.Then{}; c.Monitors["plex"] = m },
		"invalid destination": func(c *Config) {
			m := c.Monitors["plex"]
			m.Policy.Default = policy.Then{Notify: &policy.Notify{Via: "discord", WebhookEnv: "https://discord.com/not-a-reference"}}
			c.Monitors["plex"] = m
		},
		"empty condition": func(c *Config) {
			m := c.Monitors["plex"]
			m.Policy.Rules = []policy.Rule{{Then: policy.Then{Ignore: true}}}
			c.Monitors["plex"] = m
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := monitorConfig()
			mutate(c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid monitor was accepted")
			}
		})
	}
}
