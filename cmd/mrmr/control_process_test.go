package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/heath0xff/mrmr/internal/storage"
)

// The helper is a real process so signal cancellation and HTTP handler ownership
// are exercised without sending process-wide signals to the test runner itself.
func TestManagedProcessHelper(t *testing.T) {
	if os.Getenv("MRMR_TEST_MANAGED_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	if err := runManaged(os.Getenv("MRMR_TEST_CONFIG"), os.Getenv("MRMR_TEST_ADDR"), "", "", "MRMR_TEST_CONTROL_TOKEN", false, ""); err != nil {
		t.Fatal(err)
	}
}
func TestManagedProcessDrainsHTTPBeforeClosingStorage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("interrupt-based lifecycle test")
	}
	modelStarted, release := make(chan struct{}), make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(modelStarted)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer model.Close()
	defer close(release)
	reserveAddress := func() net.Listener {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return l
	}
	control, ingress := reserveAddress(), reserveAddress()
	controlAddr, ingressAddr := control.Addr().String(), ingress.Addr().String()
	control.Close()
	ingress.Close()
	dir := t.TempDir()
	configPath, dbPath := filepath.Join(dir, "mrmr.yaml"), filepath.Join(dir, "mrmr.db")
	text := strings.ReplaceAll(controlYAML, "127.0.0.1:4242", ingressAddr)
	text = strings.ReplaceAll(text, "path: test.db", "path: "+strconv.Quote(dbPath))
	text = strings.ReplaceAll(text, "http://127.0.0.1:1/v1", model.URL)
	if err := os.WriteFile(configPath, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestManagedProcessHelper$", "-test.timeout=20s")
	child.Env = append(os.Environ(), "HOME="+dir, "XDG_CONFIG_HOME="+dir, "MRMR_TEST_MANAGED_CHILD=1", "MRMR_TEST_CONFIG="+configPath, "MRMR_TEST_ADDR="+controlAddr, "MRMR_TEST_CONTROL_TOKEN="+testControlToken)
	var output bytes.Buffer
	child.Stdout = &output
	child.Stderr = &output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var childErr error
	go func() { childErr = child.Wait(); close(exited) }()
	defer func() {
		select {
		case <-exited:
		default:
			child.Process.Kill()
			<-exited
		}
	}()
	client := &http.Client{Timeout: time.Second}
	ready := false
	for i := 0; i < 100; i++ {
		response, err := client.Get("http://" + controlAddr + "/manage/")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				ready = true
				break
			}
		}
		select {
		case <-exited:
			t.Fatalf("startup: %v\n%s", childErr, output.String())
		default:
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !ready {
		t.Fatal("management listener did not start")
	}
	if other, err := lockRuntimeDB(dbPath); err == nil {
		other.Close()
		t.Fatal("running management did not own the database lock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "POST", "http://"+ingressAddr+"/api/events", strings.NewReader(`{"type":"service.warning","source":"synthetic-shutdown-test","data":{"message":"fixture"}}`))
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	defer func() { cancel(); <-finished }()
	select {
	case <-modelStarted:
	case <-ctx.Done():
		t.Fatal("in-flight model request did not start")
	}
	if err := child.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
		if childErr != nil {
			t.Fatalf("shutdown: %v\n%s", childErr, output.String())
		}
	case <-ctx.Done():
		t.Fatal("runtime did not stop")
	}
	lock, err := lockRuntimeDB(dbPath)
	if err != nil {
		t.Fatal("shutdown did not release database ownership")
	}
	defer lock.Close()
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var decisions, executions, traces int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM decisions),(SELECT count(*) FROM executions WHERE outcome='ignore'),(SELECT count(*) FROM event_traces)`).Scan(&decisions, &executions, &traces); err != nil {
		t.Fatal(err)
	}
	if decisions != 1 || executions != 1 || traces == 0 {
		t.Fatalf("shutdown lost final records: decisions=%d executions=%d traces=%d\n%s", decisions, executions, traces, output.String())
	}
	info, err := os.Stat(dbPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("management database must be private")
	}
}
