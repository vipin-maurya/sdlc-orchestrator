package server

// Live updates. Owner: W2-L.
//
// The hub is the one genuinely concurrent thing in this package: handlers
// register and unregister from many goroutines while the hub's own goroutine
// walks the client set. Every test here is written to be run with -race and
// with -count over 1; anything that passed only on the first iteration would be
// telling us about the fixture rather than about the hub.

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// startHub starts the hub with cadences a test can wait for, and stops it
// before the fixture's httptest.Server is closed — an SSE handler that is still
// in its select would otherwise hold ts.Close() open until the test binary's
// own timeout.
//
// Writing poll and ping before Start is what makes it safe to write them at
// all: the run goroutine reads them after the `go` statement, and nothing
// writes them again.
func startHub(e *env, poll, ping time.Duration) {
	e.t.Helper()
	e.srv.hub.poll, e.srv.hub.ping = poll, ping
	e.srv.Start()
	e.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		e.srv.Shutdown(ctx)
	})
}

type sseEvent struct {
	name string
	data string
}

// sseConn is one open stream, read by its own goroutine so a test can wait for
// an event with a deadline instead of blocking on a Read that may never return.
type sseConn struct {
	t      *testing.T
	res    *http.Response
	events chan sseEvent
	cancel context.CancelFunc
	closed chan struct{}
}

func (e *env) stream(path string) *sseConn {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.ts.URL+path, nil)
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	c := &sseConn{t: e.t, res: res, events: make(chan sseEvent, 64), cancel: cancel, closed: make(chan struct{})}
	e.t.Cleanup(c.close)
	go c.read()
	return c
}

func (c *sseConn) read() {
	defer close(c.closed)
	sc := bufio.NewScanner(c.res.Body)
	var ev sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		case line == "" && ev.name != "":
			select {
			case c.events <- ev:
			default: // a test that stopped reading is not a failure of the server
			}
			ev = sseEvent{}
		}
	}
}

// next waits for one event, failing the test rather than hanging when none
// arrives.
func (c *sseConn) next(within time.Duration) sseEvent {
	c.t.Helper()
	select {
	case ev := <-c.events:
		return ev
	case <-time.After(within):
		c.t.Fatalf("no SSE event within %s", within)
		return sseEvent{}
	}
}

// nextNamed skips events of other kinds, which is what a client that cares
// about one of the two event types does.
func (c *sseConn) nextNamed(name string, within time.Duration) sseEvent {
	c.t.Helper()
	deadline := time.Now().Add(within)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			c.t.Fatalf("no %q event within %s", name, within)
		}
		if ev := c.next(left); ev.name == name {
			return ev
		}
	}
}

func (c *sseConn) close() {
	c.cancel()
	c.res.Body.Close()
	<-c.closed
}

func decodeSnapshot(t *testing.T, data string) snapshot {
	t.Helper()
	var s snapshot
	if err := json.Unmarshal([]byte(data), &s); err != nil {
		t.Fatalf("decoding snapshot %q: %v", data, err)
	}
	return s
}

func ids(s snapshot) []string {
	out := make([]string, 0, len(s.Jobs))
	for _, j := range s.Jobs {
		out = append(out, j.ID)
	}
	return out
}

// TestSSEDeliversSnapshotOnChange covers both halves of the contract: a client
// is given the current list the moment it connects (so first paint and
// reconnect need nothing else), and a change to the job table produces a new
// full list rather than a description of what changed.
func TestSSEDeliversSnapshotOnChange(t *testing.T) {
	e := newEnv(t)
	first := e.job("BUILDING", "already here")
	startHub(e, 10*time.Millisecond, time.Hour)

	c := e.stream("/events/stream")
	if got := c.res.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := c.res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	opening := decodeSnapshot(t, c.nextNamed("jobs", 5*time.Second).data)
	if len(opening.Jobs) != 1 || opening.Jobs[0].ID != first.ID {
		t.Fatalf("opening snapshot carried %v, want just %s", ids(opening), first.ID)
	}
	if opening.Jobs[0].Title != "already here" || opening.Jobs[0].State != "BUILDING" {
		t.Errorf("opening snapshot job = %+v", opening.Jobs[0])
	}

	second := e.job("AWAITING_MERGE_APPROVAL", "new arrival")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("the new job never appeared in a snapshot")
		}
		snap := decodeSnapshot(t, c.nextNamed("jobs", time.Until(deadline)).data)
		if len(snap.Jobs) != 2 {
			continue
		}
		// The whole list, every time — a client that missed an event has
		// nothing to reconstruct.
		if snap.Jobs[0].ID != first.ID || snap.Jobs[1].ID != second.ID {
			t.Fatalf("snapshot carried %v, want both jobs", ids(snap))
		}
		if snap.Jobs[1].Gate != "merge" {
			t.Errorf("gate for AWAITING_MERGE_APPROVAL = %q, want merge", snap.Jobs[1].Gate)
		}
		return
	}
}

