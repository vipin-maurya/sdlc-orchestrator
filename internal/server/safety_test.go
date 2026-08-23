package server

// Tests for safety.go. Owner: W2-G.
//
// These are the tests that matter most in this package. A mistake in safety.go
// is not a bug, it is an unauthenticated approve button, and it would be
// retrofitted underneath six other owners' handlers.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- safeName ------------------------------------------------------------

func TestServedFileNamesAreAllowlisted(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "sdlc.db")
	const secret = "SECRET-BYTES-FROM-OUTSIDE-THE-DIRECTORY"
	if err := os.WriteFile(outside, []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build.log"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink is in os.ReadDir's listing, so membership alone would let it
	// through; only regular files resolve.
	link := filepath.Join(dir, "escape.log")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	hostile := []string{
		"..",
		".",
		"",
		"../../etc/passwd",
		"../../sdlc.db",
		`..\..\sdlc.db`,
		"....//",
		"/etc/passwd",
		filepath.Join(string(filepath.Separator), "etc", "passwd"),
		"C:sdlc.db",
		"build.log:$DATA",
		"BUILD~1.LOG",
		"build.log\x00",
		"build.log\x00.txt",
		"escape.log",
		"subdir/build.log",
	}
	for _, name := range hostile {
		t.Run(strings.ReplaceAll(name, "\x00", `\x00`), func(t *testing.T) {
			got, err := safeName(dir, name)
			if err == nil {
				t.Fatalf("safeName(%q) resolved to %q; it must not resolve at all", name, got)
			}
			if !errors.Is(err, errUnsafeName) && !errors.Is(err, errNoSuchName) {
				t.Fatalf("safeName(%q) = %v, want errUnsafeName or errNoSuchName", name, err)
			}
			if got != "" {
				t.Fatalf("safeName(%q) returned a path %q alongside its error", name, got)
			}
		})
	}

	// A real entry resolves, or every one of the refusals above would be
	// trivially satisfied by a function that always fails.
	got, err := safeName(dir, "build.log")
	if err != nil {
		t.Fatalf("safeName of a real entry: %v", err)
	}
	if want := filepath.Join(dir, "build.log"); got != want {
		t.Fatalf("safeName = %q, want %q", got, want)
	}
	b, err := os.ReadFile(got)
	if err != nil || string(b) != "ok" {
		t.Fatalf("resolved path did not read back: %q %v", b, err)
	}
}

