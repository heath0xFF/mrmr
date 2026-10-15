package superhuman

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// Login is the only interactive path. It creates an independent mrmr grant
// and never calls a mail tool. Only a successfully exchanged, refreshable grant
// replaces old credentials; it is saved even if later MCP initialization fails.
// showURL is a local human prompt, not a logger: authorization links should not
// be put into persistent service logs or shared with another person.
func Login(ctx context.Context, dir string, port int, showURL func(string) error) error {
	return login(ctx, dir, port, showURL, newHTTPClient())
}

func login(ctx context.Context, dir string, port int, showURL func(string) error, hc *http.Client) error {
	if port < 0 || port > 65535 || showURL == nil {
		return errors.New("invalid login callback configuration")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	store, err := openStore(dir)
	if err != nil {
		return err
	}
	defer store.lock.Close()
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return errors.New("cannot bind OAuth callback on loopback; choose another callback port")
	}
	defer listener.Close()
	redirect := "http://" + listener.Addr().String() + "/callback"
	saved := false
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		Client: hc, RedirectURL: redirect, RequestRefreshToken: true,
		// Accept a matching issuer even when discovery omits its support flag.
		// The SDK still rejects any mismatched issuer (RFC 9207).
		AcceptUnadvertisedIss: true,
		// Identity scopes are not a server-side read-only mail grant. Our fixed
		// tool calls enforce read-only behavior regardless of the consent scope.
		ScopeFilter: func([]string) []string { return []string{"openid", "email", "profile"} },
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName: "mrmr", RedirectURIs: []string{redirect},
				GrantTypes:    []string{"authorization_code", "refresh_token"},
				ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "none",
			},
		},
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			return receiveCode(ctx, listener, args.URL, showURL)
		},
		NewTokenSource: func(ctx context.Context, config *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
			record := credentialRecord{Version: 1, Resource: endpoint, Config: *config, Token: *token}
			if err := store.write(&record); err != nil {
				return nil, err
			}
			saved = true
			return &persistingTokens{ctx: ctx, store: store, record: record}, nil
		},
	})
	if err != nil {
		return safeError("prepare Superhuman authorization", err)
	}
	session, err := newClient().Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: hc, OAuthHandler: handler, DisableStandaloneSSE: true,
	}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		return safeError("Superhuman login", err)
	}
	defer session.Close()
	if !saved {
		return errors.New("Superhuman did not issue a refreshable OAuth grant")
	}
	return nil
}

func receiveCode(ctx context.Context, listener net.Listener, authURL string, showURL func(string) error) (*auth.AuthorizationResult, error) {
	u, err := url.Parse(authURL)
	if err != nil || !allowedURL(u) || u.Host != authHost || u.Query().Get("state") == "" {
		return nil, errors.New("unexpected Superhuman authorization URL")
	}
	results := make(chan *auth.AuthorizationResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", callbackHandler(listener.Addr().String(), u.Query().Get("state"), results))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	serveError := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		serveError <- server.Serve(listener)
	}()
	// The listener, server, and goroutine belong to this attempt. Cancellation,
	// denied consent, and an abandoned browser all close and join the server.
	defer func() { server.Close(); <-stopped }()
	if err := showURL(authURL); err != nil {
		return nil, errors.New("cannot display the Superhuman sign-in URL")
	}
	select {
	case result := <-results:
		if result.Code == "" {
			return nil, errors.New("Superhuman authorization was denied")
		}
		// The SDK verifies PKCE, state again, and the response issuer before
		// exchanging this code. No raw query or remote error is ever logged.
		return result, nil
	case <-serveError:
		return nil, errors.New("OAuth callback server stopped unexpectedly")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func callbackHandler(host, state string, results chan<- *auth.AuthorizationResult) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		q := r.URL.Query()
		// Reject stray local requests without consuming the real callback. The
		// state must match before either an error or a code ends this attempt.
		if r.Host != host || len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 ||
			len(q["code"]) > 1 || len(q["iss"]) > 1 || len(q["error"]) > 1 ||
			(q.Get("code") == "" && q.Get("error") == "") || (q.Get("code") != "" && q.Get("error") != "") {
			http.Error(w, "Invalid authorization callback.", http.StatusBadRequest)
			return
		}
		result := &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}
		select {
		case results <- result:
			fmt.Fprintln(w, "Authorization response received. Return to the mrmr terminal to check the result.")
		default:
			http.Error(w, "Authorization response already received.", http.StatusConflict)
		}
	}
}
