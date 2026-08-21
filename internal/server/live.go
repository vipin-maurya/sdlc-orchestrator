package server

// Live updates: the SSE hub and the snapshot JSON (spec §9). Owner: W2-L.
//
// Created by the skeleton commit with a hub that starts and stops and
// broadcasts nothing. The lifecycle is real even in the skeleton because
// server.go's Start and Shutdown call into it, and a Shutdown that leaves a
// goroutine running is the kind of defect that is much cheaper to have a test
// for from the first commit than to find later under -race.

import (
	"net/http"
	"sync"

	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// jobSnapshot's field names are the contract app.js reads. They are pinned in
// the plan's §2.7 and W2-L fills them; nothing serialises this yet.
type jobSnapshot struct {
	ID          string         `json:"id"`
	Target      string         `json:"target"`
	State       string         `json:"state"`
	Gate        string         `json:"gate"`
	Title       string         `json:"title"`
	Since       string         `json:"since"`      // RFC3339 UTC, StateEnteredAt
	UpdatedAt   string         `json:"updated_at"` // RFC3339 UTC
	HoldReason  string         `json:"hold_reason,omitempty"`
	ResumeAfter string         `json:"resume_after,omitempty"`
	Counters    store.Counters `json:"counters"`
	Progress    string         `json:"progress,omitempty"`
	ProgressAt  string         `json:"progress_at,omitempty"`
}

type snapshot struct {
	Jobs []jobSnapshot `json:"jobs"`
	Now  string        `json:"now"`
}

type hub struct {
	srv  *Server
	quit chan struct{}
	done chan struct{}
	once sync.Once
}

func newHub(s *Server) *hub {
	return &hub{srv: s, quit: make(chan struct{}), done: make(chan struct{})}
}

// start runs the hub's single goroutine. W2-L replaces the body of run with the
// 1s digest and the 15s heartbeat.
func (h *hub) start() {
	h.once.Do(func() { go h.run() })
}

func (h *hub) run() {
	defer close(h.done)
	<-h.quit
}

// stop closes the hub and waits for its goroutine, so Shutdown returning means
// the goroutine is gone rather than merely asked to go.
func (h *hub) stop() {
	h.once.Do(func() { close(h.done) }) // never started: nothing to wait for
	select {
	case <-h.quit:
	default:
		close(h.quit)
	}
	<-h.done
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request)   { stub(w, "W2-L") }
func (s *Server) handleJobsJSON(w http.ResponseWriter, r *http.Request) { stub(w, "W2-L") }
func (s *Server) handleJobJSON(w http.ResponseWriter, r *http.Request)  { stub(w, "W2-L") }
