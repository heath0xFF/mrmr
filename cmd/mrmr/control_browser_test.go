package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in browser tests run against temporary SQLite and synthetic data only.
// The executable path is supplied by the operator, never hard-coded to a Mac.
func TestManagementBrowser(t *testing.T) {
	chrome := os.Getenv("MRMR_CHROME")
	if chrome == "" {
		t.Skip("set MRMR_CHROME to run real-browser management journeys")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MRMR_DISCORD_WEBHOOK_URL", "")
	t.Setenv("MRMR_BROWSER_MISSING_CREDENTIAL", "")
	m := testManager(t)
	controlEvent(t, m)
	if _, err = m.db.Exec(`UPDATE events SET data=json_set(data,'$.message','<img src=x onerror="window.mrmrInjected=true">') WHERE id='evt_review'`); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	var modelCalls atomic.Int32
	modelServer := syntheticModel(t, &modelCalls)
	defer modelServer.Close()
	// Empty local poll results allow the UI pause path to run without touching
	// the host journal or an actual service, and without creating model work.
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("[]")) }))
	defer sourceServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	script := filepath.Join("..", "..", "internal", "dashboard", "manage.browser.cjs")
	cmd := exec.CommandContext(ctx, node, script)
	cmd.Env = append(os.Environ(), "MRMR_BROWSER_URL="+srv.URL, "MRMR_BROWSER_TOKEN="+testControlToken, "MRMR_BROWSER_MODEL_URL="+modelServer.URL, "MRMR_BROWSER_SOURCE_URL="+sourceServer.URL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser: %v\n%s", err, out)
	}
	if modelCalls.Load() != 4 {
		t.Fatalf("guided setup made %d model calls, want two explicit two-example tests", modelCalls.Load())
	}
	t.Log(string(out))
}
