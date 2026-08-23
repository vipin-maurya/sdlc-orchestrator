package server

// Live updates: the SSE hub and the snapshot JSON (spec §9). Owner: W2-L.
//
// One goroutine reads the job table on a fixed cadence and fans the result out
// to every connected tab. The alternative — each tab polling — makes N database
// reads per interval instead of one, and gives a tab whose fetches are all
// failing no way to tell itself apart from a tab where nothing is happening.
// The connection is what carries liveness here: the 15s ping is the signal
// app.js watches, and its absence is what turns "this page is stale" from an
// invisible state into a visible one.
//
// Everything broadcast is a full snapshot, never a delta. A delta stream that
// drops one message leaves the page quietly wrong for as long as the tab is
// open, and there is no way for the client to notice; a snapshot repaints
// correctly after any gap. That is what makes dropping a slow client a safe
// thing to do rather than a data-loss event — the whole back-pressure policy
// below rests on it.

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/review"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// jobSnapshot's field names are the contract app.js reads, pinned in the
// plan's §2.7. Renaming one here without renaming it there does not break a
// build — it makes a field silently absent in the browser — so they are worth
// treating as an interface rather than as struct tags.
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

// Bounds and cadences. maxSSEClients is spec §5.4's; the rest are §9.2's.
const (
	maxSSEClients = 32
	pollEvery     = time.Second
	pingEvery     = 15 * time.Second
	// clientDepth is deliberately shallow. It is enough to absorb a client
	// that is briefly busy and not enough to hide one that has stopped
	// reading: a deep buffer would let a dead tab sit there consuming memory
	// while looking healthy, and the snapshot it eventually got would be a
	// stale one it had no reason to distrust.
	clientDepth = 4
)

// frame is one thing to send. The jobs frame carries the snapshot rather than
// marshalled bytes because `?job=` narrows the payload per client, so the
// marshalling happens in each client's own goroutine. The snapshot a frame
// points at is never modified after the frame is built — that immutability is
// what makes sharing one pointer across every client goroutine safe.
type frame struct {
	event string // "jobs" | "ping"
	snap  *snapshot
	now   string // ping payload
}

// client is one open /events/stream response.
type client struct {
	ch  chan frame
	job string // "" for the whole list, else the one id this client asked for
}

type hub struct {
	srv  *Server
	quit chan struct{}
	done chan struct{}
	once sync.Once

	// poll and ping are fields rather than constants so a test can drive both
	// cadences without waiting fifteen real seconds. newHub sets them to the
	// spec's values and only a test writes them, before start; the run
	// goroutine reads them after, so the `go` statement orders the two.
	poll time.Duration
	ping time.Duration

	mu      sync.Mutex
	clients map[*client]struct{}
	closed  bool
	// last is the most recent snapshot, handed to a client the moment it
	// connects so a new tab does not stare at a server-rendered page for a
	// second before the stream says anything.
	last *snapshot

	// digest and lastErr are touched only by the run goroutine.
	digest  string
	lastErr string
}

func newHub(s *Server) *hub {
	return &hub{
		srv:  s,
		quit: make(chan struct{}),
		done: make(chan struct{}),
		poll: pollEvery,
		ping: pingEvery,
	}
}

// start runs the hub's single goroutine.
func (h *hub) start() {
	h.once.Do(func() { go h.run() })
}

func (h *hub) run() {
	defer close(h.done)
	poll := time.NewTicker(h.poll)
	defer poll.Stop()
	ping := time.NewTicker(h.ping)
	defer ping.Stop()
	// One read before the first tick, so `last` is populated for whoever
	// connects in the first second.
	h.read()
	for {
		select {
		case <-h.quit:
			return
		case <-poll.C:
			h.read()
		case <-ping.C:
			// Unconditional, and not merged with the poll: the ping is the
			// only thing that proves the connection is alive, so it must not
			// be conditional on anything the database did or did not do.
			h.broadcast(frame{event: "ping", now: rfc3339(time.Now())})
		}
	}
}

// read takes one snapshot and broadcasts it if the job table moved.
//
// The digest covers exactly the fields spec §9.2 names — not the snapshot's
// `now`, which changes every tick and would make every tick a broadcast.
func (h *hub) read() {
	snap, err := h.srv.buildSnapshot()
	if err != nil {
		// A store that is failing fails every second, and a console filled
		// with one line per second buries the panic and the bind failure that
		// share it. Log a repeat only when the error itself changes.
		if err.Error() != h.lastErr {
			h.lastErr = err.Error()
			h.srv.log.Printf("live: reading jobs: %v", err)
		}
		return
	}
	h.lastErr = ""
	d := digestJobs(snap.Jobs)
	changed := d != h.digest
	h.digest = d

	h.mu.Lock()
	h.last = snap
	h.mu.Unlock()

	if changed {
		h.broadcast(frame{event: "jobs", snap: snap})
	}
}

