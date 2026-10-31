package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const controlWebhook = "https://discord.com/api/v10/webhooks/123456789/synthetic_test_not_a_real_credential"

func TestDiscordEnrollmentBoundaryAndPersistence(t *testing.T) {
	m := testManager(t)
	before, _ := os.ReadFile(m.path)
	request := map[string]any{"name": "alerts", "url": controlWebhook, "expected": "", "confirm": true}
	for _, mode := range []string{"no token", "foreign origin", "no explicit header", "remote HTTP", "no confirmation", "invalid URL", "invalid name"} {
		t.Run(mode, func(t *testing.T) {
			body := map[string]any{}
			for k, v := range request {
				body[k] = v
			}
			if mode == "no confirmation" {
				body["confirm"] = false
			}
			if mode == "invalid URL" {
				body["url"] = "http://127.0.0.1/private"
			}
			if mode == "invalid name" {
				body["name"] = controlWebhook
			}
			data, _ := json.Marshal(body)
			r := httptest.NewRequest("POST", "http://localhost/manage/api/discord", bytes.NewReader(data))
			r.RemoteAddr = "127.0.0.1:1234"
			r.Header.Set("Authorization", "Bearer "+testControlToken)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Mrmr-Control", "1")
			r.Header.Set("Origin", "http://localhost")
			switch mode {
			case "no token":
				r.Header.Del("Authorization")
			case "foreign origin":
				r.Header.Set("Origin", "https://evil.invalid")
			case "no explicit header":
				r.Header.Del("X-Mrmr-Control")
			case "remote HTTP":
				m.host = "private.example"
				r.Host = m.host
				r.Header.Set("Origin", "http://"+m.host)
			}
			w := httptest.NewRecorder()
			m.ServeHTTP(w, r)
			if w.Code < 400 || strings.Contains(w.Body.String(), controlWebhook) {
				t.Fatalf("unsafe enrollment response: %d", w.Code)
			}
			if _, err := os.Stat(m.discord.Dir); !os.IsNotExist(err) {
				t.Fatal("rejected request created credential storage")
			}
		})
	}
	w := controlRequest(m, "POST", "/manage/api/discord", request)
	if w.Code != 200 || strings.Contains(w.Body.String(), controlWebhook) {
		t.Fatalf("save response: %d %s", w.Code, w.Body.String())
	}
	value, err := m.discord.Resolve("alerts")
	if err != nil || value != controlWebhook {
		t.Fatal("URL was not saved")
	}
	if w := controlRequest(m, "POST", "/manage/api/discord", request); w.Code != 409 {
		t.Fatalf("stale save: %d", w.Code)
	}
	for _, path := range []string{"/manage/api/state", "/manage/api/audit", "/dashboard/api/overview"} {
		response := controlRequest(m, "GET", path, nil)
		if response.Code != 200 {
			t.Fatalf("read response: %s returned %d", path, response.Code)
		}
		if strings.Contains(response.Body.String(), controlWebhook) {
			t.Fatal("read response exposed URL")
		}
	}
	after, _ := os.ReadFile(m.path)
	if !bytes.Equal(before, after) || m.activeRevision() != revision(string(before)) {
		t.Fatal("credential save changed active configuration")
	}
	for _, table := range []string{"events", "decisions", "executions", "control_draft"} {
		var count int
		if err := m.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("enrollment modified %s or count failed: %v", table, err)
		}
	}
	rows, err := m.db.Query("SELECT operation,target,result FROM control_audit")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var operation, target, result string
		if err := rows.Scan(&operation, &target, &result); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(operation+target+result, controlWebhook) {
			t.Fatal("audit leaked URL")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestDiscordURLNeverEntersConfigurationHistory(t *testing.T) {
	for _, text := range []string{
		controlYAML + "\n# " + controlWebhook,
		strings.Replace(controlYAML, "Classify this event.", "A prefix "+controlWebhook, 1),
		strings.Replace(controlYAML, "Classify this event.", `"A prefix https://\u0064iscord.com/api/v10/webhooks/123456789/synthetic_test_not_a_real_credential"`, 1),
	} {
		if err := managementSafe([]byte(text)); err == nil || strings.Contains(err.Error(), controlWebhook) {
			t.Fatal("credential-bearing configuration accepted or exposed")
		}
	}
}
