package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/policy"
	"github.com/heath0xff/mrmr/internal/runtime"
	"github.com/heath0xff/mrmr/internal/storage"
)

func testDashboard(t *testing.T) (http.Handler, *runtime.Runtime) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rt := &runtime.Runtime{DB: db}
	t.Setenv("DASHBOARD_TEST_KEY", "synthetic-private-key")
	cfg := &config.Config{
		Models:  map[string]model.Config{"judge": {Model: "d1", APIKeyEnv: "DASHBOARD_TEST_KEY", BaseURL: "https://private.invalid/model"}},
		Sources: []config.Source{{Name: "journal", Type: "systemd-journal", Every: 30 * time.Second, Units: []string{"test.service"}, Priority: "warning"}},
		Policy:  policy.Policy{Rules: []policy.Rule{{If: map[string]any{"result.urgent": true}, Then: policy.Then{Notify: &policy.Notify{Via: "discord", WebhookEnv: "DASHBOARD_TEST_KEY"}, Shadow: true}}}, Default: policy.Then{Ignore: true}},
	}
	_, err = db.InsertEvent(event.Event{ID: "evt_test", Type: "warning", Source: "journal", Subject: "test.service", Timestamp: time.Now().UTC(), Data: map[string]any{
		"message": "<script>alert('no')</script> synthetic-private-key https://discord.com/api/webhooks/test/private-token Bearer private-bearer",
		"token":   "private-field", "number": json.Number("9007199254740993"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return New(rt, cfg), rt
}

func request(h http.Handler, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost:4242"+path, nil)
	r.RemoteAddr = "127.0.0.1:50000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestDashboardLocalReadOnlyAndRedaction(t *testing.T) {
	h, rt := testDashboard(t)
	rt.StartPoll("journal")
	rt.FinishPoll("journal", nil)
	for _, path := range []string{"/dashboard/", "/dashboard/dashboard.js", "/dashboard/dashboard.css", "/dashboard/api/overview", "/dashboard/api/events", "/dashboard/api/event?id=evt_test"} {
		w := request(h, "GET", path)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatalf("missing privacy headers: %v", w.Header())
		}
		for _, secret := range []string{"synthetic-private-key", "private-token", "private-bearer", "private-field", "https://private.invalid"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("%s leaked a test credential", path)
			}
		}
		if strings.Contains(path, "/api/") && !json.Valid(w.Body.Bytes()) {
			t.Fatalf("invalid JSON: %s", w.Body.String())
		}
		if request(h, "POST", path).Code != 405 || request(h, "DELETE", path).Code != 405 {
			t.Fatalf("writable route %s", path)
		}
	}
	w := request(h, "GET", "/dashboard/api/event?id=evt_test")
	if !strings.Contains(w.Body.String(), "9007199254740993") || strings.Contains(w.Body.String(), "<script>") {
		t.Fatalf("number rounding or unescaped HTML: %s", w.Body.String())
	}
	var count int
	if err := rt.DB.QueryRow(`SELECT count(*) FROM events`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reads changed events: %d %v", count, err)
	}
	if err := rt.DB.QueryRow(`SELECT count(*) FROM decisions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("reads invoked interpretation: %d %v", count, err)
	}
	w = request(h, "HEAD", "/dashboard/api/overview")
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("HEAD should not return data")
	}
}

func TestDashboardRedactsBeforeTruncatingAndPreservesDisplayNumbers(t *testing.T) {
	h, rt := testDashboard(t)
	message := strings.Repeat("x", 230) + "synthetic-private-key"
	if _, err := rt.DB.Exec(`UPDATE events SET data=json_set(data,'$.message',?)`, message); err != nil {
		t.Fatal(err)
	}
	w := request(h, "GET", "/dashboard/api/events")
	if w.Code != 200 || strings.Contains(w.Body.String(), "synthetic-") {
		t.Fatalf("partial credential leaked: %d %s", w.Code, w.Body.String())
	}
	w = request(h, "GET", "/dashboard/api/event?id=evt_test")
	var detail struct {
		EventJSON string `json:"event_json"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail.EventJSON, "9007199254740993") {
		t.Fatal("display text lost exact number")
	}
}

func TestDashboardRejectsRemoteAndRebindingRequests(t *testing.T) {
	h, _ := testDashboard(t)
	for _, tc := range []struct {
		peer, host, origin, site string
		want                     int
	}{
		{"192.0.2.1:1234", "localhost:4242", "", "", 403},
		{"127.0.0.1:1234", "attacker.example:4242", "", "", 403},
		{"127.0.0.1:1234", "localhost:4242", "http://attacker.example", "", 403},
		{"127.0.0.1:1234", "localhost:4242", "", "cross-site", 403},
		{"127.0.0.1:1234", "localhost:4242", "null", "", 403},
		{"[::1]:1234", "[::1]:4242", "http://[::1]:4242", "same-origin", 200},
		{"127.0.0.1:1234", "127.0.0.1:14242", "", "none", 200},
	} {
		r := httptest.NewRequest("GET", "http://localhost/dashboard/api/overview", nil)
		r.RemoteAddr = tc.peer
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", tc.site)
		// Proxy headers must not upgrade an untrusted connection into loopback.
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		r.Header.Set("X-Forwarded-Host", "localhost:4242")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%+v: %d", tc, w.Code)
		}
	}
}

