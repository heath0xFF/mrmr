// Package dashboard is a local, read-only view of the running daemon. It has
// no ingestion, model-call, label-write, or service-control capabilities.
package dashboard

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/policy"
	"github.com/heath0xff/mrmr/internal/runtime"
)

//go:embed index.html dashboard.css dashboard.js
var assets embed.FS

type sourceView struct {
	Name      string             `json:"name"`
	Type      string             `json:"type"`
	EventType string             `json:"event_type"`
	EveryMs   int64              `json:"every_ms"`
	Units     []string           `json:"units"`
	Priority  string             `json:"priority"`
	Health    runtime.PollHealth `json:"health"`
}

type policyView struct {
	Rule       string         `json:"rule"`
	Conditions map[string]any `json:"conditions,omitempty"`
	Outcome    string         `json:"outcome"`
	Shadow     bool           `json:"shadow"`
}

// New takes immutable startup configuration but exposes only an allowlisted
// projection. Endpoints, credentials, prompts, and action arguments stay out.
func New(rt *runtime.Runtime, cfg *config.Config) http.Handler {
	started := time.Now().UTC()
	hostname, _ := os.Hostname()
	secrets := []string{}
	addSecret := func(name string) {
		if name != "" {
			if value := os.Getenv(name); value != "" {
				secrets = append(secrets, value)
			}
		}
	}
	for _, m := range cfg.Models {
		addSecret(m.APIKeyEnv)
	}
	for _, s := range cfg.Sources {
		addSecret(s.BearerTokenEnv)
	}
	rules := []policyView{}
	addRule := func(name string, conditions map[string]any, then policy.Then) {
		rules = append(rules, policyView{name, conditions, then.Outcome(), then.Shadow})
		if then.Notify != nil {
			addSecret(then.Notify.WebhookEnv)
		}
	}
	for i, r := range cfg.Policy.Rules {
		addRule("rule:"+strconv.Itoa(i+1), r.If, r.Then)
	}
	addRule("default", nil, cfg.Policy.Default)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		// The socket peer must be local even behind a proxy. Only the explicitly
		// configured hostname joins the local Host allowlist; forwarded headers
		// cannot grant access or turn an arbitrary hostname into a trusted one.
		if !localRequest(r, cfg.Server.DashboardHost) {
			http.Error(w, "Dashboard requires local access or the configured private proxy.", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Read-only endpoint.", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		send := func(value any, err error) {
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					http.Error(w, "Event not found.", http.StatusNotFound)
				} else {
					http.Error(w, "Cannot read runtime data. Retry shortly.", http.StatusServiceUnavailable)
				}
				return
			}
			// Decode with UseNumber so the redaction walk cannot round event IDs.
			b, err := json.Marshal(value)
			if err != nil {
				http.Error(w, "Cannot encode runtime data.", 500)
				return
			}
			if len(b) > 4<<20 {
				http.Error(w, "Record is too large for the dashboard. Use mrmr inspect locally.", http.StatusRequestEntityTooLarge)
				return
			}
			var doc any
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.UseNumber()
			if err := dec.Decode(&doc); err != nil {
				http.Error(w, "Cannot encode runtime data.", 500)
				return
			}
			doc = redact(doc, secrets)
			// Shorten summaries only AFTER redaction. A SQL substring can cut a
			// credential in half and defeat exact-value redaction at this boundary.
			object := doc.(map[string]any)
			if r.URL.Path == "/dashboard/api/events" {
				shortenMessages(object["events"])
			} else if r.URL.Path == "/dashboard/api/overview" {
				shortenMessages(object["summary"].(map[string]any)["repeats"])
			}
			if r.URL.Path == "/dashboard/api/event" {
				// JavaScript numbers cannot represent every stored JSON identity.
				// Supply the redacted Event as display text before browser parsing.
				formatted, err := json.MarshalIndent(object["event"], "", "  ")
				if err != nil {
					http.Error(w, "Cannot encode event text.", 500)
					return
				}
				object["event_json"] = string(formatted)
			}
			b, err = json.Marshal(doc)
			if err != nil || len(b) > 4<<20 {
				http.Error(w, "Record is too large for the dashboard. Use mrmr inspect locally.", http.StatusRequestEntityTooLarge)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				w.Write(b)
			}
		}
		switch r.URL.Path {
		case "/dashboard/api/overview":
			summary, err := rt.DB.DashboardSummary(ctx)
			sources := []sourceView{}
			for _, s := range cfg.Sources {
				sources = append(sources, sourceView{s.Name, s.Type, s.EventType, s.Every.Milliseconds(), s.Units, s.Priority, rt.PollHealth(s.Name)})
			}
			send(map[string]any{"at": time.Now().UTC(), "started_at": started, "hostname": hostname, "summary": summary, "sources": sources, "policy": rules, "filter_count": len(cfg.Filter)}, err)
		case "/dashboard/api/events":
			q := r.URL.Query()
			offset := 0
			var err error
			if q.Has("offset") {
				offset, err = strconv.Atoi(q.Get("offset"))
			}
			view := q.Get("view")
			if view == "" {
				view = "all"
			}
			if err != nil || offset < 0 || offset > 100000 || len(q.Get("q")) > 200 || !validView(view) {
				http.Error(w, "Invalid event query.", 400)
				return
			}
			page, err := rt.DB.DashboardEvents(ctx, q.Get("q"), view, offset)
			send(page, err)
		case "/dashboard/api/event":
			id := r.URL.Query().Get("id")
			if id == "" || len(id) > 200 {
				http.Error(w, "Invalid event ID.", 400)
				return
			}
			ins, err := rt.DB.InspectContext(ctx, id)
			send(ins, err)
		case "/dashboard", "/":
			http.Redirect(w, r, "/dashboard/", http.StatusTemporaryRedirect)
		default:
			name, kind := "", ""
			switch r.URL.Path {
			case "/dashboard/":
				name, kind = "index.html", "text/html; charset=utf-8"
			case "/dashboard/dashboard.css":
				name, kind = "dashboard.css", "text/css; charset=utf-8"
			case "/dashboard/dashboard.js":
				name, kind = "dashboard.js", "text/javascript; charset=utf-8"
			default:
				http.NotFound(w, r)
				return
			}
			b, err := assets.ReadFile(name)
			if err != nil {
				http.Error(w, "Dashboard asset unavailable.", 500)
				return
			}
			w.Header().Set("Content-Type", kind)
			if r.Method == http.MethodGet {
				w.Write(b)
			}
		}
	})
}

