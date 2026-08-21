package server

// The decision POSTs (spec §5.2). Owner: W2-I.
//
// Every test here asserts through the store, not the response: what makes a
// decision route correct is the row it wrote or did not write, and a handler
// that answers 400 and writes the row anyway is the failure this file exists to
// catch. The response is checked too, because a decision that lands but answers
// 200 is one a browser reload posts a second time.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/vipinm/sdlc-orchestrator/internal/engine"
	"github.com/vipinm/sdlc-orchestrator/internal/review"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// gateStates are the four states an approve or reject applies to, paired with
// the gate review.GateFor names for each. Written out rather than derived so
// the test fails if that mapping changes, instead of following it.
var gateStates = []struct{ state, gate string }{
	{"AWAITING_SPEC_APPROVAL", review.GateSpec},
	{"AWAITING_CODE_APPROVAL", review.GateCode},
	{"AWAITING_MERGE_APPROVAL", review.GateMerge},
	{"AWAITING_RELEASE_APPROVAL", review.GateRelease},
}

// wantRedirect asserts the 303-to-the-job that every successful POST answers.
func wantRedirect(t *testing.T, res *http.Response, to string) {
	t.Helper()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", res.StatusCode)
	}
	if got := res.Header.Get("Location"); got != to {
		t.Fatalf("Location = %q, want %q", got, to)
	}
}

// wantNoRows is the assertion every refusal shares.
func wantNoRows(t *testing.T, e *env, what string) {
	t.Helper()
	if rows := e.approvals(); len(rows) != 0 {
		t.Fatalf("%s wrote %d approval row(s): %+v", what, len(rows), rows)
	}
}

func onlyRow(t *testing.T, e *env) approvalRow {
	t.Helper()
	rows := e.approvals()
	if len(rows) != 1 {
		t.Fatalf("got %d approval rows, want exactly 1: %+v", len(rows), rows)
	}
	return rows[0]
}

// TestApprovalRowMatchesCLI pins the row against the one cli.cmdDecision writes
// for the same decision: same gate from review.GateFor, same decision string,
// and the note carried in Reason — which is what `sdlc approve --note` does
// when no --reason competes for the field. A second shape of row is how the two
// surfaces would come to mean different things to the engine.
func TestApprovalRowMatchesCLI(t *testing.T) {
	for _, gs := range gateStates {
		t.Run(gs.state, func(t *testing.T) {
			e := newEnv(t)
			j := e.job(gs.state, "a job")
			res := e.post("/jobs/"+j.ID+"/approve", url.Values{
				"state": {gs.state},
				"note":  {"looks right to me"},
			})
			wantRedirect(t, res, "/jobs/"+j.ID)
			got := onlyRow(t, e)
			want := approvalRow{jobID: j.ID, gate: gs.gate, decision: "approve", reason: "looks right to me"}
			if got != want {
				t.Errorf("row = %+v, want %+v", got, want)
			}
		})
	}
}

// TestRejectWritesTheReasonAndTheCancelFlag covers the other half of the same
// row, including `cancel`: rejecting with it set is `sdlc reject --cancel`, and
// the flag has to survive the trip or a rejection meant to end the job would
// send it back to be fixed instead.
func TestRejectWritesTheReasonAndTheCancelFlag(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		name := "keep"
		form := url.Values{"state": {"AWAITING_MERGE_APPROVAL"}, "reason": {"the dialog copy is wrong"}}
		if cancel {
			name = "cancel"
			form.Set("cancel", "yes")
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			j := e.job("AWAITING_MERGE_APPROVAL", "a job")
			res := e.post("/jobs/"+j.ID+"/reject", form)
			wantRedirect(t, res, "/jobs/"+j.ID)
			got := onlyRow(t, e)
			want := approvalRow{jobID: j.ID, gate: review.GateMerge, decision: "reject",
				reason: "the dialog copy is wrong", cancel: cancel}
			if got != want {
				t.Errorf("row = %+v, want %+v", got, want)
			}
		})
	}
}

