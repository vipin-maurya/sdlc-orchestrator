// Package server is the local web UI behind `sdlc serve`.
//
// It is an additional caller of the packages the CLI already uses, not a second
// implementation of anything: it reads the same store, renders gates through
// the same internal/review, and writes the same store.Approval rows that `sdlc
// approve` writes. internal/engine is not modified and is not imported for
// anything but its list of resumable states.
//
// The trust boundary is the machine. There is no authentication, no users and
// no roles, because a loopback listener on the operator's own workstation is
// the same boundary the CLI has. Everything that makes that boundary hold is in
// safety.go, and the reasoning is there rather than here.
package server

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/diff"
	"github.com/vipinm/sdlc-orchestrator/internal/review"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// maxFormBytes bounds every POST body (spec §5.4). No form this UI serves is
// anywhere near it; it exists so a request that never stops sending cannot hold
// a goroutine and a buffer open indefinitely.
const maxFormBytes = 1 << 20

// readHeaderTimeout bounds the one phase of a request that WriteTimeout cannot
// cover here (see Serve).
const readHeaderTimeout = 10 * time.Second

type Server struct {
	cfg     *config.Config
	st      *store.Store
	log     *log.Logger // never nil
	verbose bool

	// tpl holds one parsed set per page. html/template keeps one namespace per
	// set, so several pages each defining "content" cannot share a set —
	// parsing them together silently leaves whichever file parsed last.
	tpl map[string]*template.Template

	assets map[string]string // "app.css" -> "/static/app.<hash>.css"
	served map[string]servedAsset

	hub *hub // live.go

	// httpSrv is built here rather than by the caller because §5.4's bounds
	// are properties of this server, not of whoever binds the port.
	httpSrv *http.Server

	mu    sync.Mutex // guards diffs
	diffs []diffEntry
}

// New parses the templates and hashes the assets up front: a template that does
// not parse must stop `sdlc serve` from starting, not surface as a 500 on the
// one page nobody opened until a gate was waiting.
func New(cfg *config.Config, st *store.Store, lg *log.Logger, verbose bool) (*Server, error) {
	if lg == nil {
		lg = log.New(os.Stderr, "", log.LstdFlags)
	}
	s := &Server{cfg: cfg, st: st, log: lg, verbose: verbose}

	names, served, err := buildAssets()
	if err != nil {
		return nil, err
	}
	s.assets, s.served = names, served

	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	s.hub = newHub(s)
	s.httpSrv = &http.Server{
		Handler: s.Handler(),
		// Bounds a client that opens a connection and dribbles headers.
		ReadHeaderTimeout: readHeaderTimeout,
		// Deliberately 0. Any finite WriteTimeout is a deadline on the whole
		// response, and /events/stream is a response that is meant to stay open
		// for as long as the tab is: a finite value would kill every SSE stream
		// on a fixed schedule and present as "the live pill goes amber every N
		// seconds". Slowloris is bounded by ReadHeaderTimeout and
		// MaxBytesReader instead, which bound the parts an attacker controls.
		WriteTimeout: 0,
		ErrorLog:     lg,
	}
	return s, nil
}

// parseTemplates builds one set per page from a shared base.
//
// A map of sets rather than one set for everything: html/template has a single
// namespace per set, so two pages that both define "content" collide, and the
// collision is silent — whichever file parsed last wins and the other page
// renders someone else's body.
func (s *Server) parseTemplates() error {
	ents, err := fs.ReadDir(templateFS, "templates")
	if err != nil {
		return err
	}
	// Partials are parsed into every set: base.html is the layout and the
	// underscore-prefixed files hold named templates the pages invoke.
	var partials, pages []string
	for _, e := range ents {
		n := e.Name()
		switch {
		case e.IsDir() || !strings.HasSuffix(n, ".html"):
		case n == "base.html" || strings.HasPrefix(n, "_"):
			partials = append(partials, "templates/"+n)
		default:
			pages = append(pages, n)
		}
	}
	sort.Strings(partials)
	base, err := template.New("base.html").Funcs(s.funcMap()).ParseFS(templateFS, partials...)
	if err != nil {
		return fmt.Errorf("parsing shared templates: %w", err)
	}
	s.tpl = make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		clone, err := base.Clone()
		if err != nil {
			return err
		}
		t, err := clone.ParseFS(templateFS, "templates/"+p)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", p, err)
		}
		s.tpl[p] = t
	}
	return nil
}

