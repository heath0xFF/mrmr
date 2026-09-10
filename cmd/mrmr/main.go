// Command mrmr is the single binary for the mrmr runtime. `run` loads a YAML
// config, opens SQLite, and serves POST /api/events. Each request is
// processed synchronously: the event is persisted; duplicates short-circuit
// there with a "duplicate" marker; and the rest run deterministic filters,
// model interpretation, policy, and outcome in order, with filter-excluded
// events stopping before the model call. The HTTP response is the trace of
// everything that happened. Synchronous processing is the point: it makes
// the runtime's behavior explainable end to end and keeps the invariants
// simple (no queue, no worker goroutines, no lost requests) at the cost of
// latency, which is fine for ambient event volumes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/runtime"
	"github.com/heath0xff/mrmr/internal/source"
	"github.com/heath0xff/mrmr/internal/storage"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mrmr <run|eval|events|label|dataset|inspect> [options]")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "run":
		if err := run(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "eval":
		if err := eval(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "events":
		if err := events(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "label":
		if err := label(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "dataset":
		if err := dataset(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "inspect":
		if err := inspect(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "-h", "--help", "help":
		fmt.Println("usage: mrmr <run|eval|events|label|dataset|inspect> [options]")
	default:
		log.Fatalf("unknown subcommand %q", os.Args[1])
	}
}

// run boots the server and blocks until SIGINT/SIGTERM. Shutdown is graceful
// because an in-flight event is mid-pipeline: killing it between the event
// persist and the decision persist would leave a half-processed event with
// no way to tell from outside whether anything was lost.
func run(args []string) error {
	fs := flag.NewFlagSet("mrmr run", flag.ContinueOnError)
	configPath := fs.String("config", "mrmr.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	db, err := storage.Open(cfg.DB.Path)
	if err != nil {
		return err
	}
	defer db.Close()

	rt := &runtime.Runtime{
		DB:       db,
		Client:   &model.Client{},
		ModelCfg: cfg.Models[cfg.Interpret.Model],
		ModelKey: cfg.Interpret.Model,
		Prompt:   cfg.Interpret.Prompt,
		Schema:   cfg.Interpret.Schema,
		Policy:   cfg.Policy,
		Filters:  cfg.Filter,
	}
	// Resolve delegation endpoints once at startup; policy rules reference
	// agents by name only.
	rt.AgentEndpoints = make(map[string]string, len(cfg.Agents))
	for name, a := range cfg.Agents {
		rt.AgentEndpoints[name] = a.Endpoint
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/events", eventsHandler(rt))

	srv := &http.Server{Addr: cfg.Server.Addr, Handler: mux}

	// Cancel the context on SIGINT/SIGTERM so Shutdown has a deadline and a
	// second signal still hard-exits. Pollers share this context: shutdown
	// cancels in-flight fetches and stops the tick loops.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Prepare every source before starting any: an inaccessible journal or
	// missing token must not leave earlier sources ingesting on failed boot.
	runners, err := prepareSources(ctx, cfg.Sources, db)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	defer srv.Close()

	// Every exit path cancels and joins sources before closing SQLite,
	// including a server failure. Each adapter bounds its own I/O lifetime.
	var sources sync.WaitGroup
	defer func() {
		stop()
		sources.Wait()
	}()
	for _, runSource := range runners {
		sources.Add(1)
		go func() {
			defer sources.Done()
			runSource(ctx, rt)
		}()
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(listener) }()

	log.Printf("mrmr listening on %s (db: %s, model: %s, sources: %d)", cfg.Server.Addr, cfg.DB.Path, cfg.Interpret.Model, len(cfg.Sources))

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Bound shutdown so a hung request can't wedge the process; the pipeline
	// itself also respects ctx via the request context.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// Method values keep the two real source adapters explicit without a plugin
// registry. Construction can check local prerequisites but never starts a
// polling goroutine; ownership transfers to run only after every check passes.
func prepareSources(ctx context.Context, configs []config.Source, db *storage.DB) ([]func(context.Context, *runtime.Runtime), error) {
	var runners []func(context.Context, *runtime.Runtime)
	for _, sc := range configs {
		switch sc.Type {
		case "http-poller":
			p, err := source.NewHTTPPoller(sc)
			if err != nil {
				return nil, err
			}
			runners = append(runners, p.Run)
		case "systemd-journal":
			j, err := source.NewJournal(ctx, sc, db)
			if err != nil {
				return nil, err
			}
			runners = append(runners, j.Run)
		default:
			return nil, fmt.Errorf("source %q: unknown type %q", sc.Name, sc.Type)
		}
	}
	return runners, nil
}

// eventsRequest is the wire shape for POST /api/events. Only type and source
// are required; data and metadata are opaque maps passed through to the event.
type eventsRequest struct {
	Type     string         `json:"type"`
	Source   string         `json:"source"`
	Subject  string         `json:"subject,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// eventsHandler returns the POST /api/events handler. Ingest is
// synchronous: by the time the 200 goes out, the event's fate is final:
// a duplicate, a filter exclusion before any model call, or an executed
// policy outcome. Ingest errors are persistence
// failures and map to 500; everything the model does wrong is already
// captured in the decision and trace, not in the HTTP status.
func eventsHandler(rt *runtime.Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// This is the runtime's only external ingress, so bodies are bounded:
		// a normalized event is small, and one oversized POST must not be able
		// to eat memory. 1 MiB is generous headroom over any real event.
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req eventsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
			return
		}
		if req.Type == "" || req.Source == "" {
			writeError(w, http.StatusBadRequest, "both \"type\" and \"source\" are required")
			return
		}

		e := event.Event{
			ID:        event.NewID("evt_"),
			Type:      req.Type,
			Source:    req.Source,
			Subject:   req.Subject,
			Timestamp: time.Now().UTC(),
			Data:      req.Data,
			Metadata:  req.Metadata,
		}

		resp, err := rt.Ingest(r.Context(), e)
		if err != nil {
			// Persistence failed, which means the event's fate is unknown
			// to the caller; 500 (not 4xx) so clients may retry.
			log.Printf("ingest %s failed: %v", e.ID, err)
			writeError(w, http.StatusInternalServerError, "ingest failed")
			return
		}

		// One line per event so stdout doubles as a processing log. This is
		// the whole observability story for milestone 1: the response JSON
		// contains the decision and the full trace.
		b, err := json.Marshal(resp)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "encode response: "+err.Error())
			return
		}
		fmt.Println(string(b))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(b)
	}
}

// writeError responds with the {"error": ...} envelope. Errors never carry
// internal detail beyond what the trace already exposes.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