// TestSafeNameNeverReadsOutsideTheDirectory is the second half of AC-9: not
// only does each hostile name fail, no byte from outside the directory can be
// reached through one.
func TestSafeNameNeverReadsOutsideTheDirectory(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "sdlc.db"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(dir, filepath.Join(other, "sdlc.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := safeName(dir, rel); err == nil {
		t.Fatalf("safeName resolved %q, which climbs out of the directory", rel)
	}
}

// --- the Host allowlist --------------------------------------------------

// TestHostHeaderRejectsRebinding is the DNS-rebinding defence. A browser on the
// operator's machine can be pointed at an attacker's domain that resolves to
// 127.0.0.1; the one thing the attacker cannot change is the Host header the
// browser then sends, so that is what is checked.
func TestHostHeaderRejectsRebinding(t *testing.T) {
	e := newEnv(t)
	e.job("AWAITING_MERGE_APPROVAL", "t")

	// Every route, not just the pages: a rebinding attack that could read
	// /events/stream would watch every job the operator has.
	paths := []string{"/", "/jobs", "/jobs/JOB-1", "/healthz", "/config",
		"/events/stream", "/api/jobs.json", "/api/jobs/JOB-1.json",
		e.srv.assets["app.css"]}

	t.Run("allowed", func(t *testing.T) {
		host := strings.TrimPrefix(e.ts.URL, "http://") // 127.0.0.1:<port>
		_, port, _ := strings.Cut(host, ":")
		for _, h := range []string{
			"127.0.0.1", "127.0.0.1:" + port,
			"localhost", "localhost:" + port,
			"[::1]", "[::1]:" + port,
		} {
			for _, p := range paths {
				res := e.getHost(p, h)
				if res.StatusCode == http.StatusMisdirectedRequest {
					t.Errorf("Host %q on %s = 421; it is this machine", h, p)
				}
			}
		}
	})

	t.Run("refused", func(t *testing.T) {
		for _, h := range []string{
			"evil.com",
			"evil.example",
			// SplitHostPort will call "7777.evil.example" a port and report
			// 127.0.0.1 as the host, so without a digits-only check on the port
			// this reads as loopback.
			"127.0.0.1:7777.evil.example",
			"localhost:notaport",
			"127.0.0.1.evil.com",
			"localhost.evil.com",
			"notlocalhost",
			"127.0.0.1.evil.com:7777",
			"192.168.1.10:7777",
		} {
			for _, p := range paths {
				res := e.getHost(p, h)
				if res.StatusCode != http.StatusMisdirectedRequest {
					t.Errorf("Host %q on %s = %d, want 421", h, p, res.StatusCode)
					continue
				}
				if body := e.body(res); strings.Contains(body, "<html") || strings.Contains(body, "JOB-1") {
					t.Errorf("Host %q on %s answered with application content: %q", h, p, body)
				}
			}
		}
	})
}

// TestEmptyHostIsRefused is separate because net/http will not let a client set
// an empty Host through the normal field, so the request is written by hand.
func TestEmptyHostIsRefused(t *testing.T) {
	if hostAllowed("") {
		t.Fatal(`hostAllowed("") = true; a request with no Host is not one any browser makes`)
	}
	// HTTP/1.0 rather than 1.1: net/http rejects a 1.1 request with no Host
	// itself, with a 400, so the request never reaches the allowlist. A 1.0
	// request with no Host does reach it, with r.Host empty.
	e := newEnv(t)
	res := e.rawRequest("GET /jobs HTTP/1.0\r\n\r\n")
	if !strings.Contains(res, "421") {
		t.Fatalf("a request with no Host was not refused with 421:\n%s", res)
	}
	if strings.Contains(res, "<html") {
		t.Fatalf("a request with no Host was answered with application content:\n%s", res)
	}
}

// --- CSRF ----------------------------------------------------------------

// TestCSRFRequiredOnEveryPost walks every POST in spec §5.2. The double-submit
// pair is the whole defence: there is no session to compare against, because
// there are no users (spec §2.2).
func TestCSRFRequiredOnEveryPost(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "t")

	posts := []string{
		"/jobs/" + j.ID + "/approve",
		"/jobs/" + j.ID + "/reject",
		"/jobs/" + j.ID + "/resume",
		"/jobs/" + j.ID + "/cancel",
		"/submit",
		"/prefs/diff-view",
	}
	cases := []struct {
		name   string
		cookie string
		field  string
	}{
		{"neither", "", ""},
		{"cookie only", e.csrf, ""},
		{"field only", "", e.csrf},
		{"mismatched", e.csrf, e.csrf + "x"},
		{"both empty strings", "", ""},
	}
	for _, p := range posts {
		for _, c := range cases {
			t.Run(p+"/"+c.name, func(t *testing.T) {
				form := url.Values{"state": {j.State}}
				if c.field != "" {
					form.Set("csrf", c.field)
				}
				req := e.formRequest(p, form)
				if c.cookie != "" {
					req.AddCookie(&http.Cookie{Name: csrfCookie, Value: c.cookie})
				}
				res := e.do(req)
				if res.StatusCode != http.StatusForbidden {
					t.Fatalf("%s = %d, want 403", p, res.StatusCode)
				}
				if n := len(e.approvals()); n != 0 {
					t.Fatalf("%d approval rows written by a POST that failed CSRF", n)
				}
			})
		}
	}

	// The matching pair reaches the handler. Without this the test above would
	// pass against a server that refused every POST unconditionally.
	res := e.post("/jobs/"+j.ID+"/approve", url.Values{"state": {j.State}})
	if res.StatusCode == http.StatusForbidden {
		t.Fatal("a matching CSRF pair was refused")
	}
}

