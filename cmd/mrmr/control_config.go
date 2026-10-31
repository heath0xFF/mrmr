package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/notify"
	"github.com/heath0xff/mrmr/internal/policy"
	"github.com/heath0xff/mrmr/internal/runtime"
	"github.com/heath0xff/mrmr/internal/source"
	"gopkg.in/yaml.v3"
)

type sourceJob func(context.Context, <-chan struct{}, *runtime.Runtime)
type configDraft struct {
	YAML     string `json:"yaml"`
	Revision string `json:"revision"`
	Base     string `json:"base"`
}
type configRequest struct {
	YAML            string `json:"yaml"`
	Expected        string `json:"expected"`
	Base            string `json:"base"`
	Confirm         bool   `json:"confirm"`
	AllowLiveNotify bool   `json:"allow_live_notify"`
}

func (m *manager) prepare(ctx context.Context, c *config.Config) ([]sourceJob, error) {
	var jobs []sourceJob
	for _, s := range c.Sources {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("Source readiness check was canceled.")
		}
		if s.Paused {
			continue
		}
		switch s.Type {
		case "http-poller":
			p, err := source.NewHTTPPoller(s)
			if err != nil {
				return nil, fmt.Errorf("HTTP source readiness failed; check URL mapping and credential references.")
			}
			jobs = append(jobs, p.RunUntil)
		case "systemd-journal":
			p, err := source.NewJournal(ctx, s, m.db)
			if err != nil {
				return nil, fmt.Errorf("Journal readiness failed; check platform, access, and saved cursor.")
			}
			jobs = append(jobs, p.RunUntil)
		default:
			return nil, fmt.Errorf("Unsupported source type.")
		}
	}
	return jobs, nil
}
func (m *manager) startSources(jobs []sourceJob, rt *runtime.Runtime) {
	m.stopSources = make(chan struct{})
	stop := m.stopSources
	for _, job := range jobs {
		m.sources.Add(1)
		go func() { defer m.sources.Done(); job(m.ctx, stop, rt) }()
	}
}
func (m *manager) activeRevision() string {
	if s := m.current.Load(); s != nil {
		return s.revision
	}
	return revision("")
}

// Management refuses literal credentials instead of trying to round-trip a
// redacted file. Endpoint locations are configuration; secret values belong
// in environment variables or the private Discord credential store.
var inlineSecret = regexp.MustCompile(`(?i)(bearer\s+\S+|(?:token|password|secret|api[_-]?key)\s*[=:]\s*[^\s,;"']+)`)

var discordSecretURL = regexp.MustCompile(`(?i)https://discord\.com/api(?:/v[0-9]+)?/webhooks/`)

