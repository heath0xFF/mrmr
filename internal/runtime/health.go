package runtime

import (
	"strings"
	"sync"
	"time"
)

// PollHealth is process-local evidence, not the journal cursor's age. An empty
// successful poll is healthy too; a restart must not inherit an old green state.
type PollHealth struct {
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	LastSuccess time.Time `json:"last_success"`
	Running     bool      `json:"running"`
	Failed      bool      `json:"failed"`
	// Counters answer "is this watcher reading anything?", which poll success
	// cannot: an empty successful poll looks the same as a poll of the wrong thing.
	// They are process-local and restart at zero with every Runtime.
	Polls        int64 `json:"polls"`
	Scanned      int64 `json:"scanned"`
	Watched      int64 `json:"watched"`
	Selected     int64 `json:"selected"`
	LastScanned  int   `json:"last_scanned"`
	LastWatched  int   `json:"last_watched"`
	LastSelected int   `json:"last_selected"`
	// Backlogged means the last poll filled its batch, so more records are waiting.
	Backlogged bool `json:"backlogged"`
}

// PollCounts is what one poll saw. Scanned is everything the source read, Watched
// is what belonged to its selection, and Selected is what became an Event.
type PollCounts struct {
	Scanned, Watched, Selected int
	Backlogged                 bool
}

// SeenRecord is a record from a watched unit, selected or not. It exists so an
// operator can see below-threshold lines that are deliberately never stored.
type SeenRecord struct {
	At       time.Time `json:"at"`
	Unit     string    `json:"unit"`
	Priority int       `json:"priority"`
	Message  string    `json:"message"`
	Selected bool      `json:"selected"`
}

// These bounds cap memory per source. Message text may be sensitive, so it stays
// in this ring only: never written to SQLite, logs, or traces, and gone on restart.
const (
	seenRecordLimit  = 25
	seenMessageLimit = 1024
)

type pollHealth struct {
	mu     sync.RWMutex
	states map[string]PollHealth
	seen   map[string][]SeenRecord
}

// StartPoll and FinishPoll are called by each source's serial polling loop.
// The lock protects snapshots only, never journal reads or model requests.
func (r *Runtime) StartPoll(name string) {
	r.polls.mu.Lock()
	defer r.polls.mu.Unlock()
	if r.polls.states == nil {
		r.polls.states = make(map[string]PollHealth)
	}
	s := r.polls.states[name]
	s.StartedAt, s.Running = time.Now().UTC(), true
	r.polls.states[name] = s
}

func (r *Runtime) FinishPoll(name string, err error) {
	r.polls.mu.Lock()
	defer r.polls.mu.Unlock()
	s := r.polls.states[name]
	s.FinishedAt, s.Running, s.Failed = time.Now().UTC(), false, err != nil
	if err == nil {
		s.LastSuccess = s.FinishedAt
	}
	// Do not retain raw source errors: URLs and response text may be private.
	r.polls.states[name] = s
}

// RecordPoll adds one poll's counts. Sources call it on every exit path, so a poll
// that fails partway still reports the records it did read.
func (r *Runtime) RecordPoll(name string, c PollCounts) {
	r.polls.mu.Lock()
	defer r.polls.mu.Unlock()
	if r.polls.states == nil {
		r.polls.states = make(map[string]PollHealth)
	}
	s := r.polls.states[name]
	s.Polls++
	s.Scanned += int64(c.Scanned)
	s.Watched += int64(c.Watched)
	s.Selected += int64(c.Selected)
	s.LastScanned, s.LastWatched, s.LastSelected = c.Scanned, c.Watched, c.Selected
	s.Backlogged = c.Backlogged
	r.polls.states[name] = s
}

// RecordSeen keeps the newest records for the live view. The caller must only pass
// records from watched units: this must not become a copy of the whole journal.
func (r *Runtime) RecordSeen(name string, record SeenRecord) {
	if len(record.Message) > seenMessageLimit {
		record.Message = strings.ToValidUTF8(record.Message[:seenMessageLimit], "")
	}
	r.polls.mu.Lock()
	defer r.polls.mu.Unlock()
	if r.polls.seen == nil {
		r.polls.seen = make(map[string][]SeenRecord)
	}
	records := append(r.polls.seen[name], record)
	if len(records) > seenRecordLimit {
		// Copy rather than reslice so dropped message text can be collected.
		records = append([]SeenRecord(nil), records[len(records)-seenRecordLimit:]...)
	}
	r.polls.seen[name] = records
}

// RecentSeen returns a copy, newest first, and never nil so JSON stays an array.
func (r *Runtime) RecentSeen(name string) []SeenRecord {
	r.polls.mu.RLock()
	defer r.polls.mu.RUnlock()
	records := r.polls.seen[name]
	out := make([]SeenRecord, 0, len(records))
	for i := len(records) - 1; i >= 0; i-- {
		out = append(out, records[i])
	}
	return out
}

func (r *Runtime) PollHealth(name string) PollHealth {
	r.polls.mu.RLock()
	defer r.polls.mu.RUnlock()
	return r.polls.states[name]
}
