package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/dashboard"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/notify"
	"github.com/heath0xff/mrmr/internal/runtime"
	"github.com/heath0xff/mrmr/internal/storage"
)

// A snapshot owns one immutable configuration. HTTP requests retain their
// snapshot until completion; activation never edits a runtime in place.
type managedSnapshot struct {
	cfg      *config.Config
	rt       *runtime.Runtime
	view     http.Handler
	ingress  http.Handler
	yaml     string
	revision string
	jobs     []sourceJob
}

type manager struct {
	ctx       context.Context
	db        *storage.DB
	path      string
	bootstrap *config.Config
	discord   *notify.DiscordCredentials
	current   atomic.Pointer[managedSnapshot]
	blocked   atomic.Bool
	// Only one operator mutation or activation may proceed at a time. Reads
	// stay available while a source drains or a model evaluation is running.
	op          sync.Mutex
	token       [32]byte
	host        string
	stopSources chan struct{}
	sources     sync.WaitGroup
	evalMu      sync.Mutex
	evalCancel  context.CancelFunc
	evalWG      sync.WaitGroup
}

func revision(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func runManaged(path, addr, host, tokenEnv string, setup bool, setupDB string) error {
	ip, port, err := net.SplitHostPort(addr)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || portNumber < 1 || portNumber > 65535 || net.ParseIP(ip) == nil || !net.ParseIP(ip).IsLoopback() {
		return fmt.Errorf("control-addr must be a literal loopback address with a nonzero port")
	}
	if host != "" {
		if strings.ContainsAny(host, "/:@ \\?#") || strings.ToLower(host) != host {
			return fmt.Errorf("control-host must be an exact lowercase hostname")
		}
	}
	token := os.Getenv(tokenEnv)
	if len(token) < 32 {
		return fmt.Errorf("management requires a random token of at least 32 bytes in its configured environment variable")
	}
	b, err := os.ReadFile(path)
	var cfg *config.Config
	if errors.Is(err, os.ErrNotExist) && setup {
		cfg = &config.Config{Server: config.Server{Addr: "127.0.0.1:4242"}, DB: config.DB{Path: setupDB}}
	} else if err != nil {
		return fmt.Errorf("read startup configuration: %w", err)
	} else {
		// Reject credential-bearing files before diagnostics or history copies.
		cfg, err = managedParse(string(b))
		if err != nil {
			return err
		}
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("managed config must not be a symlink")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect managed config: %w", err)
	}
	if cfg.DB.Path == "" || cfg.DB.Path == ":memory:" || strings.HasPrefix(cfg.DB.Path, "file:") || strings.Contains(cfg.DB.Path, "?") {
		return fmt.Errorf("management requires a persistent filesystem database path")
	}
	lock, err := lockRuntimeDB(cfg.DB.Path)
	if err != nil {
		return err
	}
	defer lock.Close()
	db, err := storage.Open(cfg.DB.Path)
	if err != nil {
		return err
	}
	defer db.Close()
	// SQLite may have created WAL files before an older database's mode was
	// tightened. Protect those files as well as the main database.
	for _, file := range []string{cfg.DB.Path, cfg.DB.Path + "-wal", cfg.DB.Path + "-shm"} {
		if err := os.Chmod(file, 0600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("protect management database: %w", err)
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	credentials := notify.DefaultDiscordCredentials()
	m := &manager{ctx: ctx, db: db, path: path, bootstrap: cfg, discord: credentials, host: host, token: sha256.Sum256([]byte(token))}
	blocked, err := db.EffectsBlocked()
	if err != nil {
		return err
	}
	m.blocked.Store(blocked)
	// Crashed evaluations are evidence of interrupted work, never jobs to replay.
	if _, err = db.Exec(`UPDATE control_evaluations SET status='interrupted' WHERE status='running'`); err != nil {
		return err
	}
	if len(b) > 0 {
		snap := m.snapshot(cfg, string(b))
		jobs, err := m.prepare(ctx, cfg)
		if err != nil {
			return err
		}
		if _, err = db.Exec(`INSERT INTO control_revisions(id,yaml,created_at,status,base) VALUES(?,?,?,'active','') ON CONFLICT(id) DO UPDATE SET status='active'`, snap.revision, snap.yaml, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		snap.jobs = jobs
		m.current.Store(snap)
	}
	// All listeners are bound before serving. A failed bind cannot leave an
	// unattended management endpoint or source goroutine behind.
	defer func() {
		cancel()
		m.op.Lock()
		defer m.op.Unlock()
		m.sources.Wait()
		m.cancelEvaluation()
		m.evalWG.Wait()
	}()
	controlListener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer controlListener.Close()
	ingressListener, err := net.Listen("tcp", cfg.Server.Addr)
	if err != nil {
		return err
	}
	defer ingressListener.Close()
	view := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := m.current.Load()
		if s == nil {
			http.Error(w, "Setup is not complete.", 503)
			return
		}
		s.view.ServeHTTP(w, r)
	})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/events", func(w http.ResponseWriter, r *http.Request) {
		s := m.current.Load()
		if s == nil {
			http.Error(w, "Collection is disabled until setup completes.", 503)
			return
		}
		s.ingress.ServeHTTP(w, r)
	})
	mux.Handle("/dashboard/", view)
	mux.Handle("/dashboard", view)
	mux.Handle("GET /{$}", view)
	servers := []*http.Server{{Handler: m, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: time.Minute}, {Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: time.Minute}}
	listeners := []net.Listener{controlListener, ingressListener}
	if cfg.Server.DashboardAddr != "" {
		l, err := net.Listen("tcp", cfg.Server.DashboardAddr)
		if err != nil {
			return err
		}
		defer l.Close()
		listeners = append(listeners, l)
		servers = append(servers, &http.Server{Handler: view, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute})
	}
	if snap := m.current.Load(); snap != nil {
		m.startSources(snap.jobs, snap.rt)
	}
	errs := make(chan error, len(servers))
	var serving, requests sync.WaitGroup
	var admission sync.Mutex
	accepting := true
	for i, s := range servers {
		handler := s.Handler
		s.BaseContext = func(net.Listener) context.Context { return ctx }
		s.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			admission.Lock()
			if !accepting {
				admission.Unlock()
				http.Error(w, "Runtime is stopping.", 503)
				return
			}
			requests.Add(1)
			admission.Unlock()
			defer requests.Done()
			handler.ServeHTTP(w, r)
		})
		serving.Add(1)
		go func() { defer serving.Done(); errs <- s.Serve(listeners[i]) }()
	}
	defer func() {
		// Serve returning does not mean its handlers have returned. Close
		// admission, cancel connections, and join handlers before closing SQLite.
		admission.Lock()
		accepting = false
		admission.Unlock()
		for _, s := range servers {
			s.Close()
		}
		serving.Wait()
		requests.Wait()
	}()
	fmt.Printf("mrmr management listening on %s (private operator access required)\n", addr)
	select {
	case <-ctx.Done():
	case err = <-errs:
	}
	cancel()
	deadline, done := context.WithTimeout(context.Background(), 15*time.Second)
	defer done()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	for _, s := range servers {
		if shutdownErr := s.Shutdown(deadline); shutdownErr != nil {
			err = errors.Join(err, fmt.Errorf("shutdown HTTP listener: %w", shutdownErr))
		}
	}
	return err
}