func managementSafe(b []byte) error {
	// Scan comments too; decoded values are checked below for YAML escapes.
	if discordSecretURL.Match(b) {
		return fmt.Errorf("Use a Discord connection reference, not a webhook URL in configuration.")
	}
	if len(b) > 256<<10 {
		return fmt.Errorf("Configuration exceeds 256 KiB.")
	}
	var doc any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("Invalid YAML; check indentation, duplicate keys, and field types.")
	}
	var walk func(any) error
	walk = func(v any) error {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if err := walk(k); err != nil {
					return err
				}
				l := strings.ToLower(k)
				if l == "token" || l == "password" || l == "secret" || l == "api_key" || l == "authorization" || l == "webhook_url" {
					return fmt.Errorf("Inline credentials are not allowed; use environment references.")
				}
				if err := walk(x); err != nil {
					return err
				}
			}
		case []any:
			for _, x := range v {
				if err := walk(x); err != nil {
					return err
				}
			}
		case string:
			if discordSecretURL.MatchString(v) {
				return fmt.Errorf("Use a Discord connection reference, not a webhook URL in configuration.")
			}
			if inlineSecret.MatchString(v) {
				return fmt.Errorf("Possible inline credential detected; use environment references.")
			}
			if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
				u, e := url.Parse(v)
				if e == nil && (u.User != nil || strings.Contains(u.Path, "/api/webhooks/")) {
					return fmt.Errorf("Credential-bearing URLs are not allowed in managed configuration.")
				}
			}
		}
		return nil
	}
	if err := walk(doc); err != nil {
		return err
	}
	// Detect values of configured secret references, including accidental copies
	// inside prompts or comments. Never echo the matched value in an error.
	c, err := config.Parse(b)
	if err == nil {
		names := []string{}
		for _, v := range c.Models {
			names = append(names, v.APIKeyEnv)
		}
		for _, v := range c.Sources {
			names = append(names, v.BearerTokenEnv)
		}
		for _, p := range c.Policies() {
			for _, v := range p.Rules {
				if v.Then.Notify != nil {
					names = append(names, v.Then.Notify.WebhookEnv)
				}
			}
			if p.Default.Notify != nil {
				names = append(names, p.Default.Notify.WebhookEnv)
			}
		}
		for _, n := range names {
			if n != "" {
				if value := os.Getenv(n); value != "" && strings.Contains(string(b), value) {
					return fmt.Errorf("Configuration contains a credential value; only its environment reference may be stored.")
				}
			}
		}
	}
	return nil
}
func managedParse(text string) (*config.Config, error) {
	if err := managementSafe([]byte(text)); err != nil {
		return nil, err
	}
	c, err := config.Parse([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("Configuration is invalid. Check required fields, model roles, source mappings, and policy rules against the supported schema.")
	}
	return c, nil
}

func (m *manager) route(w http.ResponseWriter, r *http.Request) error {
	switch {
	case r.Method == "GET" && r.URL.Path == "/manage/api/state":
		var draft configDraft
		err := m.db.QueryRow(`SELECT yaml,revision,base FROM control_draft WHERE singleton=1`).Scan(&draft.YAML, &draft.Revision, &draft.Base)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("Cannot read draft.")
		}
		active := ""
		if s := m.current.Load(); s != nil {
			active = s.yaml
		}
		var history []map[string]any
		rows, err := m.db.Query(`SELECT id,created_at,status,base FROM control_revisions ORDER BY created_at DESC LIMIT 30`)
		if err != nil {
			return fmt.Errorf("Cannot read configuration history.")
		}
		for rows.Next() {
			var id, at, status, base string
			if err := rows.Scan(&id, &at, &status, &base); err != nil {
				rows.Close()
				return fmt.Errorf("Cannot read revision.")
			}
			history = append(history, map[string]any{"id": id, "at": at, "status": status, "base": base})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("Cannot read revisions.")
		}
		var doc any
		if active != "" {
			yaml.Unmarshal([]byte(active), &doc)
		}
		connections := []map[string]string{}
		discordRefs := []string{"MRMR_DISCORD_WEBHOOK_URL"}
		credential := func(name string) string {
			if name == "" {
				return "not required"
			}
			if os.Getenv(name) == "" {
				return "missing"
			}
			return "configured (not tested)"
		}
		if snap := m.current.Load(); snap != nil {
			for name, model := range snap.cfg.Models {
				connections = append(connections, map[string]string{"name": name, "kind": "model", "credential": credential(model.APIKeyEnv)})
			}
			// Presence is not delivery readiness. Never return webhook values.
			seen := map[string]bool{}
			for _, p := range snap.cfg.Policies() {
				outcomes := []policy.Then{p.Default}
				for _, rule := range p.Rules {
					outcomes = append(outcomes, rule.Then)
				}
				for _, outcome := range outcomes {
					if n := outcome.Notify; n != nil && n.Via == "discord" && !seen[n.WebhookEnv] {
						seen[n.WebhookEnv] = true
						discordRefs = append(discordRefs, n.WebhookEnv)
					}
				}
			}
			for _, source := range snap.cfg.Sources {
				if source.Type == "http-poller" {
					connections = append(connections, map[string]string{"name": source.Name, "kind": "HTTP source", "credential": credential(source.BearerTokenEnv)})
				}
			}
		}
		discord, err := m.discord.Connections(discordRefs)
		if err != nil {
			return err
		}
		for _, c := range discord {
			status := "missing"
			if c.Configured {
				status = "configured (not tested)"
			}
			// An audit row, because this is a manual check rather than an Execution:
			// it proves the webhook works, not that any event produced a message.
			var checked sql.NullString
			if err := m.db.QueryRow(`SELECT MAX(at) FROM control_audit WHERE operation='test-delivery' AND target=? AND result='delivered'`, c.Name).Scan(&checked); err != nil {
				return err
			}
			if checked.Valid {
				status = "configured, test message delivered"
			}
			connections = append(connections, map[string]string{"name": c.Name, "kind": "Discord", "credential": status, "revision": c.Revision, "delivery_checked_at": checked.String})
		}
		return controlJSON(w, map[string]any{"connections": connections, "active": active, "revision": m.activeRevision(), "draft": draft, "history": history, "document": doc, "setup": m.current.Load() == nil, "shadow_override": m.blocked.Load(), "bootstrap": map[string]any{"Server": m.bootstrap.Server, "DB": m.bootstrap.DB}})
	case r.Method == "POST" && r.URL.Path == "/manage/api/discord":
		return m.saveDiscord(w, r)
	case r.Method == "POST" && r.URL.Path == "/manage/api/test-delivery":
		return m.testDelivery(w, r)
	case r.Method == "GET" && r.URL.Path == "/manage/api/revision":
		var text string
		if err := m.db.QueryRow(`SELECT yaml FROM control_revisions WHERE id=?`, r.URL.Query().Get("id")).Scan(&text); err != nil {
			return fmt.Errorf("Revision not found.")
		}
		return controlJSON(w, map[string]string{"yaml": text})
	case r.Method == "POST" && r.URL.Path == "/manage/api/compose":
		var req struct {
			Document map[string]any `json:"document"`
		}
		if err := decodeControl(w, r, &req); err != nil {
			return err
		}
		b, err := yaml.Marshal(req.Document)
		if err != nil {
			return fmt.Errorf("Cannot encode configuration.")
		}
		if err := managementSafe(b); err != nil {
			return err
		}
		return controlJSON(w, map[string]string{"yaml": string(b)})
	case r.Method == "POST" && (r.URL.Path == "/manage/api/validate" || r.URL.Path == "/manage/api/draft" || r.URL.Path == "/manage/api/activate"):
		var req configRequest
		if err := decodeControl(w, r, &req); err != nil {
			return err
		}
		c, err := managedParse(req.YAML)
		if err != nil {
			return err
		}
		var doc any
		yaml.Unmarshal([]byte(req.YAML), &doc)
		if r.URL.Path == "/manage/api/validate" {
			return controlJSON(w, map[string]any{"valid": true, "document": doc, "revision": revision(req.YAML), "notice": "Validation makes no model calls and does not prove source or model availability."})
		}
		if req.Base != m.activeRevision() {
			http.Error(w, "Active configuration changed. Reload before saving.", 409)
			return nil
		}
		if r.URL.Path == "/manage/api/draft" {
			tx, err := m.db.Begin()
			if err != nil {
				return fmt.Errorf("Cannot save draft.")
			}
			defer tx.Rollback()
			var prior string
			err = tx.QueryRow(`SELECT revision FROM control_draft WHERE singleton=1`).Scan(&prior)
			if err != nil && err != sql.ErrNoRows {
				return fmt.Errorf("Cannot read draft revision.")
			}
			if prior != req.Expected {
				http.Error(w, "Draft changed in another session. Reload before saving.", 409)
				return nil
			}
			id := revision(req.YAML)
			if _, err = tx.Exec(`INSERT INTO control_draft VALUES(1,?,?,?) ON CONFLICT(singleton) DO UPDATE SET yaml=excluded.yaml,revision=excluded.revision,base=excluded.base`, req.YAML, id, req.Base); err != nil {
				return fmt.Errorf("Cannot save draft.")
			}
			if _, err = tx.Exec(`INSERT INTO control_audit(at,actor,operation,target,result) VALUES(?,'operator','save-draft',?,'saved')`, time.Now().UTC().Format(time.RFC3339Nano), id); err != nil {
				return fmt.Errorf("Cannot audit draft.")
			}
			if err = tx.Commit(); err != nil {
				return fmt.Errorf("Cannot commit draft.")
			}
			return controlJSON(w, configDraft{req.YAML, id, req.Base})
		}
		if !req.Confirm {
			return fmt.Errorf("Confirm collection scope, data destination, and the displayed configuration diff before activation.")
		}
		var saved configDraft
		if err = m.db.QueryRow(`SELECT yaml,revision,base FROM control_draft WHERE singleton=1`).Scan(&saved.YAML, &saved.Revision, &saved.Base); err != nil || saved.Revision != req.Expected || saved.YAML != req.YAML || saved.Base != req.Base {
			http.Error(w, "Save this exact draft against the active revision before applying it.", 409)
			return nil
		}
		if err = m.activate(r.Context(), c, req.YAML, req.Base, req.AllowLiveNotify); err != nil {
			return err
		}
		return controlJSON(w, map[string]string{"revision": m.activeRevision()})
	case r.Method == "POST" && r.URL.Path == "/manage/api/shadow":
		var req struct {
			Confirm bool `json:"confirm"`
		}
		if err := decodeControl(w, r, &req); err != nil {
			return err
		}
		if !req.Confirm {
			return fmt.Errorf("Confirm the safety change.")
		}
		// Close the gate before writing: a storage failure must not reopen effects.
		m.blocked.Store(true)
		tx, err := m.db.Begin()
		if err != nil {
			return fmt.Errorf("Effects blocked in memory, but persistence failed. Retry before restarting.")
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`INSERT INTO control_settings VALUES('shadow','true') ON CONFLICT(name) DO UPDATE SET value='true'`); err != nil {
			return fmt.Errorf("Cannot persist safety gate; retry before restarting.")
		}
		if _, err = tx.Exec(`INSERT INTO control_audit(at,actor,operation,target,result) VALUES(?,'operator','return-to-shadow','effects','blocked')`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("Effects blocked in memory, but the safety audit could not be saved. Retry before restarting.")
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("Cannot commit safety gate; retry before restarting.")
		}
		return controlJSON(w, map[string]bool{"shadow_override": true})
	case r.Method == "POST" && r.URL.Path == "/manage/api/test-model":
		return m.testModel(w, r)
	case r.Method == "GET" && r.URL.Path == "/manage/api/audit":
		rows, err := m.db.Query(`SELECT at,actor,operation,target,result FROM control_audit ORDER BY id DESC LIMIT 100`)
		if err != nil {
			return fmt.Errorf("Cannot read audit.")
		}
		defer rows.Close()
		out := []map[string]string{}
		for rows.Next() {
			var at, actor, op, target, result string
			if rows.Scan(&at, &actor, &op, &target, &result) != nil {
				return fmt.Errorf("Cannot decode audit.")
			}
			out = append(out, map[string]string{"at": at, "actor": actor, "operation": op, "target": target, "result": result})
		}
		if rows.Err() != nil {
			return fmt.Errorf("Cannot read audit.")
		}
		return controlJSON(w, out)
	default:
		return m.reviewRoute(w, r)
	}
}