// TestRejectRequiresReason is the one refusal that costs something to get
// wrong: the reason is handed to the next agent verbatim, so a rejection
// recorded without one tells it only that somebody said no. 400, the field
// named, and — the part that matters — nothing in the approvals table for the
// engine to pick up on its next tick.
func TestRejectRequiresReason(t *testing.T) {
	empty := []struct {
		name string
		form url.Values
	}{
		{"absent", url.Values{"state": {"AWAITING_MERGE_APPROVAL"}}},
		{"empty", url.Values{"state": {"AWAITING_MERGE_APPROVAL"}, "reason": {""}}},
		{"whitespace", url.Values{"state": {"AWAITING_MERGE_APPROVAL"}, "reason": {"   \t\n "}}},
	}
	for _, c := range empty {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			j := e.job("AWAITING_MERGE_APPROVAL", "a job")
			res := e.post("/jobs/"+j.ID+"/reject", c.form)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.StatusCode)
			}
			if body := e.body(res); !strings.Contains(body, "reason") {
				t.Errorf("the 400 does not flag the reason field: %q", body)
			}
			wantNoRows(t, e, "a rejection with no reason")
		})
	}
}

// TestCancelRequiresConfirmation matches promptHold's [y/N]. Cancelling deletes
// the worktree, so a POST that merely reached the route is not consent.
func TestCancelRequiresConfirmation(t *testing.T) {
	for _, confirm := range []string{"", "no", "y", "on", "true", "YES"} {
		t.Run("confirm="+confirm, func(t *testing.T) {
			e := newEnv(t)
			j := e.job("ESCALATED", "a job")
			form := url.Values{"state": {"ESCALATED"}}
			if confirm != "" {
				form.Set("confirm", confirm)
			}
			res := e.post("/jobs/"+j.ID+"/cancel", form)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("confirm=%q = %d, want 400", confirm, res.StatusCode)
			}
			wantNoRows(t, e, "an unconfirmed cancel")
		})
	}

	// The confirmed one lands, or the test above would pass against a handler
	// that refused every cancel.
	e := newEnv(t)
	j := e.job("ESCALATED", "a job")
	res := e.post("/jobs/"+j.ID+"/cancel", url.Values{"state": {"ESCALATED"}, "confirm": {"yes"}})
	wantRedirect(t, res, "/jobs/"+j.ID)
	got := onlyRow(t, e)
	if want := (approvalRow{jobID: j.ID, gate: "cancel", decision: "cancel"}); got != want {
		t.Errorf("row = %+v, want %+v", got, want)
	}
}

// TestResumeValidatesTargetState checks the `to` field against engine's list
// before anything is written. The <select> makes an invalid value unreachable
// from the UI; this is the check that answers a hand-written POST, which is the
// only way one can arrive.
func TestResumeValidatesTargetState(t *testing.T) {
	bad := []string{"NOT_A_STATE", "CREATED", "ESCALATED", "merging", "MERGING\tX"}
	for _, to := range bad {
		t.Run("to="+to, func(t *testing.T) {
			e := newEnv(t)
			j := e.job("ESCALATED", "a job")
			res := e.post("/jobs/"+j.ID+"/resume", url.Values{"state": {"ESCALATED"}, "to": {to}})
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("to=%q = %d, want 400", to, res.StatusCode)
			}
			if body := e.body(res); !strings.Contains(body, engine.ResumableStates()[0]) {
				t.Errorf("the 400 does not name the states that are valid: %q", body)
			}
			wantNoRows(t, e, "a resume with an invalid target")
		})
	}

	// Every state the engine says is resumable is accepted, so the check can
	// never be tightened past the list the form renders.
	for _, to := range engine.ResumableStates() {
		t.Run("valid/"+to, func(t *testing.T) {
			e := newEnv(t)
			j := e.job("TIMED_OUT", "a job")
			res := e.post("/jobs/"+j.ID+"/resume", url.Values{
				"state": {"TIMED_OUT"}, "to": {to}, "note": {"fixtures are stale"},
			})
			wantRedirect(t, res, "/jobs/"+j.ID)
			got := onlyRow(t, e)
			// Reason is the target state and Note is the instruction, which is
			// how handleControls reads a resume row.
			want := approvalRow{jobID: j.ID, gate: "resume", decision: "resume",
				reason: to, note: "fixtures are stale"}
			if got != want {
				t.Errorf("row = %+v, want %+v", got, want)
			}
		})
	}

	// An omitted target is the CLI's default — resume into the state the job
	// stopped in — and must not be mistaken for an invalid one.
	t.Run("omitted", func(t *testing.T) {
		e := newEnv(t)
		j := e.job("ESCALATED", "a job")
		res := e.post("/jobs/"+j.ID+"/resume", url.Values{"state": {"ESCALATED"}})
		wantRedirect(t, res, "/jobs/"+j.ID)
		if got := onlyRow(t, e).reason; got != "" {
			t.Errorf("reason = %q, want empty so the engine resumes where the job stopped", got)
		}
	})
}