// TestSSEPingsOnAnIdleConnection is the one that matters most for §9.3: the
// ping is the only thing that distinguishes a quiet system from a dead
// connection, so it must arrive when the job table is doing nothing at all.
func TestSSEPingsOnAnIdleConnection(t *testing.T) {
	e := newEnv(t)
	// A poll interval far longer than the test: no job changes, so nothing but
	// the heartbeat can produce the event below.
	startHub(e, time.Hour, 10*time.Millisecond)

	c := e.stream("/events/stream")
	c.nextNamed("jobs", 5*time.Second) // the opening snapshot

	ev := c.nextNamed("ping", 5*time.Second)
	var p struct {
		Now string `json:"now"`
	}
	if err := json.Unmarshal([]byte(ev.data), &p); err != nil {
		t.Fatalf("decoding ping %q: %v", ev.data, err)
	}
	if _, err := time.Parse(time.RFC3339, p.Now); err != nil {
		t.Errorf("ping carried now=%q, which is not RFC3339: %v", p.Now, err)
	}
}

// TestSSENarrowsToOneJob covers `?job=`: the payload is one job, and the
// heartbeat is unaffected. A narrowed stream that lost its pings would be a
// job page that cannot tell it has gone stale, which is the state §9.3 exists
// to prevent.
func TestSSENarrowsToOneJob(t *testing.T) {
	e := newEnv(t)
	e.job("BUILDING", "not this one")
	want := e.job("AWAITING_MERGE_APPROVAL", "this one")
	startHub(e, 10*time.Millisecond, 10*time.Millisecond)

	c := e.stream("/events/stream?job=" + want.ID)
	snap := decodeSnapshot(t, c.nextNamed("jobs", 5*time.Second).data)
	if len(snap.Jobs) != 1 || snap.Jobs[0].ID != want.ID {
		t.Fatalf("narrowed snapshot carried %v, want just %s", ids(snap), want.ID)
	}
	c.nextNamed("ping", 5*time.Second)

	// An id nobody has is an empty list rather than the whole list: a page that
	// silently widened to every job would show the operator another job's state
	// under this job's heading.
	other := e.stream("/events/stream?job=NOPE-1")
	empty := decodeSnapshot(t, other.nextNamed("jobs", 5*time.Second).data)
	if len(empty.Jobs) != 0 {
		t.Fatalf("snapshot for an unknown id carried %v, want nothing", ids(empty))
	}
}