// TestCSRFCookieAttributes is AC-12. Each attribute is here because of a
// specific way the token would otherwise leak or fail to be sent.
func TestCSRFCookieAttributes(t *testing.T) {
	e := newEnv(t)
	res := e.get("/healthz")
	var c *http.Cookie
	for _, got := range res.Cookies() {
		if got.Name == csrfCookie {
			c = got
		}
	}
	if c == nil {
		t.Fatal("no CSRF cookie was set")
	}
	if !c.HttpOnly {
		t.Error("the CSRF cookie is readable from script")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
	if len(c.Value) < 32 {
		t.Errorf("token is %d characters; it is meant to be 32 random bytes", len(c.Value))
	}
	// Two servers must not mint the same token, which a non-random source
	// (a counter, a timestamp, math/rand's default seed) would.
	if other := newEnv(t).csrf; other == c.Value {
		t.Error("two independent servers minted the same CSRF token")
	}
}

// --- staleness -----------------------------------------------------------

// TestStaleStateNeverReachesTheHandler is spec §10.3's first mechanism. The
// handlers are still stubs, so "reached the handler" is 501 and "was refused"
// is 409 — which is exactly the distinction under test.
func TestStaleStateNeverReachesTheHandler(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "t")

	// A fresh decision must reach the handler and write its row. This half is
	// what stops the test passing for the wrong reason: without it, a
	// requireFreshState that refused everything would satisfy the stale case
	// below and look correct.
	fresh := e.post("/jobs/"+j.ID+"/approve", url.Values{"state": {"AWAITING_MERGE_APPROVAL"}})
	if fresh.StatusCode != http.StatusSeeOther {
		t.Fatalf("a fresh decision = %d, want 303; it should have reached the handler", fresh.StatusCode)
	}
	if n := len(e.approvals()); n != 1 {
		t.Fatalf("a fresh decision wrote %d approval rows, want 1", n)
	}
	// Consume it before testing staleness. A pending row makes the next
	// decision a 409 on its own (AC-14), which is the same status staleness
	// produces — leaving it would let this test pass without the state
	// comparison ever running.
	pending, err := e.st.PendingApproval(j.ID, "merge")
	if err != nil || pending == nil {
		t.Fatalf("no pending approval to consume: %v", err)
	}
	if err := e.st.ConsumeApproval(pending.ID); err != nil {
		t.Fatal(err)
	}
	before := len(e.approvals())

	// The job moves to another state that ALSO has an approval gate. Moving it
	// to a gateless state (MERGING, say) would make this pass without the
	// comparison ever running: approvalGate refuses a state with no gate, and
	// its refusal is a 409 too. Verified — with the state comparison disabled
	// and MERGING here, this test still passed.
	j.State = "AWAITING_RELEASE_APPROVAL"
	if err := e.st.UpdateJob(j); err != nil {
		t.Fatal(err)
	}
	stale := e.post("/jobs/"+j.ID+"/approve", url.Values{"state": {"AWAITING_MERGE_APPROVAL"}})
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("a stale decision = %d, want 409", stale.StatusCode)
	}
	if body := e.body(stale); !strings.Contains(body, "AWAITING_RELEASE_APPROVAL") {
		t.Errorf("the 409 does not say what the job is doing now: %q", body)
	}
	// Counted against the row the fresh decision left behind rather than
	// against zero: ConsumeApproval marks a row spent, it does not delete it.
	if n := len(e.approvals()); n != before {
		t.Fatalf("a stale decision wrote %d approval row(s)", n-before)
	}

	// A form with no state at all is refused too, or hand-posting one would be
	// a way around the check.
	none := e.post("/jobs/"+j.ID+"/approve", url.Values{})
	if none.StatusCode != http.StatusBadRequest {
		t.Fatalf("a decision with no state field = %d, want 400", none.StatusCode)
	}
}

// TestDecisionOnAnUnknownJobIs404 covers the other order: the job is resolved
// before the state is compared, so a made-up id cannot reach a handler.
func TestDecisionOnAnUnknownJobIs404(t *testing.T) {
	e := newEnv(t)
	res := e.post("/jobs/JOB-999/approve", url.Values{"state": {"AWAITING_MERGE_APPROVAL"}})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("= %d, want 404", res.StatusCode)
	}
}

// --- panics --------------------------------------------------------------

func TestRecoverPanicAnswers500AndKeepsServing(t *testing.T) {
	e := newEnv(t)
	h := e.srv.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, mustRequest(t))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a panicking handler = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("the panic value reached the browser; it belongs in the console")
	}
}