// TestApproveOnAJobWithNoGateIsRefused covers the two states that carry no
// approval gate: a running job, and a held one — which is cleared with resume
// or cancel, not with approve. cmdDecision refuses both and so must this.
func TestApproveOnAJobWithNoGateIsRefused(t *testing.T) {
	for _, state := range []string{"PLANNING", "ESCALATED", "TIMED_OUT"} {
		for _, action := range []string{"approve", "reject"} {
			t.Run(state+"/"+action, func(t *testing.T) {
				e := newEnv(t)
				j := e.job(state, "a job")
				res := e.post("/jobs/"+j.ID+"/"+action, url.Values{
					"state": {state}, "reason": {"a reason"},
				})
				if res.StatusCode != http.StatusConflict {
					t.Fatalf("%s in %s = %d, want 409", action, state, res.StatusCode)
				}
				wantNoRows(t, e, action+" at a state with no approval gate")
			})
		}
	}
}

// TestSecondDecisionIsRefused is AC-14. The engine consumes the oldest row, so
// a second one is not a correction — it is an answer that will be discarded
// without anybody being told.
func TestSecondDecisionIsRefused(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "a job")
	res := e.post("/jobs/"+j.ID+"/approve", url.Values{"state": {j.State}, "note": {"first"}})
	wantRedirect(t, res, "/jobs/"+j.ID)

	second := e.post("/jobs/"+j.ID+"/reject", url.Values{"state": {j.State}, "reason": {"second thoughts"}})
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("a second decision = %d, want 409", second.StatusCode)
	}
	// The same sentence reviewer.promptGate prints, so an operator who uses
	// both surfaces is told the same thing by both.
	if body := e.body(second); !strings.Contains(body, "already pending for "+j.ID) {
		t.Errorf("the 409 does not say a decision is already pending: %q", body)
	}
	if got := onlyRow(t, e).reason; got != "first" {
		t.Errorf("the surviving row reads %q; the first decision must be the one that stands", got)
	}
}

// TestSecondDecisionIsAllowedOnceTheFirstIsConsumed keeps the check above from
// being read as "one decision per job forever": the block is a pending row, and
// PendingApproval is what clears.
func TestSecondDecisionIsAllowedOnceTheFirstIsConsumed(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "a job")
	if err := e.st.AddApproval(&store.Approval{
		JobID: j.ID, Gate: review.GateMerge, Decision: "approve",
	}); err != nil {
		t.Fatal(err)
	}
	// Consumed the way the engine consumes it once it has acted on it, which is
	// the state a gate that comes round again is actually in.
	a, err := e.st.PendingApproval(j.ID, review.GateMerge)
	if err != nil || a == nil {
		t.Fatalf("PendingApproval: %v, %v", a, err)
	}
	if err := e.st.ConsumeApproval(a.ID); err != nil {
		t.Fatal(err)
	}
	res := e.post("/jobs/"+j.ID+"/approve", url.Values{"state": {j.State}})
	wantRedirect(t, res, "/jobs/"+j.ID)
	if n := len(e.approvals()); n != 2 {
		t.Fatalf("got %d rows, want 2 — the consumed one and the new one", n)
	}
}

