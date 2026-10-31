package runtime

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
)

func TestMonitorsIsolateModelsPoliciesAndDiscordDestinations(t *testing.T) {
	legacyURL, legacyCalls := modelServer(t, `{"category":"important","importance":0.95}`)
	localURL, localCalls := modelServer(t, `{"category":"important","importance":0.95}`)
	rt := newTestRuntime(t, legacyURL)
	local := model.Interpretation{Model: "local", Prompt: rt.Prompt, Schema: rt.Schema}
	rt.Models = map[string]model.Config{"local": {Provider: "openai-compatible", Model: "local-model", BaseURL: localURL}, "legacy": rt.ModelCfg}
	rt.InterpretByType = map[string]model.Interpretation{"service.warning": {Model: "legacy", Prompt: rt.Prompt, Schema: rt.Schema}}
	rt.Monitors = map[string]config.Monitor{}
	for _, name := range []string{"plex", "other"} {
		t.Setenv("WEBHOOK_"+name, "https://discord.com/api/webhooks/123/"+name)
		rt.Monitors[name] = config.Monitor{Interpret: local, Policy: policy.Policy{Default: policy.Then{Notify: &policy.Notify{Via: "discord", WebhookEnv: "WEBHOOK_" + name, Message: "{{ .event.source }}"}}}}
	}
	rt.Monitors["quiet"] = config.Monitor{Interpret: local, Policy: policy.Policy{Default: policy.Then{Ignore: true}}}
	destinations := []string{}
	rt.HTTP = &http.Client{Transport: discordTransport(func(req *http.Request) (*http.Response, error) {
		var pending int
		if err := rt.DB.QueryRow(`SELECT count(*) FROM executions WHERE status='pending'`).Scan(&pending); err != nil || pending != 1 {
			t.Fatal("monitor effect had no unique durable intent")
		}
		body, _ := io.ReadAll(req.Body)
		name := strings.TrimPrefix(req.URL.Path, "/api/webhooks/123/")
		if !strings.Contains(string(body), `"content":"`+name+`"`) {
			t.Fatal("notification crossed source boundary")
		}
		destinations = append(destinations, name)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
	})}
	for _, name := range []string{"plex", "other", "quiet", "legacy"} {
		e := testEvent(name)
		e.Source, e.Type = name, "service.warning"
		resp, err := rt.Ingest(context.Background(), e)
		if err != nil {
			t.Fatal(err)
		}
		wantModel, wantOutcome, wantPolicy := "local", "notify", rt.Monitors[name].Policy
		if name == "quiet" {
			wantOutcome = "ignore"
		}
		if name == "legacy" {
			wantModel, wantPolicy = "legacy", rt.Policy
		}
		if resp.Decision.Interpreter != wantModel || resp.Outcome != wantOutcome {
			t.Fatalf("%s: %+v", name, resp)
		}
		ins, err := rt.DB.Inspect(e.ID)
		if err != nil || len(ins.Executions) != 1 || ins.Executions[0].PolicyHash != wantPolicy.Version() {
			t.Fatal("execution did not retain the selected monitor policy")
		}
	}
	if strings.Join(destinations, ",") != "plex,other" || localCalls.Load() != 3 || legacyCalls.Load() != 1 {
		t.Fatalf("destinations=%v local=%d legacy=%d", destinations, localCalls.Load(), legacyCalls.Load())
	}
	// The durable global stop must still cover source-local live rules.
	rt.EffectsBlocked = func() bool { return true }
	e := testEvent("blocked")
	e.Source = "plex"
	if _, err := rt.Ingest(context.Background(), e); err != nil || len(destinations) != 2 {
		t.Fatal("source-local policy bypassed the effect stop")
	}
}