// broadcast hands the frame to every client that can take it and drops every
// client that cannot.
//
// The send is non-blocking, so one wedged tab can never stall the hub or the
// other tabs, and a dropped client leaks nothing: its channel is closed, which
// is how its handler goroutine learns to return. Dropping is safe precisely
// because these are snapshots — the tab reconnects and the next one repaints
// it correctly.
func (h *hub) broadcast(f frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.ch <- f:
		default:
			delete(h.clients, c)
			close(c.ch)
		}
	}
}

// add registers a client and returns the snapshot to open its stream with. The
// second return is "" on success and otherwise the plain-text reason to refuse
// with, because "we are at the limit" and "we are shutting down" are the same
// 503 to a browser and very different things to the operator reading the page.
func (h *hub) add(c *client) (*snapshot, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, "the server is shutting down"
	}
	if len(h.clients) >= maxSSEClients {
		return nil, fmt.Sprintf("%d live connections is the limit", maxSSEClients)
	}
	if h.clients == nil {
		h.clients = make(map[*client]struct{}, maxSSEClients)
	}
	h.clients[c] = struct{}{}
	return h.last, ""
}

// remove unregisters a client whose request ended. Closing the channel here
// rather than in the handler keeps one rule for the whole file: whoever takes a
// client out of the map closes its channel, under the mutex, and a send only
// ever happens to a client still in the map.
func (h *hub) remove(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.ch)
	}
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
	// And then the clients. An SSE response is by design one that never ends
	// on its own, so a stream handler would otherwise sit in its select until
	// the tab at the other end closed — which is exactly the goroutine that
	// must not outlive Shutdown.
	h.closeAll()
}

func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for c := range h.clients {
		delete(h.clients, c)
		close(c.ch)
	}
}

// --- snapshots -----------------------------------------------------------

// buildSnapshot renders the job table into the shape app.js consumes.
//
// It carries no progress line: that costs one event query per job, and this
// runs once a second for every job in the table. The single-job routes fill it
// in, where it is one query for one job that somebody is actually looking at.
func (s *Server) buildSnapshot() (*snapshot, error) {
	jobs, err := s.st.ListJobs()
	if err != nil {
		return nil, err
	}
	// Never nil: a nil slice marshals to `null`, and app.js would have to
	// defend against it on every event.
	out := &snapshot{Jobs: make([]jobSnapshot, 0, len(jobs)), Now: rfc3339(time.Now())}
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, snapshotOf(j))
	}
	return out, nil
}

func snapshotOf(j *store.Job) jobSnapshot {
	return jobSnapshot{
		ID:          j.ID,
		Target:      j.Target,
		State:       j.State,
		Gate:        review.GateFor(j.State),
		Title:       j.IssueTitle,
		Since:       rfc3339(j.StateEnteredAt),
		UpdatedAt:   rfc3339(j.UpdatedAt),
		HoldReason:  j.HoldReason,
		ResumeAfter: rfc3339(j.ResumeAfter),
		Counters:    j.Counters,
	}
}

// withProgress attaches the newest progress event, which is the one line that
// distinguishes a state that is working from one that is hung.
//
// A failure to read the events is not a failure of the snapshot: the job's
// state is the part being asked for, and losing the progress line is worth
// less than losing the whole response.
func (s *Server) withProgress(js *jobSnapshot) {
	events, err := s.st.ListEvents(js.ID)
	if err != nil {
		s.log.Printf("live: reading events for %s: %v", js.ID, err)
		return
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Kind != "progress" {
			continue
		}
		js.Progress = progressLine(e.Detail)
		js.ProgressAt = rfc3339(e.CreatedAt)
		return
	}
}

// progressLine picks the human-readable half of a progress event's detail.
//
// The engine writes several shapes of progress detail; the one produced from
// agent output carries the condensed action line under "last", and that is the
// sentence an operator wants. Everything else is handed over as the compact
// JSON it already is rather than being reformatted into a claim about what the
// engine meant.
func progressLine(detail string) string {
	if detail == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(detail), &m); err != nil {
		return detail
	}
	if s, ok := m["last"].(string); ok && s != "" {
		return s
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(detail)); err != nil {
		return detail
	}
	return buf.String()
}

