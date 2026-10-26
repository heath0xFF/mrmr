package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDashboardServerProcess(t *testing.T) {
	if os.Getenv("MRMR_TEST_DASHBOARD_PROCESS") != "1" {
		return
	}
	if err := run([]string{"-config", os.Getenv("MRMR_TEST_CONFIG")}); err != nil {
		t.Fatal(err)
	}
}

func TestSeparateDashboardListenerAndShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shutdown test uses POSIX interrupt")
	}
	address := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close()
		return addr
	}
	ingest, dashboard := address(), address()
	dir := t.TempDir()
	config := filepath.Join(dir, "mrmr.yaml")
	text := fmt.Sprintf(`server:
  addr: %q
  dashboard_addr: %q
  dashboard_host: server.example.ts.net
db:
  path: %q
models:
  judge:
    provider: openai-compatible
    base_url: http://127.0.0.1:1
    model: synthetic
interpret:
  model: judge
  prompt: Classify this event.
  schema:
    category:
      type: string
default:
  ignore: true
`, ingest, dashboard, filepath.Join(dir, "mrmr.db"))
	if err := os.WriteFile(config, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	// A child process owns signal handling. The parent always cancels and joins
	// it, including failed assertions, so no listener survives the test.
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDashboardServerProcess$")
	cmd.Env = append(os.Environ(), "MRMR_TEST_DASHBOARD_PROCESS=1", "MRMR_TEST_CONFIG="+config)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			<-done
		}
	})
	client := &http.Client{Timeout: time.Second}
	ready := false
	for until := time.Now().Add(10 * time.Second); time.Now().Before(until); {
		resp, err := client.Get("http://" + dashboard + "/dashboard/api/overview")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			ready = resp.StatusCode == 200
		}
		if ready {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("dashboard listener did not become ready")
	}
	for _, tc := range []struct {
		addr, path string
		want       int
	}{
		{dashboard, "/api/events", 405},
		{dashboard, "/dashboard/api/events", 405},
		{ingest, "/api/events", 400},
	} {
		resp, err := client.Post("http://"+tc.addr+tc.path, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%+v: %d", tc, resp.StatusCode)
		}
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		joined = true
		if err != nil {
			t.Fatalf("shutdown: %v\n%s", err, output.String())
		}
	case <-time.After(12 * time.Second):
		t.Fatal("servers did not shut down")
	}
	for _, addr := range []string{ingest, dashboard} {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			t.Fatalf("listener survived shutdown: %s", addr)
		}
	}
}