// TestEveryPostAnswers303 is AC-15 across all six routes: a decision that
// answered 200 would be re-posted by a reload, and the second post of an
// approval is a decision nobody made.
func TestEveryPostAnswers303(t *testing.T) {
	e := newEnv(t)
	gate := e.job("AWAITING_MERGE_APPROVAL", "gate job")
	held := e.job("ESCALATED", "held job")
	cancelled := e.job("ESCALATED", "cancel job")

	posts := []struct {
		path string
		form url.Values
		to   string
	}{
		{"/jobs/" + gate.ID + "/approve", url.Values{"state": {gate.State}}, "/jobs/" + gate.ID},
		{"/jobs/" + held.ID + "/resume", url.Values{"state": {held.State}}, "/jobs/" + held.ID},
		{"/jobs/" + cancelled.ID + "/cancel", url.Values{"state": {cancelled.State}, "confirm": {"yes"}}, "/jobs/" + cancelled.ID},
		{"/submit", url.Values{"target": {"demo"}, "title": {"from the ui"}, "body": {"do it"}}, "/jobs/JOB-4"},
		{"/prefs/diff-view", url.Values{"view": {"split"}}, "/jobs"},
	}
	for _, p := range posts {
		t.Run(p.path, func(t *testing.T) {
			wantRedirect(t, e.post(p.path, p.form), p.to)
		})
	}

	// reject needs its own job because the three above are spent: a second
	// decision on any of them is the 409 the previous test asserts.
	rej := e.job("AWAITING_CODE_APPROVAL", "reject job")
	wantRedirect(t, e.post("/jobs/"+rej.ID+"/reject",
		url.Values{"state": {rej.State}, "reason": {"no"}}), "/jobs/"+rej.ID)
}

// --- submit --------------------------------------------------------------

// TestSubmitFromUICreatesSameJob asserts the web form goes through jobs.Submit:
// the job it creates carries the id, branch and worktree that function derives,
// and its validation is the validation the form gets. A second set of rules
// here is how `sdlc submit` and the UI would come to accept different things.
func TestSubmitFromUICreatesSameJob(t *testing.T) {
	e := newEnv(t)
	res := e.post("/submit", url.Values{
		"target": {"demo"}, "title": {"make the button blue"}, "body": {"it is red"},
	})
	wantRedirect(t, res, "/jobs/JOB-1")

	j, err := e.st.GetJob("JOB-1")
	if err != nil || j == nil {
		t.Fatalf("GetJob: %v, %v", j, err)
	}
	if j.IssueTitle != "make the button blue" || j.IssueBody != "it is red" {
		t.Errorf("title/body = %q / %q", j.IssueTitle, j.IssueBody)
	}
	if j.Target != "demo" || j.State != "CREATED" {
		t.Errorf("target/state = %q / %q", j.Target, j.State)
	}
	// Derived by jobs.Submit from the target's config, not by this handler.
	if j.Branch != "sdlc/JOB-1" {
		t.Errorf("branch = %q, want sdlc/JOB-1", j.Branch)
	}
	if len(e.approvals()) != 0 {
		t.Error("submitting a job wrote an approval row")
	}
}

// TestSubmitRejectionsComeFromJobs walks the refusals jobs.Submit already
// makes, so the handler is seen mapping them rather than re-deciding them.
func TestSubmitRejectionsComeFromJobs(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
		code int
	}{
		{"no title", url.Values{"target": {"demo"}, "body": {"a body"}}, http.StatusBadRequest},
		{"whitespace title", url.Values{"target": {"demo"}, "title": {"   "}}, http.StatusBadRequest},
		{"unknown target", url.Values{"target": {"nope"}, "title": {"t"}}, http.StatusBadRequest},
		{"control characters", url.Values{"target": {"demo"}, "title": {"a\x1b[31mtitle"}}, http.StatusBadRequest},
		{"title too long", url.Values{"target": {"demo"}, "title": {strings.Repeat("x", 501)}}, http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			res := e.post("/submit", c.form)
			if res.StatusCode != c.code {
				t.Fatalf("status = %d, want %d", res.StatusCode, c.code)
			}
			list, err := e.st.ListJobs()
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 0 {
				t.Fatalf("a refused submission created %d job(s)", len(list))
			}
		})
	}
}