// digestJobs covers (id, state, updated_at, hold_reason, resume_after,
// counters) — spec §9.2's list, and nothing that moves on its own.
func digestJobs(jobs []jobSnapshot) string {
	h := sha256.New()
	for _, j := range jobs {
		counters, err := json.Marshal(j.Counters)
		if err != nil {
			// Unreachable for a struct of scalars, and a digest that silently
			// dropped a field would be a change nobody was ever told about.
			counters = []byte(err.Error())
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%s\n",
			j.ID, j.State, j.UpdatedAt, j.HoldReason, j.ResumeAfter, counters)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// narrow applies `?job=`. It keeps the snapshot's shape rather than returning a
// bare job, so app.js has one payload shape to handle and one code path to
// apply it with.
func narrow(snap *snapshot, id string) *snapshot {
	if id == "" {
		return snap
	}
	out := &snapshot{Jobs: make([]jobSnapshot, 0, 1), Now: snap.Now}
	for _, j := range snap.Jobs {
		if j.ID == id {
			out.Jobs = append(out.Jobs, j)
		}
	}
	return out
}

// --- handlers ------------------------------------------------------------

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Nothing in this binary's chain removes Flusher; if something ever
		// does, an SSE response that is buffered forever looks exactly like a
		// hung server, so say which it is.
		plainError(w, http.StatusInternalServerError, "this response cannot be streamed")
		return
	}

	c := &client{ch: make(chan frame, clientDepth), job: r.URL.Query().Get("job")}
	snap, refused := s.hub.add(c)
	if refused != "" {
		// Plain text, and named: the operator's next question is "why did the
		// page stop updating", and a bare 503 does not answer it.
		w.Header().Set("Retry-After", "5")
		plainError(w, http.StatusServiceUnavailable, "no live connection available — "+refused+
			".\nThe pages still work; reload to see current state.")
		return
	}
	defer s.hub.remove(c)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// Not for this server, which has no proxy in front of it by design, but for
	// the `ssh -L` and reverse-proxy setups operators build anyway: a proxy
	// that buffers this response turns live updates into no updates.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// The opening snapshot. The hub's is preferred because it costs no query;
	// it is nil only when nothing has started the hub, and a stream that sent
	// nothing at all would be indistinguishable from a hung one.
	if snap == nil {
		built, err := s.buildSnapshot()
		if err != nil {
			s.log.Printf("live: opening stream: %v", err)
			return
		}
		snap = built
	}
	if !writeFrame(w, flusher, c.job, frame{event: "jobs", snap: snap}) {
		return
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-c.ch:
			if !ok {
				// Dropped for falling behind, or the hub stopped. Either way
				// this response is over; the browser reconnects and the next
				// snapshot repaints it.
				return
			}
			if !writeFrame(w, flusher, c.job, f) {
				return
			}
		}
	}
}

// writeFrame writes one SSE event and reports whether the connection is still
// usable.
//
// The payload goes out as a single `data:` line because encoding/json escapes
// every newline it encodes — a job title with a line break in it cannot split
// the frame, which for a field an issue author controls is the difference
// between a rendering quirk and an injected event.
func writeFrame(w http.ResponseWriter, flusher http.Flusher, job string, f frame) bool {
	var payload any
	if f.event == "ping" {
		payload = struct {
			Now string `json:"now"`
		}{f.now}
	} else {
		payload = narrow(f.snap, job)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.event, b); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func (s *Server) handleJobsJSON(w http.ResponseWriter, r *http.Request) {
	snap, err := s.buildSnapshot()
	if err != nil {
		s.log.Printf("live: %s: %v", r.URL.Path, err)
		plainError(w, http.StatusInternalServerError, "could not read the job list — see the sdlc serve console")
		return
	}
	writeJSON(w, snap)
}

func (s *Server) handleJobJSON(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j, err := s.st.GetJob(id)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && j == nil):
		// Plain text rather than the HTML error page the pages use: this route
		// exists to be scripted against, and a script that gets a styled 404
		// page where it expected JSON reports something misleading. A missing
		// job is separated from a broken read here, unlike on the HTML pages,
		// for the same reason: 404 and 500 are the two answers a caller can act
		// on differently.
		plainError(w, http.StatusNotFound, "no job "+id)
		return
	case err != nil:
		s.log.Printf("live: reading job %q: %v", id, err)
		plainError(w, http.StatusInternalServerError, "could not read job "+id+" — see the sdlc serve console")
		return
	}
	js := snapshotOf(j)
	s.withProgress(&js)
	writeJSON(w, &snapshot{Jobs: []jobSnapshot{js}, Now: rfc3339(time.Now())})
}

// writeJSON marshals before writing a byte, for the same reason render does:
// a failure halfway through leaves a 200 carrying half a document.
func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		plainError(w, http.StatusInternalServerError, "could not encode the response")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// No-store, not no-cache: these carry job state, and a snapshot served from
	// a back/forward cache is a freshness claim this feature exists to avoid.
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

// plainError is the error shape for the live and data routes. The HTML fail()
// belongs to the pages; a stream and a JSON document are not read by anything
// that wants a rendered page back.
func plainError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, "%d %s — %s\n", code, http.StatusText(code), msg)
}