// funcMap is the whole of what a template may call. Every function here
// returns a plain string, so every value a template prints is escaped by
// html/template. None of them returns one of the pre-escaped types — an escape
// hatch that exists is an escape hatch that eventually gets applied to a
// finding description an agent wrote, and TestHouseRules pins their count at
// zero for the whole package.
func (s *Server) funcMap() template.FuncMap {
	return template.FuncMap{
		"asset":   func(name string) string { return s.assets[name] },
		"rfc3339": rfc3339,
		"short":   shortSHA,
		"gateFor": review.GateFor,
		// gateClass and stateClass return a class name from a closed set
		// (shape.go), never the value they were given. That is the whole
		// point of them: a template that built a class out of a state string
		// would let whatever wrote that string choose which of this UI's
		// styles it wears.
		"gateClass":  gateClass,
		"stateClass": stateClass,
		"sevRank":    func(sev string) int { return artifact.SeverityRank[sev] },
		"base":       path.Base,
		"add":        func(a, b int) int { return a + b },
	}
}

// rfc3339 renders a timestamp for the browser to localise. Always UTC, because
// the operator may be reading this through `ssh -L` from another timezone, and
// "" for the zero time so a template can test it.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// shortSHA is the template's name for review.Short. The truncation lives there
// because the gate documents this UI renders are review's own, and a page that
// abbreviated a SHA to a different width than the document beside it would be
// two answers to one question.
func shortSHA(sha string) string { return review.Short(sha) }

// Handler registers every route in spec §5 and wraps them in the middleware
// chain. It is called once, from New; it is exported because the tests build a
// httptest.Server from it without binding a port.
//
// The chain is recoverPanic -> checkHost -> withCSRFCookie -> logRequest, in
// that order, and the order is load-bearing: the Host check must run before any
// handler and before the CSRF cookie is minted, so a rebinding attempt is
// refused without being handed a token.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// --- 5.1 pages -------------------------------------------------------
	// {$} rather than a bare "/" so the root pattern matches only the root:
	// "/" in ServeMux is a subtree match, which would swallow every
	// unregistered path and answer it with a redirect instead of a 404.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/jobs", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /jobs", s.handleJobs)
	mux.HandleFunc("GET /jobs/{id}", s.handleJob)
	mux.HandleFunc("GET /jobs/{id}/gate", s.handleGate)
	mux.HandleFunc("GET /jobs/{id}/diff", s.handleDiff)
	mux.HandleFunc("GET /jobs/{id}/diff/{index}", s.handleDiffFile)
	mux.HandleFunc("GET /jobs/{id}/diff.patch", s.handleDiffPatch)
	mux.HandleFunc("GET /jobs/{id}/events", s.handleEvents)
	mux.HandleFunc("GET /jobs/{id}/logs", s.handleLogs)
	mux.HandleFunc("GET /jobs/{id}/logs/{name}", s.handleLog)
	mux.HandleFunc("GET /jobs/{id}/artifacts", s.handleArtifacts)
	mux.HandleFunc("GET /jobs/{id}/artifacts/{name}", s.handleArtifact)
	mux.HandleFunc("GET /jobs/{id}/prompts/{name}", s.handlePrompt)
	mux.HandleFunc("GET /submit", s.handleSubmitForm)
	mux.HandleFunc("GET /config", s.handleConfig)
	mux.HandleFunc("GET "+staticPrefix+"{path...}", s.serveStatic)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})

	// --- 5.2 actions -----------------------------------------------------
	// Every decision route runs requireCSRF then requireFreshState before it
	// can reach the store, and neither is optional: the first stops another
	// origin from posting, the second stops this origin from posting a
	// decision about a job that has since moved (spec §10.3).
	mux.HandleFunc("POST /jobs/{id}/approve", s.decision(s.handleApprove))
	mux.HandleFunc("POST /jobs/{id}/reject", s.decision(s.handleReject))
	mux.HandleFunc("POST /jobs/{id}/resume", s.decision(s.handleResume))
	mux.HandleFunc("POST /jobs/{id}/cancel", s.decision(s.handleCancel))
	// Submit creates a job rather than deciding one, and the view preference
	// writes a cookie: neither has a job state to be stale against, so neither
	// takes requireFreshState.
	mux.HandleFunc("POST /submit", s.requireCSRF(s.handleSubmit))
	mux.HandleFunc("POST /prefs/diff-view", s.requireCSRF(s.handleDiffViewPref))

	// --- 5.3 live and data ----------------------------------------------
	mux.HandleFunc("GET /events/stream", s.handleStream)
	mux.HandleFunc("GET /api/jobs.json", s.handleJobsJSON)
	// ServeMux wildcards span a whole path segment, so "{id}.json" is not a
	// legal pattern (it panics at registration). The segment is matched whole
	// and the suffix stripped here, so the URL spec §5.3 pins is served
	// exactly and the handler still reads {id} like every other job route.
	mux.HandleFunc("GET /api/jobs/{idjson}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := strings.CutSuffix(r.PathValue("idjson"), ".json")
		if !ok || id == "" {
			s.fail(w, r, http.StatusNotFound, "no such page")
			return
		}
		r.SetPathValue("id", id)
		s.handleJobJSON(w, r)
	})

	return s.recoverPanic(s.checkHost(s.withCSRFCookie(s.logRequest(limitBody(mux)))))
}

