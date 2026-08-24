package server

// The shared fixture. Owned by W2-G and pinned here so all six W2.1 owners
// test against one environment rather than six subtly different ones. It
// follows newReviewEnv (internal/cli/review_test.go): a temp data dir, a
// config.Default() with one target, a real store, and jobs inserted directly.

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/store"

	_ "modernc.org/sqlite"
)

type env struct {
	t    *testing.T
	tmp  string
	cfg  *config.Config // a snapshot: live.Get() at fixture construction time
	live *config.Live
	st   *store.Store
	srv  *Server
	ts   *httptest.Server

	// csrf is the token the fixture's cookie jar holds, captured from the
	// first response. post() sends it in both halves of the double-submit
	// pair; postRaw() sends neither.
	csrf string
}

// fixtureConfigYAML is what newEnv writes to disk and loads through
// config.Load — the real pipeline `sdlc serve` uses, not a struct built in
// memory and handed to New directly, because the config edit surface under
// test reads and writes s.cfg.Get().Path and Live.Apply needs a real file to
// back up and rewrite. Only what differs from config.Default() is spelled
// out, the same minimal-target shape internal/config's own tests use:
// build.commands and unit_test.command are the two fields Validate requires
// non-empty on a target, and the commands themselves never run.
const fixtureConfigYAML = `
orchestrator:
  data_dir: %s
  lock_file: %s
database:
  path: %s
targets:
  demo:
    repo_path: %s
    default_branch: main
    branch_prefix: sdlc/
    build: { commands: [["true"]] }
    unit_test: { command: ["true"] }
    ship: { command: ["scripts", "ship.sh"] }
`

func newEnv(t *testing.T) *env {
	t.Helper()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	raw := fmt.Sprintf(fixtureConfigYAML,
		dataDir,
		filepath.Join(dataDir, "engine.lock"),
		filepath.Join(dataDir, "sdlc.db"),
		filepath.Join(tmp, "repo"))
	path := filepath.Join(tmp, "sdlc.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(loaded.Database.Path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	live := config.NewLive(loaded)

	// A logger to io.Discard: a panic test writes a stack, and a test run whose
	// output is mostly stack traces hides the failures.
	srv, err := New(live, st, log.New(io.Discard, "", 0), false)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	e := &env{t: t, tmp: tmp, cfg: loaded, live: live, st: st, srv: srv, ts: ts}
	e.csrf = e.freshToken()
	return e
}

// freshToken makes one request purely to be handed a CSRF cookie, which is
// exactly what a browser does when it loads the form page.
func (e *env) freshToken() string {
	e.t.Helper()
	res := e.get("/healthz")
	for _, c := range res.Cookies() {
		if c.Name == csrfCookie {
			return c.Value
		}
	}
	e.t.Fatal("no CSRF cookie was set")
	return ""
}

// job inserts a job already sitting in state.
func (e *env) job(state, title string) *store.Job {
	e.t.Helper()
	j := e.newJobRow(state, title)
	e.layout(j)
	return j
}

// newJobRow inserts the database row and nothing else.
func (e *env) newJobRow(state, title string) *store.Job {
	e.t.Helper()
	id, err := e.st.NextJobID("JOB")
	if err != nil {
		e.t.Fatal(err)
	}
	j := &store.Job{
		ID: id, Target: "demo", IssueTitle: title, IssueBody: "do the thing",
		Branch: "sdlc/" + id, WorktreePath: filepath.Join(e.tmp, "repo", ".worktrees", id),
		State: state, StateEnteredAt: time.Now().Add(-90 * time.Minute),
	}
	if err := e.st.CreateJob(j); err != nil {
		e.t.Fatal(err)
	}
	if state != "CREATED" {
		j.State = state
		if err := e.st.UpdateJob(j); err != nil {
			e.t.Fatal(err)
		}
	}
	return j
}

// bareJob is a job row with nothing on disk: a job that has just been created,
// or one whose data directory was cleaned up underneath it. The pages have to
// stay readable in that state, so one helper produces it deliberately rather
// than tests depending on job() having happened not to write anything.
func (e *env) bareJob(state, title string) *store.Job {
	e.t.Helper()
	return e.newJobRow(state, title)
}

// layout writes the on-disk directories and one sample file in each.
//
// It is part of the default fixture rather than an opt-in because safeName
// resolves a {name} route by membership in the directory listing: a job with no
// directories makes every logs/artifacts/prompts request a truthful 404, which
// reads in a route test as "the route is not registered" — which is exactly how
// three registered routes came to look unregistered.
func (e *env) layout(j *store.Job) {
	e.t.Helper()
	if err := artifact.EnsureLayout(e.cfg.Orchestrator.DataDir, j.ID, j.IssueTitle, j.IssueBody); err != nil {
		e.t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(artifact.LogsDir(e.cfg.Orchestrator.DataDir, j.ID), "build.log"):      "building\ndone\n",
		filepath.Join(artifact.ArtifactsDir(e.cfg.Orchestrator.DataDir, j.ID), "spec.json"): `{"schema":"spec/1","summary":"a spec"}`,
		filepath.Join(artifact.PromptsDir(e.cfg.Orchestrator.DataDir, j.ID), "planning.md"): "plan the thing\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
}

// do sends req without following redirects: a 303 is the assertion in several
// tests, and a client that chases it reports the destination's status instead.
func (e *env) do(req *http.Request) *http.Response {
	e.t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { res.Body.Close() })
	return res
}

func (e *env) get(path string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.ts.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.do(req)
}

// post sends both halves of the double-submit pair, which is what a real form
// submission from a page this server rendered looks like.
func (e *env) post(path string, form url.Values) *http.Response {
	e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", e.csrf)
	req := e.formRequest(path, form)
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: e.csrf})
	return e.do(req)
}