func (m *manager) activate(ctx context.Context, c *config.Config, text, base string, allowLiveNotify bool) (resultErr error) {
	// Network binding and database relocation require a supervised process
	// restart. Refuse them here rather than claiming a partial activation.
	if c.Server != m.bootstrap.Server || c.DB != m.bootstrap.DB {
		return fmt.Errorf("Listener and database changes require a process restart; keep the current server and db settings.")
	}
	// Confirmation grants notification delivery only. It cannot authorize HTTP
	// actions, emitted events, agents, or removal of the persistent safety stop.
	for _, p := range c.Policies() {
		outcomes := []policy.Then{p.Default}
		for _, r := range p.Rules {
			outcomes = append(outcomes, r.Then)
		}
		for _, outcome := range outcomes {
			if outcome.Outcome() == "ignore" || outcome.Shadow {
				continue
			}
			if outcome.Notify == nil {
				return fmt.Errorf("Actions and delegation must remain in shadow.")
			}
			if !allowLiveNotify {
				return fmt.Errorf("Confirm live notifications explicitly, or keep notifications in shadow.")
			}
			if m.blocked.Load() {
				return fmt.Errorf("The persistent safety stop blocks live notifications. This activation cannot remove it.")
			}
			if outcome.Notify.Via == "discord" {
				webhook, err := m.discord.Resolve(outcome.Notify.WebhookEnv)
				if err != nil {
					return err
				}
				if err := notify.ValidateDiscordConnection(outcome.Notify.WebhookEnv, webhook); err != nil {
					return err
				}
			}
		}
	}
	if old := m.current.Load(); old != nil {
		for _, before := range old.cfg.Sources {
			if before.Type == "systemd-journal" && !before.Paused {
				cursor, err := m.db.Cursor(before.Name)
				if err != nil || cursor == "" {
					return fmt.Errorf("An active journal checkpoint is missing. Refusing to reset its position during activation.")
				}
			}
			for _, after := range c.Sources {
				if before.Name != after.Name {
					continue
				}
				// A renamed source starts a new cursor. Reusing an old cursor against a
				// different transport or item mapping can silently skip unrelated data.
				a, b := before, after
				a.Every = 0
				b.Every = 0
				a.Paused = false
				b.Paused = false
				a.Priority = ""
				b.Priority = ""
				a.Units = nil
				b.Units = nil
				// An omitted scope is the legacy system scope, not a new identity.
				if a.Type == "systemd-journal" && a.Scope == "" {
					a.Scope = "system"
				}
				if b.Type == "systemd-journal" && b.Scope == "" {
					b.Scope = "system"
				}
				if !reflect.DeepEqual(a, b) {
					return fmt.Errorf("A source identity or mapping changed. Use a new source name and explicitly accept a new cursor.")
				}
			}
		}
	}
	disk, err := os.ReadFile(m.path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Cannot inspect config file.")
	}
	if revision(string(disk)) != base {
		return fmt.Errorf("Configuration was changed outside the UI. Restart with the external file or restore the active version before applying.")
	}
	// Retain checkpoints for existing names; remove only fresh preparation
	// checkpoints if activation fails before the revision becomes active.
	fresh := []string{}
	runningNames := map[string]bool{}
	if old := m.current.Load(); old != nil {
		for _, s := range old.cfg.Sources {
			runningNames[s.Name] = !s.Paused
		}
	}
	for _, s := range c.Sources {
		var exists bool
		if err := m.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM source_cursors WHERE name=?)`, s.Name).Scan(&exists); err != nil {
			return fmt.Errorf("Cannot read source checkpoint.")
		}
		// An active poller may create its first checkpoint during preparation.
		// Never delete its progress, or an existing deliberately empty cursor.
		if !exists && !runningNames[s.Name] {
			fresh = append(fresh, s.Name)
		}
	}
	activated := false
	defer func() {
		if !activated {
			for _, name := range fresh {
				if _, err := m.db.Exec(`DELETE FROM source_cursors WHERE name=?`, name); err != nil {
					resultErr = fmt.Errorf("Activation did not complete and checkpoint cleanup failed. Inspect source cursors before retrying.")
				}
			}
		}
	}()
	if err := managementModelReady(c); err != nil {
		return err
	}
	prepareCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	jobs, err := m.prepare(prepareCtx, c)
	cancel()
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return fmt.Errorf("Activation request canceled before changes.")
	}
	old := m.current.Load()
	if m.stopSources != nil {
		close(m.stopSources)
		m.sources.Wait()
	}
	// After draining, old runners can be restarted without preparing or resetting
	// their checkpoints if a file conflict or storage failure prevents activation.
	resume := func() {
		if old != nil {
			m.startSources(old.jobs, old.rt)
		}
	}
	disk, err = os.ReadFile(m.path)
	if (err != nil && !os.IsNotExist(err)) || revision(string(disk)) != base {
		resume()
		return fmt.Errorf("Configuration changed while sources drained. No revision was activated.")
	}
	if ctx.Err() != nil {
		resume()
		return fmt.Errorf("Activation canceled; previous configuration retained.")
	}
	id := revision(text)
	if err = m.audit("activate", id, "intent"); err != nil {
		resume()
		return fmt.Errorf("Cannot audit activation; previous configuration retained.")
	}
	if _, err = m.db.Exec(`INSERT INTO control_revisions(id,yaml,created_at,status,base) VALUES(?,?,?,'prepared',?) ON CONFLICT(id) DO UPDATE SET status='prepared'`, id, text, time.Now().UTC().Format(time.RFC3339Nano), base); err != nil {
		resume()
		return fmt.Errorf("Cannot store revision.")
	}
	committed, syncErr := replaceConfig(m.path, text)
	if !committed {
		resume()
		return fmt.Errorf("Configuration file update failed. Previous revision remains active.")
	}
	snap := m.snapshot(c, text)
	snap.jobs = jobs
	m.current.Store(snap)
	activated = true
	m.startSources(jobs, snap.rt)
	// The file is startup authority. A crash after its rename is reconciled from
	// that file on boot; audit may retain intent rather than invent completion.
	if _, err = m.db.Exec(`UPDATE control_revisions SET status='active' WHERE id=?`, id); err != nil {
		return fmt.Errorf("Configuration is active, but history update failed. Reload status before retrying.")
	}
	if err = m.audit("activate", id, "active"); err != nil {
		return fmt.Errorf("Configuration is active, but completion audit failed.")
	}
	if syncErr != nil {
		return fmt.Errorf("Configuration is active, but directory sync failed. Verify disk health before restarting.")
	}
	return nil
}

// testDeliveryMessage is fixed so this endpoint can never carry caller text.
const testDeliveryMessage = "mrmr test message, sent from the management page. No event or incident caused this."

// testDeliveryInterval bounds how often a browser can post to the operator's channel.
const testDeliveryInterval = 30 * time.Second

// testDelivery sends one labeled message when an operator asks for it. It exists
// because "credential configured" says nothing about whether Discord accepts it.
// Only a webhook the active policy already references can be named, the safety gate
// still wins, and intent is audited first so a crash cannot hide an external send.
func (m *manager) testDelivery(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name    string `json:"name"`
		Confirm bool   `json:"confirm"`
	}
	if err := decodeControl(w, r, &req); err != nil {
		return err
	}
	if !req.Confirm {
		return fmt.Errorf("Confirm the test message first; nothing was sent.")
	}
	if m.blocked.Load() {
		return fmt.Errorf("The safety gate is blocking effects; nothing was sent.")
	}
	if !m.activeDiscordReference(req.Name) {
		return fmt.Errorf("That Discord connection is not used by the active configuration; nothing was sent.")
	}
	var last sql.NullString
	if err := m.db.QueryRow(`SELECT MAX(at) FROM control_audit WHERE operation='test-delivery' AND target=? AND result='intent'`, req.Name).Scan(&last); err != nil {
		return fmt.Errorf("Cannot read test history; nothing was sent.")
	}
	if at, err := time.Parse(time.RFC3339Nano, last.String); err == nil && time.Since(at) < testDeliveryInterval {
		return fmt.Errorf("A test message was just sent. Wait 30 seconds before sending another.")
	}
	webhook, err := m.discord.Resolve(req.Name)
	if err != nil {
		return fmt.Errorf("That Discord connection has no saved webhook; nothing was sent.")
	}
	if err := m.audit("test-delivery", req.Name, "intent"); err != nil {
		return fmt.Errorf("Cannot record test intent; nothing was sent.")
	}
	sendErr := notify.Discord(r.Context(), &http.Client{}, webhook, testDeliveryMessage)
	result := "delivered"
	if sendErr != nil {
		result = "failed"
	}
	if err := m.audit("test-delivery", req.Name, result); err != nil {
		return fmt.Errorf("The test finished, but its audit could not be saved.")
	}
	if sendErr != nil {
		// notify.Discord errors are written to exclude the credential-bearing URL.
		return fmt.Errorf("Discord did not accept the test message: %v", sendErr)
	}
	return controlJSON(w, map[string]string{"name": req.Name, "at": time.Now().UTC().Format(time.RFC3339Nano)})
}

func (m *manager) activeDiscordReference(name string) bool {
	snap := m.current.Load()
	if snap == nil || name == "" {
		return false
	}
	for _, p := range snap.cfg.Policies() {
		outcomes := []policy.Then{p.Default}
		for _, rule := range p.Rules {
			outcomes = append(outcomes, rule.Then)
		}
		for _, outcome := range outcomes {
			if n := outcome.Notify; n != nil && n.Via == "discord" && n.WebhookEnv == name {
				return true
			}
		}
	}
	return false
}