// decision is the guard stack every POST that writes an approval row goes
// through. Composing it once here is what keeps a later handler from being
// registered with one of the two checks missing.
func (s *Server) decision(h freshHandler) http.HandlerFunc {
	return s.requireCSRF(s.requireFreshState(h))
}

// limitBody caps every POST body at 1 MiB (spec §5.4). It is applied here, once
// around the mux, rather than in each handler: a bound that each of six owners
// has to remember to apply is a bound that will be missing from one of them.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// logRequest is quiet unless --v: the console `sdlc serve` shares with the
// operator is also where a panic and a bind failure appear, and a line per
// asset request would bury them.
func (s *Server) logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.verbose {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		s.log.Printf("%s %s %s", r.Method, r.URL.RequestURI(), time.Since(start).Round(time.Millisecond))
	})
}

// --- lifecycle -----------------------------------------------------------

// Start starts the background work — the live hub, and nothing else. It is
// separate from Serve so a test can drive an httptest.Server and still get
// stream events.
func (s *Server) Start() { s.hub.start() }

// Serve starts the background work and then serves on ln until Shutdown. The
// caller must pass a listener it opened on config.NormalizeListen's return
// value: validating one address string and binding another is how a routable
// address gets bound, which for a server with no authentication publishes an
// approve button on the network.
func (s *Server) Serve(ln net.Listener) error {
	s.Start()
	return s.httpSrv.Serve(ln)
}

// Shutdown stops accepting, waits out ctx for in-flight requests, and then
// stops the hub. In that order: an approval POST that is halfway through
// writing its row must finish, and the hub's goroutine must not outlive the
// process's intention to exit.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.httpSrv.Shutdown(ctx)
	s.hub.stop()
	return err
}

// --- shared helpers ------------------------------------------------------

type pageData struct {
	Title string
	Nav   string // "jobs" | "submit" | "config"
	// Layout is the class base.html puts on <main>: "" for a document page,
	// "review" for the three-pane gate. It is a field rather than something
	// the content template sets because the element it lands on belongs to
	// base.html, and a page reaching up into the layout to widen itself is how
	// two pages end up disagreeing about the shell they share.
	Layout   string
	CSRF     string
	EngineUp bool // orchestrator.lock_file exists
	Assets   map[string]string
	Now      time.Time
	Body     any
}

// wide returns the page with a layout class set. Written as a method on the
// value so a handler reads s.page(...).wide("review") in one expression.
func (p pageData) wide(layout string) pageData {
	p.Layout = layout
	return p
}

// page wraps a per-page model in everything base.html needs.
func (s *Server) page(r *http.Request, title, nav string, body any) pageData {
	return pageData{
		Title:    title,
		Nav:      nav,
		CSRF:     csrfToken(r),
		EngineUp: s.engineUp(),
		Assets:   s.assets,
		Now:      time.Now(),
		Body:     body,
	}
}

