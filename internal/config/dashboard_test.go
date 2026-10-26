package config

import "testing"

func TestDashboardProxyBoundary(t *testing.T) {
	for _, tc := range []struct {
		addr, host string
		valid      bool
	}{
		{"", "", true},
		{"127.0.0.1:4244", "server.example.ts.net", true},
		{"[::1]:4244", "server.example.ts.net", true},
		{"0.0.0.0:4244", "server.example.ts.net", false},
		{"100.64.0.1:4244", "server.example.ts.net", false},
		{"localhost:4244", "server.example.ts.net", false},
		{"127.0.0.1:0", "server.example.ts.net", false},
		{"127.0.0.1:70000", "server.example.ts.net", false},
		{"127.0.0.1:4244", "", false},
		{"", "server.example.ts.net", false},
		{"127.0.0.1:4244", "https://server.example.ts.net", false},
		{"127.0.0.1:4244", "server.example.ts.net:443", false},
		{"127.0.0.1:4244", "*.example.ts.net", false},
		{"127.0.0.1:4244", "server.example.ts.net/path", false},
		{"127.0.0.1:4244", "server..ts.net", false},
	} {
		cfg := validConfig()
		cfg.Server.DashboardAddr, cfg.Server.DashboardHost = tc.addr, tc.host
		if err := cfg.Validate(); (err == nil) != tc.valid {
			t.Errorf("addr=%q host=%q: %v", tc.addr, tc.host, err)
		}
	}
}
