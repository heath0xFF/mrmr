// Package event defines the two records that flow through mrmr's pipeline:
// the Event (what happened, normalized) and the Decision (what the model
// thought it meant). These types are intentionally boring and dependency-
// free — every stage of the runtime handles them, so any cleverness here
// would be paid for everywhere. data and metadata are opaque maps: the
// core makes no assumptions about what a source puts in them, because
// assuming is how source-specific logic leaks into the runtime.
package event

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Event is the normalized record of something that happened. Sources are
// dumb by design: they detect and emit, they never interpret. The runtime
// stamps ID and Timestamp at ingestion — a source's clock is not trusted,
// though a source may carry its original time inside data or metadata if
// the event itself needs it.
type Event struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Source    string         `json:"source"`
	Subject   string         `json:"subject,omitempty"`
	Timestamp time.Time      `json:"timestamp"` // event time, UTC
	Data      map[string]any `json:"data,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Decision is the validated result of interpreting an Event. Status is the
// axis the rest of the runtime cares about: "ok" decisions reach policy;
// "invalid" (schema failure after retry) and "errored" (endpoint failure)
// both route to ignore. Decisions persist even when they fail, because "the
// model was down at 3am" is a question worth answering from the audit trail.
type Decision struct {
	ID          string         `json:"id"`
	EventID     string         `json:"event_id"`
	Interpreter string         `json:"interpreter"` // model config name
	Model       string         `json:"model"`       // actual model id sent to endpoint
	Status      string         `json:"status"`
	Result      map[string]any `json:"result,omitempty"`
	LatencyMs   int64          `json:"latency_ms"`
	Error       string         `json:"error,omitempty"`
}

// NewID returns a sortable unique id: prefix + hex(unix-millis + random).
// ponytail: hand-rolled instead of a ULID dep; swap if cross-system ordering matters.
func NewID(prefix string) string {
	var b [10]byte
	ms := time.Now().UnixMilli()
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	if _, err := rand.Read(b[6:]); err != nil {
		panic(fmt.Sprintf("new id: %v", err))
	}
	return prefix + hex.EncodeToString(b[:])
}
