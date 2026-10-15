package config

import (
	"testing"

	"github.com/heath0xff/mrmr/internal/policy"
)

func TestDiscordSecretReference(t *testing.T) {
	for _, tc := range []struct {
		via, ref string
		valid    bool
	}{
		{"discord", "MRMR_DISCORD_WEBHOOK_URL", true},
		{"discord", "", false},
		{"discord", "{{ .result.target }}", false},
		{"discord", "https://discord.com/api/webhooks/123/synthetic", false},
		{"stdout", "MRMR_DISCORD_WEBHOOK_URL", false},
	} {
		c := validConfig()
		c.Policy.Default = policy.Then{Notify: &policy.Notify{Via: tc.via, WebhookEnv: tc.ref}}
		if err := c.Validate(); (err == nil) != tc.valid {
			t.Fatalf("via=%s ref=%s error=%v", tc.via, tc.ref, err)
		}
	}
}
