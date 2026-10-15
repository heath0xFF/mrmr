package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const syntheticWebhook = "https://discord.com/api/webhooks/123/synthetic-not-a-secret"

func TestDiscordBoundary(t *testing.T) {
	for _, status := range []int{200, 204, 302, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "POST" || r.URL.String() != syntheticWebhook+"?wait=true" {
					t.Fatal("wrong fixed destination")
				}
				if _, ok := r.Context().Deadline(); !ok {
					t.Fatal("unbounded request")
				}
				var body struct {
					Content  string `json:"content"`
					Flags    int    `json:"flags"`
					Mentions struct {
						Parse []string `json:"parse"`
					} `json:"allowed_mentions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.Content != "@everyone synthetic" || body.Flags != 4 || body.Mentions.Parse == nil || len(body.Mentions.Parse) != 0 {
					t.Fatalf("unsafe message: %+v", body)
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": []string{"https://unapproved.invalid"}}, Body: io.NopCloser(strings.NewReader(syntheticWebhook)), Request: r}, nil
			})}
			err := Discord(context.Background(), client, syntheticWebhook, "@everyone synthetic")
			if (err != nil) != (status >= 300) {
				t.Fatalf("status=%d error=%v", status, err)
			}
			if calls != 1 {
				t.Fatal("redirected or retried")
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-not-a-secret") {
				t.Fatal("secret leaked")
			}
		})
	}
}

func TestDiscordRejectsDestinationsAndRedactsTransportErrors(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New(syntheticWebhook)
	})}
	for _, url := range []string{"", "http://discord.com/api/webhooks/123/token", syntheticWebhook + "?thread_id=1", syntheticWebhook + "#fragment", "https://discord.com@evil.invalid/api/webhooks/123/token", "https://user:password@discord.com/api/webhooks/123/token", "https://discord.com:443/api/webhooks/123/token", "https://discord.com/api/webhooks/123/%74oken"} {
		if err := Discord(context.Background(), client, url, "test"); err == nil {
			t.Fatalf("accepted invalid destination")
		}
	}
	if err := Discord(context.Background(), client, syntheticWebhook, " \n "); err == nil {
		t.Fatal("accepted empty message")
	}
	if calls != 0 {
		t.Fatal("invalid input reached transport")
	}
	err := Discord(context.Background(), client, syntheticWebhook, "test")
	if err == nil || strings.Contains(err.Error(), "synthetic-not-a-secret") || strings.Contains(err.Error(), "https") {
		t.Fatalf("unsafe transport error: %v", err)
	}
}

func TestDiscordTextLimit(t *testing.T) {
	for _, text := range []string{strings.Repeat("a", 2000), strings.Repeat("a", 2001), strings.Repeat("🙂", 1000), strings.Repeat("🙂", 1001)} {
		got := discordContent(text)
		if !utf8.ValidString(got) || len(utf16.Encode([]rune(got))) > 2000 {
			t.Fatal("broken Unicode or exceeded message limit")
		}
		if len(utf16.Encode([]rune(text))) <= 2000 && got != text {
			t.Fatal("changed exact-limit content")
		}
		if len(utf16.Encode([]rune(text))) > 2000 && !strings.HasSuffix(got, "…") {
			t.Fatal("truncation was not marked")
		}
	}
}
