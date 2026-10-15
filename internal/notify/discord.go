// Package notify contains bounded notification effects, never routing policy.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"
)

var webhookPath = regexp.MustCompile(`^/api(?:/v10)?/webhooks/[0-9]+/[A-Za-z0-9_-]+$`)

// Discord sends one message to an environment-resolved webhook. Neither the
// credential-bearing URL nor Discord's response body may reach errors or traces.
func Discord(ctx context.Context, client *http.Client, webhook, message string) error {
	u, err := url.Parse(webhook)
	if err != nil || u.Scheme != "https" || u.Host != "discord.com" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || !webhookPath.MatchString(u.Path) {
		return fmt.Errorf("discord: missing or invalid webhook secret")
	}
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("discord: notification message is empty")
	}
	// Model text cannot ping people or cause automatic link previews. wait=true
	// asks Discord to confirm creation rather than silently accepting a lost send.
	u.RawQuery = "wait=true"
	body, err := json.Marshal(map[string]any{
		"content": discordContent(message), "flags": 4,
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	if err != nil {
		return fmt.Errorf("discord: encode notification")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("discord: build notification request")
	}
	req.Header.Set("Content-Type", "application/json")
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := bounded.Do(req)
	if err != nil {
		// net/http errors include the full URL, which is a credential here.
		return fmt.Errorf("discord: notification request failed")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("discord: notification returned status %d", resp.StatusCode)
	}
	return nil
}

func discordContent(text string) string {
	// Count UTF-16 units conservatively and leave room for an ellipsis without
	// splitting a rune. Exact-limit content remains unchanged.
	runes := []rune(text)
	units, cut := 0, 0
	for i, r := range runes {
		units += utf16.RuneLen(r)
		if units <= 1999 {
			cut = i + 1
		}
		if units > 2000 {
			return string(runes[:cut]) + "…"
		}
	}
	return text
}
