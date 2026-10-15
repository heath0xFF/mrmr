package superhuman

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The production origin guard still runs in these tests. Only the underlying
// dial destination is replaced: all OAuth and MCP traffic terminates at one
// local fixture, with the SDK executing its real discovery/PKCE/refresh flow.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type authFixture struct {
	mu                         sync.Mutex
	access, refresh, challenge string
	refreshes, registrations   int
	calls                      []string
	rejectRefresh              bool
	hc                         *http.Client
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	f := &authFixture{access: "test-access-original", refresh: "test-refresh-original"}
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	for _, name := range []string{"list_accounts", "list_threads", "get_thread", "get_message", "send_draft"} {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls = append(f.calls, req.Params.Name)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"accounts":[{"accountEmail":"test@example.invalid","isPrimary":true,"addedAt":"2026-01-01T00:00:00Z"}]}`}}}, nil
		})
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			fmt.Fprintf(w, `{"resource":%q,"authorization_servers":["https://%s"]}`, endpoint, authHost)
		case "/.well-known/oauth-authorization-server":
			fmt.Fprintf(w, `{"issuer":"https://%s","authorization_endpoint":"https://%s/oauth2/authorize","token_endpoint":%q,"registration_endpoint":"https://%s/oauth2/register","scopes_supported":["openid","email","profile","offline_access"],"code_challenge_methods_supported":["S256"],"response_types_supported":["code"],"token_endpoint_auth_methods_supported":["none"]}`, authHost, authHost, tokenEndpoint, authHost)
		case "/oauth2/register":
			f.mu.Lock()
			f.registrations++
			f.mu.Unlock()
			var metadata map[string]any
			if json.NewDecoder(r.Body).Decode(&metadata) != nil || metadata["client_name"] != "mrmr" {
				t.Error("invalid client registration")
			}
			fmt.Fprint(w, `{"client_id":"test-client","token_endpoint_auth_method":"none"}`)
		case "/oauth2/token":
			f.mu.Lock()
			defer f.mu.Unlock()
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			switch r.Form.Get("grant_type") {
			case "authorization_code":
				digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
				if r.Form.Get("code") != "test-code" || r.Form.Get("resource") != endpoint || base64.RawURLEncoding.EncodeToString(digest[:]) != f.challenge {
					t.Error("invalid code/PKCE/resource exchange")
				}
			case "refresh_token":
				f.refreshes++
				if r.Form.Get("refresh_token") != f.refresh {
					t.Error("reused an obsolete refresh token")
				}
				if f.rejectRefresh {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":"invalid_grant","error_description":"test-access-original test-refresh-original PRIVATE-MAIL"}`)
					return
				}
				f.access = fmt.Sprintf("test-access-%d", f.refreshes)
				f.refresh = fmt.Sprintf("test-refresh-%d", f.refreshes)
			default:
				t.Error("unexpected grant type")
			}
			fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"token_type":"Bearer","expires_in":3600}`, f.access, f.refresh)
		case "/mcp":
			f.mu.Lock()
			valid := r.Header.Get("Authorization") == "Bearer "+f.access
			f.mu.Unlock()
			if !valid {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.mail.superhuman.com/.well-known/oauth-protected-resource/mcp"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			mcpHandler.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	f.hc = newHTTPClient()
	f.hc.Transport = guardedTransport{roundTripFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		copy.URL.Scheme, copy.URL.Host = target.Scheme, target.Host
		copy.Host = target.Host
		return srv.Client().Transport.RoundTrip(copy)
	})}
	return f
}

func (f *authFixture) browser(t *testing.T) func(string) error {
	t.Helper()
	return func(link string) error {
		u, err := url.Parse(link)
		if err != nil {
			return err
		}
		q := u.Query()
		if u.Host != authHost || q.Get("code_challenge_method") != "S256" || !strings.Contains(q.Get("scope"), "offline_access") {
			t.Error("missing PKCE or offline access")
		}
		f.mu.Lock()
		f.challenge = q.Get("code_challenge")
		f.mu.Unlock()
		callback, _ := url.Parse(q.Get("redirect_uri"))
		if callback.Hostname() != "127.0.0.1" {
			return fmt.Errorf("callback is not loopback")
		}
		callback.RawQuery = url.Values{"state": {q.Get("state")}, "code": {"test-code"}, "iss": {"https://" + authHost}}.Encode()
		resp, err := http.Get(callback.String())
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("callback status %d", resp.StatusCode)
		}
		return nil
	}
}

func TestLoginCheckRefreshAndRestart(t *testing.T) {
	f := newAuthFixture(t)
	dir := filepath.Join(t.TempDir(), "credentials")
	if err := login(t.Context(), dir, 0, f.browser(t), f.hc); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if len(f.calls) != 0 {
		t.Error("login invoked mail tools")
	}
	f.mu.Unlock()
	for i := 0; i < 3; i++ {
		// Each check opens a new store and session, as after a process restart.
		// Expire the saved access token to force rotation on subsequent checks.
		if i > 0 {
			s, err := openStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			r, err := s.read()
			if err != nil {
				t.Fatal(err)
			}
			r.Token.Expiry = time.Now().Add(-time.Hour)
			if err := s.write(r); err != nil {
				t.Fatal(err)
			}
			s.lock.Close()
		}
		count, err := check(t.Context(), dir, f.hc)
		if err != nil || count != 1 {
			t.Fatalf("check %d: count=%d err=%v", i, count, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refreshes != 2 || f.registrations != 1 || strings.Join(f.calls, ",") != "list_accounts,list_accounts,list_accounts" {
		t.Fatalf("refreshes=%d registrations=%d calls=%v", f.refreshes, f.registrations, f.calls)
	}
	info, err := os.Stat(filepath.Join(dir, "credentials.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("credential permissions: %v %v", info, err)
	}
}

func TestRefreshErrorDoesNotLeakOrRegisterAgain(t *testing.T) {
	f := newAuthFixture(t)
	dir := filepath.Join(t.TempDir(), "credentials")
	if err := login(t.Context(), dir, 0, f.browser(t), f.hc); err != nil {
		t.Fatal(err)
	}
	s, _ := openStore(dir)
	r, _ := s.read()
	r.Token.Expiry = time.Now().Add(-time.Hour)
	if err := s.write(r); err != nil {
		t.Fatal(err)
	}
	s.lock.Close()
	f.mu.Lock()
	f.rejectRefresh = true
	f.mu.Unlock()
	_, err := check(t.Context(), dir, f.hc)
	if err == nil {
		t.Fatal("refresh failure accepted")
	}
	for _, secret := range []string{"test-access-original", "test-refresh-original", "PRIVATE-MAIL"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("remote error leaked")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registrations != 1 || len(f.calls) != 0 {
		t.Fatal("unattended check expanded authority or called tools after refresh failure")
	}
}

func TestOriginGuardAndResponseBound(t *testing.T) {
	calls := 0
	transport := guardedTransport{roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat("x", (4<<20)+1)))}, nil
	})}
	for _, target := range []string{"http://mcp.mail.superhuman.com/mcp", "https://evil.invalid/mcp", "https://mcp.mail.superhuman.com:444/mcp", "https://user@mcp.mail.superhuman.com/mcp", "https://mcp.mail.superhuman.com.evil.invalid/mcp"} {
		r, _ := http.NewRequest("GET", target, nil)
		if _, err := transport.RoundTrip(r); err == nil {
			t.Errorf("allowed %s", target)
		}
	}
	if calls != 0 {
		t.Fatal("untrusted origin reached network")
	}
	r, _ := http.NewRequest("GET", endpoint, nil)
	resp, err := transport.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("oversized response accepted")
	}
	if err := newHTTPClient().CheckRedirect(r, nil); err != http.ErrUseLastResponse {
		t.Fatal("redirects must not forward credentials")
	}
}