// postRaw sends neither half. Everything a cross-origin page can arrange looks
// like this, which is why it has its own helper rather than being spelled out
// per test.
func (e *env) postRaw(path string, form url.Values) *http.Response {
	e.t.Helper()
	return e.do(e.formRequest(path, form))
}

func (e *env) formRequest(path string, form url.Values) *http.Request {
	e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// getHost sends a GET with a chosen Host header, which is how a rebinding
// attempt looks from the server's side.
func (e *env) getHost(path, host string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.ts.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	// net/http reads the Host header from this field, not from the header map.
	req.Host = host
	return e.do(req)
}

// rawRequest writes bytes straight onto the connection and returns everything
// that comes back. net/http's client will not send a request with no Host at
// all, and "no Host" is a case the allowlist has to answer for.
func (e *env) rawRequest(raw string) string {
	e.t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(e.ts.URL, "http://"))
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		e.t.Fatal(err)
	}
	if _, err := io.WriteString(conn, raw); err != nil {
		e.t.Fatal(err)
	}
	out, err := io.ReadAll(conn)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(out)
}

func (e *env) body(res *http.Response) string {
	e.t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

type approvalRow struct {
	jobID    string
	gate     string
	decision string
	reason   string
	note     string
	cancel   bool
}

// approvals reads every row the approvals table holds, consumed or not. The
// store exposes only "the next pending row for one gate", and what the guard
// tests assert is a counting property: a refused POST must leave zero rows,
// whatever gate it named.
func (e *env) approvals() []approvalRow {
	e.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(e.cfg.Database.Path))
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT job_id,gate,decision,reason,note,cancel FROM approvals ORDER BY id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var got []approvalRow
	for rows.Next() {
		var r approvalRow
		var cancel int
		if err := rows.Scan(&r.jobID, &r.gate, &r.decision, &r.reason, &r.note, &cancel); err != nil {
			e.t.Fatal(err)
		}
		r.cancel = cancel != 0
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return got
}

// engineLock creates the lock file so the no-engine banner is suppressed. The
// engine infers liveness from this file's presence and so does the UI.
func (e *env) engineLock() {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.cfg.Orchestrator.LockFile), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.cfg.Orchestrator.LockFile, []byte("1"), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// configHistoryEntry writes a file directly into this env's config history
// directory and returns its name, for tests that need one real, resolvable
// entry without going through a live edit (config.Live.Apply's own tests, in
// internal/config, already cover that backups are written correctly — what
// this package needs is just a fixture the history routes can serve).
func (e *env) configHistoryEntry(name, content string) string {
	e.t.Helper()
	dir := configHistoryDir(e.cfg.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return name
}
