package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func testDeliveryRequest(m *manager, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://localhost/manage/api/test-delivery", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Host = "localhost"
	r.Header.Set("Authorization", "Bearer "+testControlToken)
	r.Header.Set("Origin", "http://localhost")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Mrmr-Control", "1")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	return w
}

// Each refusal happens before any network use, so these cannot send anything. The
// successful send is covered by notify's adapter tests, which never reach Discord.
func TestTestDeliveryRefusals(t *testing.T) {
	m := testManager(t)
	for _, tc := range []struct{ name, body, want string }{
		{"unconfirmed", `{"name":"X","confirm":false}`, "Confirm"},
		{"not in active policy", `{"name":"SOME_OTHER_WEBHOOK","confirm":true}`, "not used by the active configuration"},
		{"empty name", `{"name":"","confirm":true}`, "not used by the active configuration"},
		{"caller text is rejected", `{"name":"X","confirm":true,"message":"hello"}`, "Invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := testDeliveryRequest(m, tc.body)
			if w.Code != 400 || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
	m.blocked.Store(true)
	if w := testDeliveryRequest(m, `{"name":"X","confirm":true}`); w.Code != 400 || !strings.Contains(w.Body.String(), "safety gate") {
		t.Fatalf("safety gate must win: %d %s", w.Code, w.Body.String())
	}
	var intents int
	if err := m.db.QueryRow(`SELECT count(*) FROM control_audit WHERE operation='test-delivery'`).Scan(&intents); err != nil || intents != 0 {
		t.Fatalf("a refusal recorded a send attempt: %d %v", intents, err)
	}
}