// TestSSERejectsThe33rdClient is spec §5.4's bound. The refusal is plain text
// and says why, because the operator's next question after "the page stopped
// updating" is exactly that.
func TestSSERejectsThe33rdClient(t *testing.T) {
	e := newEnv(t)
	startHub(e, time.Hour, time.Hour)

	conns := make([]*sseConn, 0, maxSSEClients)
	for i := 0; i < maxSSEClients; i++ {
		c := e.stream("/events/stream")
		if c.res.StatusCode != http.StatusOK {
			t.Fatalf("client %d was refused with %d", i+1, c.res.StatusCode)
		}
		conns = append(conns, c)
	}

	res := e.get("/events/stream")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("client %d got %d, want 503", maxSSEClients+1, res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("refusal Content-Type = %q, want text/plain", ct)
	}
	if body := e.body(res); !strings.Contains(body, "32") {
		t.Errorf("the refusal does not name the limit: %q", body)
	}

	// A slot is a slot only if it comes back. A stream that ended but still
	// counted would take the UI to permanently unusable after 32 reloads.
	conns[0].close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := e.get("/events/stream")
		if res.StatusCode == http.StatusOK {
			res.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot held by a closed stream was never released")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSSEDropsSlowClient exercises the back-pressure policy directly on the
// hub rather than through HTTP, because "a client that has stopped reading" is
// not something a socket reliably reproduces — the kernel's buffers absorb far
// more than this test would want to write.
//
// Two properties: the broadcast never blocks on the client that stopped
// reading, and the other client keeps receiving. The dropped client's channel
// is closed, which is what makes its handler return instead of leaking.
func TestSSEDropsSlowClient(t *testing.T) {
	e := newEnv(t)
	h := e.srv.hub

	slow := &client{ch: make(chan frame, clientDepth)}
	fast := &client{ch: make(chan frame, clientDepth)}
	for _, c := range []*client{slow, fast} {
		if _, refused := h.add(c); refused != "" {
			t.Fatalf("add: %s", refused)
		}
	}

	for i := 0; i < clientDepth+2; i++ {
		done := make(chan struct{})
		go func() {
			h.broadcast(frame{event: "ping", now: rfc3339(time.Now())})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("broadcast blocked on a client that had stopped reading")
		}
		select {
		case <-fast.ch:
		default:
			t.Fatalf("the reading client missed broadcast %d", i+1)
		}
	}

	// Everything buffered, and then a closed channel: the signal the handler
	// select waits on.
	for range clientDepth {
		select {
		case <-slow.ch:
		default:
			t.Fatal("the dropped client's buffer was not full")
		}
	}
	select {
	case _, ok := <-slow.ch:
		if ok {
			t.Fatal("the dropped client's channel carried more than its buffer")
		}
	case <-time.After(time.Second):
		t.Fatal("the dropped client's channel was never closed")
	}

	h.mu.Lock()
	_, slowStillThere := h.clients[slow]
	_, fastStillThere := h.clients[fast]
	h.mu.Unlock()
	if slowStillThere {
		t.Error("the slow client is still registered")
	}
	if !fastStillThere {
		t.Error("the client that kept up was dropped too")
	}
}

// TestHubStopsOnShutdown covers what TestShutdownIsClean cannot: a stream is a
// response that never ends on its own, so a handler sitting in its select is
// the goroutine most likely to outlive the process's intention to exit. After
// Shutdown the hub's goroutine is gone, every stream has ended, and no client
// is still registered.
func TestHubStopsOnShutdown(t *testing.T) {
	e := newEnv(t)
	startHub(e, 10*time.Millisecond, 10*time.Millisecond)

	conns := make([]*sseConn, 0, 3)
	for i := 0; i < 3; i++ {
		c := e.stream("/events/stream")
		c.nextNamed("jobs", 5*time.Second)
		conns = append(conns, c)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case <-e.srv.hub.done:
	default:
		t.Fatal("the hub goroutine is still running after Shutdown returned")
	}

	// Each handler returning ends its response, which the client sees as the
	// body reaching EOF — the observable form of "the goroutine is gone".
	for i, c := range conns {
		select {
		case <-c.closed:
		case <-time.After(5 * time.Second):
			t.Fatalf("stream %d was still open after Shutdown", i+1)
		}
	}

	h := e.srv.hub
	h.mu.Lock()
	n := len(h.clients)
	h.mu.Unlock()
	if n != 0 {
		t.Errorf("%d clients still registered after Shutdown", n)
	}

	// A tab that reconnects into a shut-down server is refused rather than
	// registered into a hub that will never send it anything.
	res := e.get("/events/stream")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a stream opened after Shutdown got %d, want 503", res.StatusCode)
	}
}

// TestSnapshotJSONFieldNames pins the wire contract app.js reads. A renamed
// field here does not break a build; it makes a column in the browser silently
// stop updating.
func TestSnapshotJSONFieldNames(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "pin the names")

	res := e.get("/api/jobs.json")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/jobs.json = %d", res.StatusCode)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var raw struct {
		Now  string           `json:"now"`
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(e.body(res)), &raw); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(raw.Jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(raw.Jobs))
	}
	if _, err := time.Parse(time.RFC3339, raw.Now); err != nil {
		t.Errorf("now=%q is not RFC3339: %v", raw.Now, err)
	}
	for _, field := range []string{"id", "target", "state", "gate", "title", "since", "updated_at", "counters"} {
		if _, ok := raw.Jobs[0][field]; !ok {
			t.Errorf("snapshot job has no %q field: %v", field, raw.Jobs[0])
		}
	}
	if raw.Jobs[0]["gate"] != "merge" || raw.Jobs[0]["id"] != j.ID {
		t.Errorf("snapshot job = %v", raw.Jobs[0])
	}
	if _, err := time.Parse(time.RFC3339, raw.Jobs[0]["since"].(string)); err != nil {
		t.Errorf("since=%v is not RFC3339: %v", raw.Jobs[0]["since"], err)
	}
}

// TestJobJSONCarriesProgress covers the one field the list snapshot leaves out.
// It is here rather than in the stream because that is where it is filled: a
// progress lookup for every job every second is a cost the list does not pay.
func TestJobJSONCarriesProgress(t *testing.T) {
	e := newEnv(t)
	j := e.job("BUILDING", "working")
	if err := e.st.AddEvent(&store.Event{
		JobID: j.ID, State: "BUILDING", Kind: "progress",
		Detail: `{"state":"BUILDING","last":"editing internal/foo.go"}`,
	}); err != nil {
		t.Fatal(err)
	}

	res := e.get("/api/jobs/" + j.ID + ".json")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/jobs/%s.json = %d", j.ID, res.StatusCode)
	}
	snap := decodeSnapshot(t, e.body(res))
	if len(snap.Jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(snap.Jobs))
	}
	if snap.Jobs[0].Progress != "editing internal/foo.go" {
		t.Errorf("progress = %q, want the agent's last action line", snap.Jobs[0].Progress)
	}
	if _, err := time.Parse(time.RFC3339, snap.Jobs[0].ProgressAt); err != nil {
		t.Errorf("progress_at=%q is not RFC3339: %v", snap.Jobs[0].ProgressAt, err)
	}

	// A job that does not exist is a plain-text 404, not the HTML error page:
	// this route is the one meant to be scripted against.
	missing := e.get("/api/jobs/NOPE-1.json")
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("GET a missing job = %d, want 404", missing.StatusCode)
	}
	if ct := missing.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("404 Content-Type = %q, want text/plain", ct)
	}
}
