package superhuman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

const (
	endpoint      = "https://mcp.mail.superhuman.com/mcp"
	authHost      = "mcp.auth.mail.superhuman.com"
	tokenEndpoint = "https://" + authHost + "/oauth2/token"
)

// This is deliberately not an arbitrary MCP client: neither configuration nor
// a model may change the resource, OAuth authority, or callable operation.
// Use the official SDK for protocol negotiation, discovery, and PKCE instead
// of maintaining our own implementation of those security-sensitive protocols.
type guardedTransport struct{ base http.RoundTripper }

func (t guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !allowedURL(req.URL) {
		return nil, errors.New("Superhuman request outside the allowed HTTPS origins")
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, safeError("Superhuman HTTPS request", err)
	}
	resp.Body = http.MaxBytesReader(nil, resp.Body, 4<<20)
	return resp, nil
}

func allowedURL(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.User == nil && u.Fragment == "" &&
		(u.Host == "mcp.mail.superhuman.com" || u.Host == authHost)
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: guardedTransport{http.DefaultTransport},
		// No redirects: a token, code, or client secret must never be forwarded
		// to a location chosen in an HTTP response, even within the allowlist.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func safeError(operation string, err error) error {
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, ErrLoginRequired} {
		if errors.Is(err, known) {
			return fmt.Errorf("%s: %w", operation, known)
		}
	}
	// External error strings may echo authorization codes, tokens, or mail.
	// Do not wrap them: even downstream debug logging must not recover them.
	return fmt.Errorf("%s failed; check connectivity or sign in again (remote details suppressed)", operation)
}

// storedAuth never opens a browser or expands scopes. An expired access token
// refreshes via the saved grant; a rejected grant requires explicit login.
type storedAuth struct{ source oauth2.TokenSource }

func (a storedAuth) TokenSource(context.Context) (oauth2.TokenSource, error) { return a.source, nil }
func (a storedAuth) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	resp.Body.Close()
	return ErrLoginRequired
}

func newClient() *mcp.Client {
	return mcp.NewClient(&mcp.Implementation{Name: "mrmr", Version: "dev"}, &mcp.ClientOptions{
		Capabilities:   &mcp.ClientCapabilities{},
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// Check authenticates without interactive fallback, checks the required tool
// catalog, and counts linked accounts. It never reads messages, changes the
// default account, or exposes account addresses in logs or CLI output.
func Check(ctx context.Context, dir string) (int, error) {
	return check(ctx, dir, newHTTPClient())
}

func check(ctx context.Context, dir string, hc *http.Client) (int, error) {
	store, err := openStore(dir)
	if err != nil {
		return 0, err
	}
	defer store.lock.Close()
	record, err := store.read()
	if err != nil {
		return 0, err
	}
	source := &persistingTokens{ctx: context.WithValue(ctx, oauth2.HTTPClient, hc), store: store, record: *record}
	session, err := newClient().Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: hc, OAuthHandler: storedAuth{source}, DisableStandaloneSSE: true,
	}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		return 0, safeError("connect to Superhuman", err)
	}
	defer session.Close()
	return checkSession(ctx, session)
}

func checkSession(ctx context.Context, session *mcp.ClientSession) (int, error) {
	wanted := map[string]bool{"list_accounts": false, "list_threads": false, "get_thread": false, "get_message": false}
	params := &mcp.ListToolsParams{}
	// Catalog pagination is bounded too. A server can't keep the CLI alive by
	// returning an endless cursor chain; no tool has been invoked at this point.
	for page := 0; ; page++ {
		if page == 10 {
			return 0, errors.New("Superhuman tool catalog exceeded 10 pages")
		}
		result, err := session.ListTools(ctx, params)
		if err != nil {
			return 0, safeError("list Superhuman tools", err)
		}
		for _, tool := range result.Tools {
			if _, ok := wanted[tool.Name]; ok {
				wanted[tool.Name] = true
			}
		}
		if result.NextCursor == "" {
			break
		}
		if result.NextCursor == params.Cursor {
			return 0, errors.New("Superhuman tool catalog repeated a cursor")
		}
		params.Cursor = result.NextCursor
	}
	for name, present := range wanted {
		if !present {
			return 0, fmt.Errorf("Superhuman is missing required read tool %s", name)
		}
	}
	// Fixed name and fixed arguments are the authority boundary. Server tool
	// annotations are hints, not permission; write tools are never dispatched.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_accounts", Arguments: map[string]any{}})
	if err != nil {
		return 0, safeError("list Superhuman accounts", err)
	}
	if result.IsError {
		return 0, errors.New("Superhuman rejected account listing")
	}
	var accounts struct {
		Accounts []struct {
			Email string `json:"accountEmail"`
		} `json:"accounts"`
	}
	// Prefer the typed payload. Older servers may only return JSON text; never
	// interpret free-form prose or fetch a URL from a tool result to recover it.
	var data []byte
	if result.StructuredContent != nil {
		data, err = json.Marshal(result.StructuredContent)
	} else if len(result.Content) == 1 {
		if text, ok := result.Content[0].(*mcp.TextContent); ok {
			data = []byte(text.Text)
		}
	}
	if err != nil || json.Unmarshal(data, &accounts) != nil || len(accounts.Accounts) == 0 {
		return 0, errors.New("Superhuman returned no usable linked accounts")
	}
	seen := make(map[string]bool)
	for _, account := range accounts.Accounts {
		if account.Email == "" || seen[account.Email] {
			return 0, errors.New("invalid Superhuman linked-account response")
		}
		seen[account.Email] = true
	}
	return len(accounts.Accounts), nil
}
