package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPModes(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		status                          int
		body                            string
		emit, wantCompletion, wantError bool
	}{
		{"accepted legacy", 202, "accepted", false, false, false},
		{"empty legacy", 204, "", false, false, false},
		{"structured ignored by legacy", 200, `{"status":"completed"}`, false, false, false},
		{"completion", 200, `{"status":"completed"}`, true, true, false},
		{"failed work is an observation", 200, `{"status":"failed","summary":"No access"}`, true, true, false},
		{"cancelled work", 200, `{"status":"cancelled"}`, true, true, false},
		{"not a callback protocol", 202, `{"status":"completed"}`, true, false, true},
		{"reversed timestamps", 200, `{"status":"completed","started_at":"2026-01-02T00:00:00Z","completed_at":"2026-01-01T00:00:00Z"}`, true, false, true},
		{"endpoint error", 401, "PRIVATE_TOKEN", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			result, err := Run(context.Background(), srv.Client(), Config{Endpoint: srv.URL}, Request{EmitCompletion: tc.emit})
			if (err != nil) != tc.wantError || (result != nil) != tc.wantCompletion {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE_TOKEN") {
				t.Fatal("remote error content leaked")
			}
		})
	}
}

func TestInvalidTimeoutNeverDispatches(t *testing.T) {
	for _, timeout := range []time.Duration{time.Millisecond, -time.Second, 6 * time.Minute} {
		if _, err := Run(context.Background(), &http.Client{}, Config{Endpoint: "http://unused.invalid", Timeout: timeout}, Request{}); err == nil {
			t.Fatal("invalid timeout accepted")
		}
	}
}

func TestCancellationAndRedirects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, &http.Client{}, Config{Endpoint: "http://unused.invalid"}, Request{}); err == nil {
		t.Fatal("cancelled work accepted")
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed agent redirect") }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer srv.Close()
	if _, err := Run(context.Background(), srv.Client(), Config{Endpoint: srv.URL}, Request{}); err == nil {
		t.Fatal("redirect accepted")
	}
}
