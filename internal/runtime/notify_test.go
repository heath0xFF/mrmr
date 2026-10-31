package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heath0xff/mrmr/internal/notify"
	"github.com/heath0xff/mrmr/internal/policy"
)

type discordTransport func(*http.Request) (*http.Response, error)

func (f discordTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDiscordUsesDurableIntentAndShadow(t *testing.T) {
	const secret = "https://discord.com/api/webhooks/123/synthetic-private-token"
	for _, mode := range []string{"environment", "stored"} {
		for _, status := range []string{"ok", "shadow", "blocked", "error"} {
			t.Run(mode+"/"+status, func(t *testing.T) {
				t.Setenv("TEST_DISCORD_WEBHOOK", secret)
				url, _ := modelServer(t, `{"category":"important","importance":0.95}`)
				rt := newTestRuntime(t, url)
				rt.EffectsBlocked = func() bool { return status == "blocked" }
				shadow := status == "shadow" || status == "blocked"
				if mode == "stored" {
					t.Setenv("TEST_DISCORD_WEBHOOK", secret+"old")
					rt.DiscordCredentials = &notify.DiscordCredentials{Dir: filepath.Join(t.TempDir(), "discord")}
					if _, err := rt.DiscordCredentials.Save("TEST_DISCORD_WEBHOOK", secret, ""); err != nil {
						t.Fatal(err)
					}
					// Shadow work must not even read a damaged credential store.
					if shadow {
						if err := os.WriteFile(filepath.Join(rt.DiscordCredentials.Dir, "credentials.json"), []byte("invalid"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				rt.Policy = policy.Policy{Default: policy.Then{Notify: &policy.Notify{Via: "discord", WebhookEnv: "TEST_DISCORD_WEBHOOK", Message: "{{ .event.subject }}: {{ .result.category }}"}, Shadow: status == "shadow"}}
				calls := 0
				rt.HTTP = &http.Client{Transport: discordTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.String() != secret+"?wait=true" {
						t.Fatal("wrong credential destination")
					}
					var pending string
					if err := rt.DB.QueryRow("SELECT status FROM executions").Scan(&pending); err != nil || pending != "pending" {
						t.Fatalf("effect preceded durable intent: %q %v", pending, err)
					}
					body, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(body), "mrmr: important") {
						t.Fatal("message did not use selected event/judgment")
					}
					if status == "error" {
						return nil, errors.New(secret)
					}
					return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: r}, nil
				})}
				response, err := rt.Ingest(context.Background(), testEvent("discord-"+status))
				if err != nil || response.Outcome != "notify" {
					t.Fatalf("response=%+v error=%v", response, err)
				}
				if (calls == 0) != shadow {
					t.Fatal("wrong shadow behavior")
				}
				var storedStatus, storedError string
				if err := rt.DB.QueryRow("SELECT status,error FROM executions").Scan(&storedStatus, &storedError); err != nil {
					t.Fatal(err)
				}
				if shadow {
					status = "shadow"
				}
				if storedStatus != status {
					t.Fatalf("stored=%s want=%s", storedStatus, status)
				}
				encoded, _ := json.Marshal(response)
				if strings.Contains(string(encoded)+storedError, "synthetic-private-token") {
					t.Fatal("credential reached trace or storage")
				}
			})
		}
	}
}
