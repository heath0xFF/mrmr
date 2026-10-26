package runtime

import (
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
}

type pollHealth struct {
	mu     sync.RWMutex
	states map[string]PollHealth
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

func (r *Runtime) PollHealth(name string) PollHealth {
	r.polls.mu.RLock()
	defer r.polls.mu.RUnlock()
	return r.polls.states[name]
}