func TestDashboardPrivateProxyStillHasNoWriteRoutes(t *testing.T) {
	_, rt := testDashboard(t)
	h := New(rt, &config.Config{Server: config.Server{DashboardHost: "server.example.ts.net"}})
	for _, tc := range []struct {
		method, path, peer, host string
		want                     int
	}{
		{"GET", "/dashboard/api/overview", "127.0.0.1:1234", "server.example.ts.net", 200},
		{"GET", "/dashboard/api/overview", "192.0.2.1:1234", "server.example.ts.net", 403},
		{"GET", "/dashboard/api/overview", "127.0.0.1:1234", "other.example.ts.net", 403},
		{"GET", "/api/events", "127.0.0.1:1234", "server.example.ts.net", 404},
		{"POST", "/api/events", "127.0.0.1:1234", "server.example.ts.net", 405},
		{"POST", "/dashboard/../api/events", "127.0.0.1:1234", "server.example.ts.net", 405},
		{"POST", "/dashboard/api/events", "127.0.0.1:1234", "server.example.ts.net", 405},
	} {
		r := httptest.NewRequest(tc.method, "http://localhost"+tc.path, nil)
		r.Host, r.RemoteAddr = tc.host, tc.peer
		r.Header.Set("Origin", "https://"+tc.host)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%+v: %d", tc, w.Code)
		}
	}
}

func TestDashboardProxyDoesNotTrustForwardedHeaders(t *testing.T) {
	_, rt := testDashboard(t)
	h := New(rt, &config.Config{Server: config.Server{DashboardHost: "server.example.ts.net"}})
	for _, tc := range []struct{ host, origin, site string }{
		{"attacker.example", "", ""},
		{"server.example.ts.net", "https://attacker.example", ""},
		{"server.example.ts.net", "", "cross-site"},
	} {
		r := httptest.NewRequest("GET", "http://localhost/dashboard/api/overview", nil)
		r.Host, r.RemoteAddr = tc.host, "127.0.0.1:1234"
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", tc.site)
		r.Header.Set("X-Forwarded-Host", "server.example.ts.net")
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%+v: %d", tc, w.Code)
		}
	}
}

func TestDashboardInputAndStorageErrors(t *testing.T) {
	h, rt := testDashboard(t)
	for _, path := range []string{"/dashboard/api/events?offset=-1", "/dashboard/api/events?offset=100001", "/dashboard/api/events?offset=no", "/dashboard/api/events?view=bogus", "/dashboard/api/events?q=" + strings.Repeat("x", 201), "/dashboard/api/event"} {
		if w := request(h, "GET", path); w.Code != 400 {
			t.Fatalf("%s = %d", path, w.Code)
		}
	}
	for _, path := range []string{"/dashboard/api/event?id=missing", "/dashboard/not-a-file", "/dashboard/../mrmr.yaml"} {
		if w := request(h, "GET", path); w.Code != 404 {
			t.Fatalf("%s = %d", path, w.Code)
		}
	}
	rt.DB.Close()
	if w := request(h, "GET", "/dashboard/api/overview"); w.Code != 503 || strings.Contains(w.Body.String(), "sqlite") {
		t.Fatalf("unsafe storage error: %d %s", w.Code, w.Body.String())
	}
}
