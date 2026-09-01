// Command mrmr is the single binary for the mrmr runtime. `run` loads a YAML
// config, opens SQLite, and serves POST /api/events. Each request runs the full
// pipeline synchronously — persist event, interpret via the configured
// model, evaluate policy, execute the outcome — so the HTTP response is the
// trace of everything that happened. Synchronous processing is the point:
// it makes the runtime's behavior explainable end to end and keeps the
// invariants simple (no queue, no worker goroutines, no lost requests) at
// the cost of latency, which is fine for ambient event volumes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/heath0xff/mrmr/internal/config"
	"github.com/heath0xff/mrmr/internal/event"
	"github.com/heath0xff/mrmr/internal/model"
	"github.com/heath0xff/mrmr/internal/runtime"
	"github.com/heath0xff/mrmr/internal/storage"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mrmr <run|eval> [options]")
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
	case "-h", "--help", "help":
		fmt.Println("usage: mrmr <run|eval> [options]")
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
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/events", eventsHandler(rt))

	srv := &http.Server{Addr: cfg.Server.Addr, Handler: mux}

	// Cancel the context on SIGINT/SIGTERM so Shutdown has a deadline and a
	// second signal still hard-exits.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	log.Printf("mrmr listening on %s (db: %s, model: %s)", cfg.Server.Addr, cfg.DB.Path, cfg.Interpret.Model)

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

// eventsRequest is the wire shape for POST /api/events. Only type and source
// are required; data and metadata are opaque maps passed through to the event.
type eventsRequest struct {
	Type     string         `json:"type"`
	Source   string         `json:"source"`
	Subject  string         `json:"subject,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// eventsHandler returns the POST /api/events handler. Ingest is synchronous:
// by the time the 200 goes out, the event is persisted, interpreted, routed
// by policy, and the outcome executed. Ingest errors are persistence
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
