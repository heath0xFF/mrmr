package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Identity must open management only for a listed login on the exact proxy host,
// and must never weaken the browser-request checks that the token did not need.
func TestManagementTailscaleIdentity(t *testing.T) {
	const proxy = "marvin.example.ts.net"
	const allowed = "operator@example"
	for _, tc := range []struct {
		name, method, path, host, origin, control string
		logins                                    []string
		users                                     map[string]bool
		want                                      int
	}{
		{name: "allowed login reads state", method: "GET", path: "/manage/api/state", host: proxy, logins: []string{allowed}, want: 200},
		{name: "allowed login reads dashboard", method: "GET", path: "/dashboard/api/overview", host: proxy, logins: []string{allowed}, want: 200},
		{name: "no header", method: "GET", path: "/manage/api/state", host: proxy, want: 401},
		{name: "unlisted login", method: "GET", path: "/manage/api/state", host: proxy, logins: []string{"someone@example"}, want: 401},
		{name: "duplicated header", method: "GET", path: "/manage/api/state", host: proxy, logins: []string{allowed, allowed}, want: 401},
		{name: "localhost ignores identity", method: "GET", path: "/manage/api/state", host: "localhost", logins: []string{allowed}, want: 401},
		{name: "foreign host", method: "GET", path: "/manage/api/state", host: "evil.invalid", logins: []string{allowed}, want: 403},
		{name: "feature off", method: "GET", path: "/manage/api/state", host: proxy, logins: []string{allowed}, users: map[string]bool{}, want: 401},
		{name: "cross-origin write", method: "POST", path: "/manage/api/draft", host: proxy, origin: "https://evil.invalid", control: "1", logins: []string{allowed}, want: 403},
		{name: "write without control header", method: "POST", path: "/manage/api/draft", host: proxy, origin: "https://" + proxy, logins: []string{allowed}, want: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testManager(t)
			m.host = proxy
			m.tailscaleUsers = map[string]bool{allowed: true}
			if tc.users != nil {
				m.tailscaleUsers = tc.users
			}
			r := httptest.NewRequest(tc.method, "http://localhost"+tc.path, strings.NewReader("{}"))
			r.RemoteAddr = "127.0.0.1:1"
			r.Host = tc.host
			r.Header.Set("Content-Type", "application/json")
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.control != "" {
				r.Header.Set("X-Mrmr-Control", tc.control)
			}
			for _, login := range tc.logins {
				r.Header.Add("Tailscale-User-Login", login)
			}
			w := httptest.NewRecorder()
			m.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestParseTailscaleUsers(t *testing.T) {
	if users, err := parseTailscaleUsers("", ""); err != nil || users != nil {
		t.Fatalf("empty list must disable the feature: %v %v", users, err)
	}
	users, err := parseTailscaleUsers("a@example, b@example", "host.example")
	if err != nil || !users["a@example"] || !users["b@example"] {
		t.Fatalf("list: %v %v", users, err)
	}
	for _, tc := range [][2]string{{"a@example", ""}, {"a@example,", "host.example"}, {"a b", "host.example"}} {
		if _, err := parseTailscaleUsers(tc[0], tc[1]); err == nil {
			t.Fatalf("expected rejection of %q with host %q", tc[0], tc[1])
		}
	}
}