// postWithReferer is post() plus a Referer header. It lives here rather than in
// the shared fixture because the header only matters to one route: the view
// preference is the only handler that reads it, and it reads it as a hint that
// has to prove it is same-origin.
func (e *env) postWithReferer(path string, form url.Values, referer string) *http.Response {
	e.t.Helper()
	form.Set("csrf", e.csrf)
	req := e.formRequest(path, form)
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: e.csrf})
	req.Header.Set("Referer", referer)
	return e.do(req)
}

// --- the diff view preference -------------------------------------------

// TestDiffViewPrefIsNotAnOpenRedirect is the whole point of checking the
// Referer rather than following it. Any page anywhere can send a browser
// through this server and out to a destination of its choosing, which is how a
// phishing page borrows an address the operator trusts.
func TestDiffViewPrefIsNotAnOpenRedirect(t *testing.T) {
	e := newEnv(t)
	hostile := []string{
		"https://evil.example/phish",
		"//evil.example/phish",
		"http://127.0.0.1.evil.example/phish",
		"https://evil.example",
		"javascript:alert(1)",
		"http://evil.example/jobs/JOB-1/diff",
	}
	for _, ref := range hostile {
		t.Run(ref, func(t *testing.T) {
			res := e.postWithReferer("/prefs/diff-view", url.Values{"view": {"split"}}, ref)
			wantRedirect(t, res, "/jobs")
		})
	}

	// A Referer this server actually served is followed, or the check would be
	// satisfied by ignoring the header entirely and the reader would lose their
	// place on every toggle.
	own := []struct{ ref, want string }{
		{e.ts.URL + "/jobs/JOB-1/diff", "/jobs/JOB-1/diff"},
		{e.ts.URL + "/jobs/JOB-1/diff?file=2", "/jobs/JOB-1/diff?file=2"},
		{"/jobs/JOB-1/diff", "/jobs/JOB-1/diff"},
	}
	for _, c := range own {
		t.Run(c.ref, func(t *testing.T) {
			wantRedirect(t, e.postWithReferer("/prefs/diff-view", url.Values{"view": {"unified"}}, c.ref), c.want)
		})
	}
}

// TestDiffViewPrefSetsTheCookie covers what the redirect is in aid of, and the
// allowlist: a value the diff handler does not recognise would leave the reader
// on whatever it happens to default to.
func TestDiffViewPrefSetsTheCookie(t *testing.T) {
	e := newEnv(t)
	for _, view := range []string{"unified", "split"} {
		res := e.post("/prefs/diff-view", url.Values{"view": {view}})
		var c *http.Cookie
		for _, got := range res.Cookies() {
			if got.Name == prefDiffViewCookie {
				c = got
			}
		}
		if c == nil {
			t.Fatalf("view=%s set no %s cookie", view, prefDiffViewCookie)
		}
		if c.Value != view {
			t.Errorf("cookie = %q, want %q", c.Value, view)
		}
		if !c.HttpOnly || c.Path != "/" {
			t.Errorf("cookie attributes: HttpOnly=%v Path=%q", c.HttpOnly, c.Path)
		}
	}
	for _, view := range []string{"", "sideways", "unified\nSet-Cookie: x=y"} {
		res := e.post("/prefs/diff-view", url.Values{"view": {view}})
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("view=%q = %d, want 400", view, res.StatusCode)
		}
	}
}

// --- the forms -----------------------------------------------------------

// renderForm executes one of _forms.html's named templates the way a page
// invokes it.
func renderForm(t *testing.T, e *env, name string, m FormModel) string {
	t.Helper()
	tpl, ok := e.srv.tpl["job.html"]
	if !ok {
		t.Fatal("job.html did not parse, so the shared forms cannot be reached")
	}
	var b strings.Builder
	if err := tpl.ExecuteTemplate(&b, name, m); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return b.String()
}