func validView(view string) bool {
	switch view {
	case "all", "unlabeled", "shadow", "ignore", "errors":
		return true
	}
	return false
}

func localRequest(r *http.Request, proxyHost string) bool {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(peer).IsLoopback() {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != "localhost" && host != "127.0.0.1" && host != "::1" && host != "[::1]" && (proxyHost == "" || host != proxyHost) {
		return false
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host != r.Host {
			return false
		}
	}
	return true
}

func shortenMessages(value any) {
	for _, item := range value.([]any) {
		row := item.(map[string]any)
		message := []rune(row["message"].(string))
		if len(message) > 240 {
			row["message"] = string(message[:240]) + "…"
		}
	}
}

var privateURL = regexp.MustCompile(`https?://[^\s<>"']+`)
var privateField = regexp.MustCompile(`(?i)(bearer\s+|(?:token|password|secret|api[_-]?key)\s*[=:]\s*)[^\s,;"']+`)

// Redaction is defense in depth, not a promise that arbitrary event text is
// public. Keep the view private; render every external string as text, not HTML.
func redact(v any, secrets []string) any {
	switch v := v.(type) {
	case string:
		for _, secret := range secrets {
			v = strings.ReplaceAll(v, secret, "[redacted]")
		}
		v = privateURL.ReplaceAllString(v, "[URL redacted]")
		return privateField.ReplaceAllString(v, "${1}[redacted]")
	case []any:
		for i, item := range v {
			v[i] = redact(item, secrets)
		}
		return v
	case map[string]any:
		clean := make(map[string]any, len(v))
		for k, item := range v {
			switch strings.ToLower(k) {
			case "authorization", "password", "secret", "token", "api_key", "access_token", "refresh_token", "webhook_url":
				item = "[redacted]"
			}
			clean[redact(k, secrets).(string)] = redact(item, secrets)
		}
		return clean
	default:
		return v
	}
}