// engineUp is inferred from the lock file and is deliberately weak: acquireLock
// removes a stale lock at startup, so an absent lock after a crash is
// ambiguous. The banner's wording hedges to match; anything more confident
// would be a claim this cannot support (spec §13.8).
func (s *Server) engineUp() bool {
	p := s.cfg.Orchestrator.LockFile
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// render executes a page into a buffer before writing a byte of it: a template
// that fails halfway would otherwise leave a 200 with half a page on it, which
// looks like a rendering quirk rather than the error it is.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, body any) {
	s.renderStatus(w, r, http.StatusOK, page, body)
}

func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, code int, page string, body any) {
	t, ok := s.tpl[page]
	if !ok {
		s.log.Printf("no template %q", page)
		http.Error(w, "500 internal error — see the sdlc serve console", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base.html", body); err != nil {
		s.log.Printf("rendering %s for %s: %v", page, r.URL.Path, err)
		http.Error(w, "500 internal error — see the sdlc serve console", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	w.Write(buf.Bytes())
}

// fail renders error.html with a status. Never used for a decision route:
// those answer 303 (AC-15).
func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	body := struct {
		Code   int
		Status string
		Msg    string
	}{code, http.StatusText(code), msg}
	s.renderStatus(w, r, code, "error.html", s.page(r, http.StatusText(code), "", body))
}

// job resolves {id} or answers 404 and reports false.
func (s *Server) job(w http.ResponseWriter, r *http.Request) (*store.Job, bool) {
	id := r.PathValue("id")
	j, err := s.st.GetJob(id)
	if err != nil || j == nil {
		// One message for "no such job" and for a read error: the id came from
		// a URL, so the overwhelmingly likely cause is a typo or a job that was
		// never created, and a database error is on the console either way.
		if err != nil {
			s.log.Printf("reading job %q: %v", id, err)
		}
		s.fail(w, r, http.StatusNotFound, fmt.Sprintf("no job %q", id))
		return nil, false
	}
	return j, true
}

// target resolves the job's target or answers 500 and reports false.
func (s *Server) target(w http.ResponseWriter, r *http.Request, j *store.Job) (config.Target, bool) {
	t, err := s.cfg.Target(j.Target)
	if err != nil {
		// 500 rather than 404: the job is real and the config is what is
		// wrong, which is the operator's to fix and not the URL's.
		s.fail(w, r, http.StatusInternalServerError, fmt.Sprintf(
			"job %s names target %q, which the loaded config does not define", j.ID, j.Target))
		return config.Target{}, false
	}
	return t, true
}

// --- parsed-diff cache ---------------------------------------------------

type diffEntry struct {
	key   string // jobID + "\x00" + HeadSHA + "\x00" + BaseSHA + "\x00" + strconv.Itoa(len(patch))
	patch string
	files []*diff.File
}

// maxDiffEntries is small on purpose: a parsed 4 MiB patch is not cheap to
// hold, and nobody reads five patches at once.
const maxDiffEntries = 4

// diffFor returns the parsed patch, parsing at most once per (job, head, base,
// length). The head sha is in the key so a cached diff can never outlive the
// commit it described: a stale diff at a merge gate is the one cache bug that
// would actually cost something. At most 4 entries, least-recently-used first
// out. Nothing else in this server is cached.
func (s *Server) diffFor(j *store.Job, patch string) ([]*diff.File, error) {
	key := j.ID + "\x00" + j.HeadSHA + "\x00" + j.Counters.BaseSHA + "\x00" + strconv.Itoa(len(patch))
	s.mu.Lock()
	for i := range s.diffs {
		if s.diffs[i].key != key || s.diffs[i].patch != patch {
			continue
		}
		e := s.diffs[i]
		// Move to the front so "least recently used" means what it says.
		s.diffs = append(s.diffs[:i], s.diffs[i+1:]...)
		s.diffs = append([]diffEntry{e}, s.diffs...)
		s.mu.Unlock()
		return e.files, nil
	}
	s.mu.Unlock()

	// Parsed outside the lock: a 4 MiB patch takes long enough that holding the
	// mutex across it would stall every other page in the tab.
	files, err := diff.Parse(patch)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.diffs = append([]diffEntry{{key: key, patch: patch, files: files}}, s.diffs...)
	if len(s.diffs) > maxDiffEntries {
		s.diffs = s.diffs[:maxDiffEntries]
	}
	s.mu.Unlock()
	return files, nil
}