func (m *manager) snapshot(cfg *config.Config, text string) *managedSnapshot {
	rt := &runtime.Runtime{DiscordCredentials: m.discord, DB: m.db, Client: &model.Client{}, ModelCfg: cfg.Models[cfg.Interpret.Model], ModelKey: cfg.Interpret.Model, Prompt: cfg.Interpret.Prompt, Schema: cfg.Interpret.Schema, Questions: cfg.Interpret.Questions, Policy: cfg.Policy, Filters: cfg.Filter, Agents: cfg.Agents, Models: cfg.Models, InterpretByType: cfg.InterpretByType, Monitors: cfg.Monitors, EffectsBlocked: m.blocked.Load, ConfigRevision: revision(text)}
	return &managedSnapshot{cfg: cfg, rt: rt, view: dashboard.New(rt, cfg), ingress: eventsHandler(rt), yaml: text, revision: revision(text)}
}

func (m *manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	host := r.Host
	if h, _, e := net.SplitHostPort(host); e == nil {
		host = h
	}
	local := net.ParseIP(peer) != nil && net.ParseIP(peer).IsLoopback()
	if err != nil || !local || (host != "localhost" && host != "127.0.0.1" && host != "::1" && (m.host == "" || host != m.host)) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "Private management access required.", 403)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, e := url.Parse(origin)
		if e != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
			http.Error(w, "Origin rejected.", 403)
			return
		}
	}
	if r.Method == "GET" && (r.URL.Path == "/manage/" || r.URL.Path == "/manage/manage.js" || r.URL.Path == "/manage/manage-setup.js" || r.URL.Path == "/manage/manage-monitors.js" || r.URL.Path == "/manage/manage.css" || r.URL.Path == "/manage/signal.css" || r.URL.Path == "/manage/signal.js") {
		dashboard.ManagementAsset(w, r)
		return
	}
	if r.Method == "GET" && (r.URL.Path == "/" || r.URL.Path == "/manage") {
		http.Redirect(w, r, "/manage/", 307)
		return
	}
	// The browser retains the token only in memory, not cookies, URLs, storage,
	// or service logs. An explicit header prevents ambient browser authority.
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	hash := sha256.Sum256([]byte(auth))
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(hash[:], m.token[:]) != 1 {
		http.Error(w, "Operator token required.", 401)
		return
	}
	if r.Method != "GET" && r.Method != "POST" {
		http.Error(w, "Method not allowed.", 405)
		return
	}
	if r.Method == "POST" && (r.Header.Get("X-Mrmr-Control") != "1" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")) {
		http.Error(w, "Explicit JSON control request required.", 403)
		return
	}
	// Remote browser enrollment must use HTTPS at the trusted private proxy.
	// Loopback HTTP remains available for local use and SSH tunnels.
	if r.Method == "POST" && r.URL.Path == "/manage/api/discord" && host != "localhost" && host != "127.0.0.1" && host != "::1" && r.Header.Get("Origin") != "https://"+r.Host {
		http.Error(w, "Use private HTTPS or localhost to save credentials.", 403)
		return
	}
	// The management origin can read existing evidence, but never receives the
	// ingestion mux. Even authenticated callers cannot submit arbitrary events.
	if strings.HasPrefix(r.URL.Path, "/dashboard/") {
		s := m.current.Load()
		if s == nil {
			http.Error(w, "Setup is not complete.", 503)
			return
		}
		clone := r.Clone(r.Context())
		clone.Host = "localhost"
		clone.Header = r.Header.Clone()
		clone.Header.Del("Origin")
		s.view.ServeHTTP(w, clone)
		return
	}
	if r.Method == "POST" && r.URL.Path != "/manage/api/shadow" {
		if !m.op.TryLock() {
			http.Error(w, "Another change is in progress. Retry shortly.", 409)
			return
		}
		defer m.op.Unlock()
	}
	if err := m.route(w, r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func decodeControl(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("Invalid or oversized JSON request.")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("Expected one JSON object.")
	}
	return nil
}
func controlJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}
func (m *manager) audit(operation, target, result string) error {
	_, err := m.db.Exec(`INSERT INTO control_audit(at,actor,operation,target,result) VALUES(?,'operator',?,?,?)`, time.Now().UTC().Format(time.RFC3339Nano), operation, target, result)
	return err
}

// Atomic replacement prevents a crash from leaving truncated startup YAML.
// A private temp file is synced before rename; the containing directory is
// synced too. Callers compare the disk revision immediately before this call.
func replaceConfig(path, text string) (bool, error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".mrmr-config-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(text); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return false, err
	}
	// Rename already committed the visible file. Report durability uncertainty
	// separately so the caller does not keep running a different revision.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return true, err
	}
	defer dir.Close()
	return true, dir.Sync()
}
