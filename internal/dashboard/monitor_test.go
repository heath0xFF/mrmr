package dashboard

import (
	"strings"
	"testing"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/policy"
)

func TestMonitorProjectionRedactsLocalCredentials(t *testing.T) {
	_, rt := testDashboard(t)
	const secret = "source-local-synthetic-credential"
	t.Setenv("MONITOR_DISCORD", secret)
	if _, err := rt.DB.Exec(`UPDATE events SET data=? WHERE id='evt_test'`, `{"message":"`+secret+`"}`); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Monitors: map[string]config.Monitor{
		"journal": {
			Interpret: config.Interpret{Model: "local-decision"},
			Policy:    policy.Policy{Default: policy.Then{Notify: &policy.Notify{Via: "discord", WebhookEnv: "MONITOR_DISCORD"}, Shadow: true}},
		},
	}}
	h := New(rt, cfg)
	for _, path := range []string{"/dashboard/api/overview", "/dashboard/api/events", "/dashboard/api/event?id=evt_test"} {
		w := request(h, "GET", path)
		if w.Code != 200 || strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), "MONITOR_DISCORD") {
			t.Fatalf("monitor credential was disclosed by %s", path)
		}
		if path == "/dashboard/api/overview" && (!strings.Contains(w.Body.String(), "monitor:journal/default") || !strings.Contains(w.Body.String(), `"monitor_interpreters":{"journal":"local-decision"}`)) {
			t.Fatal("read-only dashboard omitted source-local authority")
		}
	}
}
