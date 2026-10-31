package main

import (
	"strings"
	"testing"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/policy"
)

func TestManagementLiveNotificationsRequireBoundedConsent(t *testing.T) {
	for _, kind := range []string{"confirmed", "default", "stdout", "no consent", "safety stop", "missing credential", "http action", "emit action", "agent"} {
		t.Run(kind, func(t *testing.T) {
			m := testManager(t)
			t.Setenv("MRMR_MONITOR_DISCORD", "")
			c := monitorCandidate(t, "http://127.0.0.1:1/v1")
			c.Sources[0].Scope = "user"
			local := c.Monitors["service-monitor"]
			local.Policy.Rules[0].Then.Shadow = false
			if kind == "stdout" {
				local.Policy.Rules[0].Then.Notify.Via = "stdout"
				local.Policy.Rules[0].Then.Notify.WebhookEnv = ""
			}
			if kind == "http action" {
				local.Policy.Rules[0].Then = policy.Then{Action: &policy.Action{Type: "http", URL: "http://127.0.0.1:1/never"}}
			}
			if kind == "emit action" {
				local.Policy.Rules[0].Then = policy.Then{Action: &policy.Action{Type: "emit", EventType: "never"}}
			}
			if kind == "agent" {
				c.Agents = map[string]config.Agent{"worker": {Endpoint: "http://127.0.0.1:1/never"}}
				local.Policy.Rules[0].Then = policy.Then{Delegate: &policy.Delegate{Agent: "worker", Prompt: "Never dispatch this fixture"}}
			}
			if kind == "default" {
				local.Policy.Default = local.Policy.Rules[0].Then
				local.Policy.Rules = nil
			}
			c.Monitors["service-monitor"] = local
			if kind != "missing credential" {
				if _, err := m.discord.Save("MRMR_MONITOR_DISCORD", controlWebhook, ""); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "safety stop" {
				m.blocked.Store(true)
			}
			req := saveControlDraft(t, m, monitorYAML(t, c))
			req.AllowLiveNotify = kind != "no consent"
			before := m.activeRevision()
			w := controlRequest(m, "POST", "/manage/api/activate", req)
			allowed := kind == "confirmed" || kind == "default" || kind == "stdout"
			if (w.Code == 200) != allowed {
				t.Fatalf("activation %d: %s", w.Code, w.Body.String())
			}
			if !allowed && m.activeRevision() != before {
				t.Fatal("rejected permission changed active state")
			}
			var count int
			if err := m.db.QueryRow("SELECT count(*) FROM executions").Scan(&count); err != nil || count != 0 {
				t.Fatal("activation dispatched an effect")
			}
		})
	}
}

func TestJournalScopeIsPartOfSourceIdentity(t *testing.T) {
	m := testManager(t)
	c := monitorCandidate(t, "http://127.0.0.1:1/v1")
	apply := func() int {
		req := saveControlDraft(t, m, monitorYAML(t, c))
		return controlRequest(m, "POST", "/manage/api/activate", req).Code
	}
	if apply() != 200 {
		t.Fatal("initial preview activation failed")
	}
	c.Sources[0].Scope = "system"
	if apply() != 200 {
		t.Fatal("explicit system scope changed legacy identity")
	}
	before := m.activeRevision()
	c.Sources[0].Scope = "user"
	req := saveControlDraft(t, m, monitorYAML(t, c))
	w := controlRequest(m, "POST", "/manage/api/activate", req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "identity") || m.activeRevision() != before {
		t.Fatal("scope changed under an existing source name")
	}
}