// TestDecisionFormsCarryTheStateField is AC-13 from the template side. The
// hidden state field is what makes a tab left open on a gate page refuse rather
// than approve; a form without it posts a decision the freshness check cannot
// evaluate, and the operator sees a 400 instead of the gate they were reading.
func TestDecisionFormsCarryTheStateField(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "a job")
	m := e.srv.formModel(mustRequest(t), j, "unified")

	for _, name := range []string{"decisionForm", "holdForm"} {
		out := renderForm(t, e, name, m)
		forms := strings.Count(out, "<form")
		if forms != 2 {
			t.Fatalf("%s rendered %d forms, want 2", name, forms)
		}
		if got := strings.Count(out, `name="state" value="AWAITING_MERGE_APPROVAL"`); got != forms {
			t.Errorf("%s: %d of %d forms carry the state field", name, got, forms)
		}
		if got := strings.Count(out, `name="csrf"`); got != forms {
			t.Errorf("%s: %d of %d forms carry the CSRF token", name, got, forms)
		}
		if got := strings.Count(out, `data-gate-state="AWAITING_MERGE_APPROVAL"`); got != forms {
			t.Errorf("%s: %d of %d forms carry data-gate-state for app.js", name, got, forms)
		}
	}
}

// TestResumeFormOffersExactlyTheResumableStates is what makes the server-side
// check unreachable from the UI: the options are engine's list, so a state
// added there appears here and nothing else ever does.
func TestResumeFormOffersExactlyTheResumableStates(t *testing.T) {
	e := newEnv(t)
	j := e.job("ESCALATED", "a job")
	out := renderForm(t, e, "holdForm", e.srv.formModel(mustRequest(t), j, "unified"))

	for _, s := range engine.ResumableStates() {
		if !strings.Contains(out, `<option value="`+s+`">`) {
			t.Errorf("the resume select has no option for %s", s)
		}
	}
	// One more than the engine's list: the empty default, "the state it
	// stopped in", which is what an omitted --to means.
	if got, want := strings.Count(out, "<option "), len(engine.ResumableStates())+1; got != want {
		t.Errorf("%d options rendered, want %d", got, want)
	}
	if !strings.Contains(out, `name="confirm" value="yes" required`) {
		t.Error("the cancel form does not require the confirmation the handler demands")
	}
}

// TestDecisionFormEscapesWhatItRenders. The job id and state reach the form as
// attribute values; html/template escapes them, and this asserts that nothing
// here bypassed it. The values are agent- and operator-supplied, so a quote in
// one must not become markup.
func TestDecisionFormEscapesWhatItRenders(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", `"><script>alert(1)</script>`)
	j.ID = `JOB-1"><script>alert(1)</script>`
	out := renderForm(t, e, "decisionForm", e.srv.formModel(mustRequest(t), j, "unified"))
	if strings.Contains(out, "<script>") {
		t.Errorf("a hostile id reached the page as markup:\n%s", out)
	}
}

// TestNoGateNoDecisionForm: a job that is not at an approval gate must not be
// shown buttons that the server would refuse with a 409.
func TestNoGateNoDecisionForm(t *testing.T) {
	e := newEnv(t)
	j := e.job("PLANNING", "a job")
	if out := renderForm(t, e, "decisionForm", e.srv.formModel(mustRequest(t), j, "unified")); strings.Contains(out, "<form") {
		t.Errorf("a running job was offered a decision form:\n%s", out)
	}
	// A held job is not offered one either: a hold is cleared with holdForm.
	j.State = "ESCALATED"
	if out := renderForm(t, e, "decisionForm", e.srv.formModel(mustRequest(t), j, "unified")); strings.Contains(out, "<form") {
		t.Errorf("a held job was offered an approve/reject form:\n%s", out)
	}
}

// TestDiffViewToggleCarriesBothViews covers the toggle's own DOM contract.
func TestDiffViewToggleCarriesBothViews(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "a job")
	out := renderForm(t, e, "diffViewToggle", e.srv.formModel(mustRequest(t), j, "split"))
	for _, want := range []string{`action="/prefs/diff-view"`, `name="view" value="unified"`, `name="view" value="split"`, `name="csrf"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the toggle is missing %s:\n%s", want, out)
		}
	}
}
