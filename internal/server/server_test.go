package server

// Tests for the route table, the templates and the lifecycle. Owner: W2-G.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// route is one line of spec §5. The table is written out longhand rather than
// derived from the mux, because deriving it would assert only that the code
// agrees with itself: what is under test is that the code agrees with the
// specification.
type route struct {
	method string
	path   string
}

// specRoutes is every route in spec §5.1, §5.2 and §5.3. {id} is JOB-1,
// {name}/{index} are values the fixture can resolve or that a stub answers,
// and historyName is a real entry in the config history directory — see
// TestEveryRouteResolves, which creates one before calling this — so that a
// 404 there is unambiguously "the route is not registered" and not "there
// happens to be no history yet".
func specRoutes(assetURL, historyName string) []route {
	return []route{
		// 5.1 pages
		{"GET", "/"},
		{"GET", "/jobs"},
		{"GET", "/jobs/JOB-1"},
		{"GET", "/jobs/JOB-1/gate"},
		{"GET", "/jobs/JOB-1/diff"},
		{"GET", "/jobs/JOB-1/diff/0"},
		{"GET", "/jobs/JOB-1/diff.patch"},
		{"GET", "/jobs/JOB-1/events"},
		{"GET", "/jobs/JOB-1/logs"},
		{"GET", "/jobs/JOB-1/logs/build.log"},
		{"GET", "/jobs/JOB-1/artifacts"},
		{"GET", "/jobs/JOB-1/artifacts/spec.json"},
		{"GET", "/jobs/JOB-1/prompts/planning.md"},
		{"GET", "/submit"},
		{"GET", "/config"},
		{"GET", "/config/history"},
		{"GET", "/config/history/" + historyName},
		{"GET", assetURL},
		{"GET", "/healthz"},
		// 5.2 actions
		{"POST", "/jobs/JOB-1/approve"},
		{"POST", "/jobs/JOB-1/reject"},
		{"POST", "/jobs/JOB-1/resume"},
		{"POST", "/jobs/JOB-1/cancel"},
		{"POST", "/submit"},
		{"POST", "/prefs/diff-view"},
		{"POST", "/config"},
		// 5.3 live and data
		{"GET", "/events/stream"},
		{"GET", "/api/jobs.json"},
		{"GET", "/api/jobs/JOB-1.json"},
	}
}

// TestEveryRouteResolves is what proves the route table matches spec §5 line
// for line. It asserts only that the mux found a handler — most handlers are
// still stubs and answer 501 — because "the route exists" is the property six
// later owners are relying on and the only one that can be asserted before
// their handlers land.
func TestEveryRouteResolves(t *testing.T) {
	e := newEnv(t)
	e.job("AWAITING_MERGE_APPROVAL", "a job to resolve routes against")
	historyName := e.configHistoryEntry("20200101T000000.000000000Z-sdlc.yaml", "orchestrator: {}\n")

	for _, rt := range specRoutes(e.srv.assets["app.css"], historyName) {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			var res *http.Response
			if rt.method == "GET" {
				res = e.get(rt.path)
			} else {
				res = e.post(rt.path, url.Values{"state": {"AWAITING_MERGE_APPROVAL"}})
			}
			if res.StatusCode == http.StatusNotFound {
				t.Fatalf("%s %s: 404 — the route is not registered", rt.method, rt.path)
			}
			if res.StatusCode == http.StatusMethodNotAllowed {
				t.Fatalf("%s %s: 405 — the route is registered for a different method", rt.method, rt.path)
			}
		})
	}
}

func TestUnregisteredPathIs404(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/nope", "/jobs/JOB-1/nope", "/api/nope.json", "/jobs/JOB-1/diff.json"} {
		if got := e.get(p).StatusCode; got != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, got)
		}
	}
}

// TestPostOnlyRoutesRejectGET pins the property the spec asks the mux itself to
// provide: a decision route is not reachable by following a link, which is what
// a GET is.
func TestPostOnlyRoutesRejectGET(t *testing.T) {
	e := newEnv(t)
	e.job("AWAITING_MERGE_APPROVAL", "t")
	for _, p := range []string{"/jobs/JOB-1/approve", "/jobs/JOB-1/reject",
		"/jobs/JOB-1/resume", "/jobs/JOB-1/cancel", "/prefs/diff-view"} {
		if got := e.get(p).StatusCode; got != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", p, got)
		}
	}
}

func TestRootRedirectsToJobs(t *testing.T) {
	e := newEnv(t)
	res := e.get("/")
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET / = %d, want 303", res.StatusCode)
	}
	if got := res.Header.Get("Location"); got != "/jobs" {
		t.Fatalf("Location = %q, want /jobs", got)
	}
}

