package superhuman

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

func testRecord() credentialRecord {
	return credentialRecord{Version: 1, Resource: endpoint,
		Config: oauth2.Config{ClientID: "test-client", Endpoint: oauth2.Endpoint{TokenURL: tokenEndpoint, AuthStyle: oauth2.AuthStyleInParams}},
		Token:  oauth2.Token{AccessToken: "test-access-original", RefreshToken: "test-refresh-original", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)},
	}
}

func TestPrivateStoreAndLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.lock.Close()
	if _, err := s.read(); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("missing grant: %v", err)
	}
	if second, err := openStore(dir); err == nil {
		second.lock.Close()
		t.Fatal("concurrent owner accepted")
	}
	record := testRecord()
	if err := s.write(&record); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "credentials.json")
	before, _ := os.ReadFile(path)
	record.Config.Endpoint.TokenURL = "https://evil.invalid/steal"
	if err := s.write(&record); err == nil {
		t.Fatal("foreign token endpoint accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("invalid grant replaced saved state")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.read(); err == nil {
		t.Fatal("world-readable credential accepted")
	}
	os.Chmod(path, 0600)
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".original", path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.read(); err == nil {
		t.Fatal("symlink credential accepted")
	}
	if err := s.lock.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := openStore(dir)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	second.lock.Close()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if third, err := openStore(dir); err == nil {
		third.lock.Close()
		t.Fatal("public state directory accepted")
	}
}

func TestRotatedTokenIsRetainedAfterSaveFailure(t *testing.T) {
	f := newAuthFixture(t)
	s, err := openStore(filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.lock.Close()
	record := testRecord()
	record.Token.Expiry = time.Now().Add(-time.Hour)
	if err := s.write(&record); err != nil {
		t.Fatal(err)
	}
	source := &persistingTokens{ctx: context.WithValue(t.Context(), oauth2.HTTPClient, f.hc), store: s, record: record}
	path := filepath.Join(s.dir, "credentials.json")
	os.Rename(path, path+".old")
	os.Mkdir(path, 0700) // force atomic rename to fail after the remote rotation
	if _, err := source.Token(); err == nil {
		t.Fatal("used a token whose rotation was not saved")
	}
	os.Remove(path)
	if _, err := source.Token(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refreshes != 1 {
		t.Fatalf("rotation repeated after save failure: %d", f.refreshes)
	}
	saved, err := s.read()
	if err != nil || saved.Token.RefreshToken != f.refresh {
		t.Fatal("rotated token was not durably retried")
	}
}

func TestCredentialValidation(t *testing.T) {
	for _, mutate := range []func(*credentialRecord){
		func(r *credentialRecord) { r.Version = 2 },
		func(r *credentialRecord) { r.Resource = "https://evil.invalid" },
		func(r *credentialRecord) { r.Token.RefreshToken = "" },
		func(r *credentialRecord) { r.Token.TokenType = "Basic" },
		func(r *credentialRecord) { r.Token.Expiry = time.Time{} },
	} {
		r := testRecord()
		mutate(&r)
		if r.validate() == nil {
			t.Fatal("invalid credential record accepted")
		}
	}
	s, err := openStore(filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.lock.Close()
	for _, data := range []string{`{"token":"SECRET"}`, `{} {}`, strings.Repeat(" ", (1<<20)+1)} {
		if err := os.WriteFile(filepath.Join(s.dir, "credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.read(); err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("corrupt credentials accepted or exposed")
		}
	}
}

func TestCallbackRejectsUnsolicitedRequests(t *testing.T) {
	results := make(chan *auth.AuthorizationResult, 1)
	handler := callbackHandler("127.0.0.1:18742", "expected", results)
	for _, query := range []string{"code=SECRET", "state=wrong&code=SECRET", "state=expected&state=wrong&code=SECRET", "state=expected&code=SECRET&code=OTHER", "state=expected", "state=expected&code=SECRET&error=denied"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:18742/callback?"+query, nil)
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != 400 || len(results) != 0 || strings.Contains(w.Body.String(), "SECRET") {
			t.Fatalf("invalid callback accepted or exposed: status=%d", w.Code)
		}
	}
	for _, host := range []string{"evil.invalid", "127.0.0.1:18742"} {
		r := httptest.NewRequest("GET", "http://"+host+"/callback?state=expected&code=test-code&iss=test-issuer", nil)
		w := httptest.NewRecorder()
		handler(w, r)
		if host == "evil.invalid" && (w.Code != 400 || len(results) != 0) {
			t.Fatal("callback accepted another Host")
		}
	}
	result := <-results
	if result.Iss != "test-issuer" || result.State != "expected" {
		t.Fatal("callback lost issuer/state validation inputs")
	}
}

func TestCallbackCancellationClosesListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = receiveCode(ctx, listener, "https://"+authHost+"/oauth2/authorize?state=expected", func(string) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("callback cancellation: %v", err)
	}
	// receiveCode must join its server before returning, not merely request
	// shutdown and leave a goroutine holding the callback port.
	next, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatalf("callback listener leaked: %v", err)
	}
	next.Close()
}

func TestDeniedLoginPreservesExistingGrant(t *testing.T) {
	f := newAuthFixture(t)
	dir := filepath.Join(t.TempDir(), "private")
	s, _ := openStore(dir)
	record := testRecord()
	if err := s.write(&record); err != nil {
		t.Fatal(err)
	}
	s.lock.Close()
	path := filepath.Join(dir, "credentials.json")
	before, _ := os.ReadFile(path)
	err := login(t.Context(), dir, 0, func(link string) error {
		u, _ := url.Parse(link)
		callback, _ := url.Parse(u.Query().Get("redirect_uri"))
		callback.RawQuery = url.Values{"state": {u.Query().Get("state")}, "error": {"access_denied"}, "error_description": {"SECRET"}}.Encode()
		resp, err := http.Get(callback.String())
		if err == nil {
			resp.Body.Close()
		}
		return err
	}, f.hc)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("denial accepted or leaked")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("denied sign-in destroyed existing grant")
	}
}

func TestLoginRejectsMismatchedIssuer(t *testing.T) {
	f := newAuthFixture(t)
	dir := filepath.Join(t.TempDir(), "private")
	err := login(t.Context(), dir, 0, func(link string) error {
		u, _ := url.Parse(link)
		callback, _ := url.Parse(u.Query().Get("redirect_uri"))
		callback.RawQuery = url.Values{"state": {u.Query().Get("state")}, "code": {"test-code"}, "iss": {"https://evil.invalid"}}.Encode()
		resp, err := http.Get(callback.String())
		if err == nil {
			resp.Body.Close()
		}
		return err
	}, f.hc)
	if err == nil {
		t.Fatal("mismatched issuer accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("untrusted issuer produced saved credentials")
	}
}