func TestHealthzSaysOk(t *testing.T) {
	e := newEnv(t)
	res := e.get("/healthz")
	if res.StatusCode != http.StatusOK || strings.TrimSpace(e.body(res)) != "ok" {
		t.Fatalf("GET /healthz = %d %q", res.StatusCode, e.body(res))
	}
}

// TestTemplatesExecuteWithZeroValues is the contract every W2.1 owner's
// template has to keep: a content block must execute against a pageData whose
// Body is nil. A page that dereferences .Body unguarded works in the test that
// built a model for it and fails on the day something hands it nothing — which
// is always a day something has already gone wrong, and usually a day somebody
// is waiting at a gate.
func TestTemplatesExecuteWithZeroValues(t *testing.T) {
	e := newEnv(t)
	if len(e.srv.tpl) == 0 {
		t.Fatal("no templates were parsed")
	}
	for name, tpl := range e.srv.tpl {
		for _, data := range []pageData{{}, e.srv.page(mustRequest(t), "", "", nil)} {
			if err := tpl.ExecuteTemplate(discard{}, "base.html", data); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
}

func mustRequest(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// TestAssetsAreContentHashedAndImmutable covers the whole offline story: the
// URL carries a hash, the response says it may be cached forever, and a name
// without the hash is not served at all.
func TestAssetsAreContentHashedAndImmutable(t *testing.T) {
	e := newEnv(t)
	u := e.srv.assets["app.css"]
	if !strings.HasPrefix(u, "/static/app.") || !strings.HasSuffix(u, ".css") || u == "/static/app.css" {
		t.Fatalf("asset URL %q carries no content hash", u)
	}
	res := e.get(u)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", u, res.StatusCode)
	}
	if got := res.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := res.Header.Get("Content-Type"); got != "text/css; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := e.get("/static/app.css").StatusCode; got != http.StatusNotFound {
		t.Errorf("unhashed /static/app.css = %d, want 404", got)
	}
}

// TestNoExternalResources is AC-3 by grep over what actually ships: no page may
// fetch anything from a host other than this one. `sdlc serve` has to render on
// a machine with no network, and a UI that pulls a font from a CDN also tells
// that CDN which gates the operator is reading.
func TestNoExternalResources(t *testing.T) {
	e := newEnv(t)
	for name := range e.srv.tpl {
		src, err := templateFS.ReadFile("templates/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{`src="http`, `src="//`, `href="http`, `href="//`} {
			if strings.Contains(string(src), bad) {
				t.Errorf("templates/%s fetches something external (%s)", name, bad)
			}
		}
	}
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"@import", "@font-face", "http://", "https://"} {
		if strings.Contains(string(css), bad) {
			t.Errorf("static/app.css references %s", bad)
		}
	}
}

// TestPostBodyIsBounded is AC-16 seen from the outside: a body over 1 MiB never
// reaches a handler.
func TestPostBodyIsBounded(t *testing.T) {
	e := newEnv(t)
	e.job("AWAITING_MERGE_APPROVAL", "t")
	form := url.Values{"csrf": {e.csrf}, "state": {"AWAITING_MERGE_APPROVAL"},
		"note": {strings.Repeat("x", 2<<20)}}
	res := e.post("/jobs/JOB-1/approve", form)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized POST = %d, want 413", res.StatusCode)
	}
	if got := len(e.approvals()); got != 0 {
		t.Fatalf("%d approval rows written for a refused POST", got)
	}
}

// TestShutdownIsClean asserts the hub goroutine is gone by the time Shutdown
// returns, rather than merely asked to go: a background goroutine that outlives
// the server is what turns a clean SIGINT into a hang.
func TestShutdownIsClean(t *testing.T) {
	e := newEnv(t)
	e.srv.Start()
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
}

// TestShutdownWithoutStart covers the path a failed bind takes: Serve never
// ran, so the hub never started, and Shutdown must still return rather than
// wait forever for a goroutine that does not exist.
func TestShutdownWithoutStart(t *testing.T) {
	e := newEnv(t)
	done := make(chan error, 1)
	go func() { done <- e.srv.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown blocked when the hub had never been started")
	}
}

// TestEveryPageOffersARefreshWithoutJavaScript holds spec §9.3's promise that
// every page is usable with JavaScript off. app.js hides this link once the
// stream is live, so it has to be in the server-rendered markup — a link the
// script creates is a link that does not exist for the reader who needs it.
func TestEveryPageOffersARefreshWithoutJavaScript(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "something to look at")
	for _, p := range []string{
		"/jobs", "/jobs/" + j.ID, "/jobs/" + j.ID + "/gate", "/jobs/" + j.ID + "/diff",
		"/jobs/" + j.ID + "/events", "/jobs/" + j.ID + "/logs", "/submit", "/config",
	} {
		body := e.body(e.get(p))
		if !strings.Contains(body, `id="refresh-link"`) {
			t.Errorf("GET %s renders no refresh link, so the page is a dead end without JavaScript", p)
		}
	}
}
