package server

// Decision and submit POSTs (spec §5.2). Owner: W2-I.
//
// Created empty by the skeleton commit. Every handler here is already behind
// requireCSRF, and the four decision handlers behind requireFreshState as well
// — that is server.go's job and not this file's, so an owner filling these in
// cannot accidentally register one without a guard.
//
// Every handler in this file writes at most one store.Approval through
// st.AddApproval — the same call `sdlc approve`, `sdlc reject`, `sdlc resume`,
// `sdlc cancel` and the `sdlc review` prompt reach through cli.recordDecision —
// and does nothing else. The engine must see one shape of row no matter which
// surface a person used; a second way to approve a job is how the two drift.
//
// On success every handler answers 303 (AC-15), so a browser reload re-fetches
// the job page instead of re-posting the decision. A refusal is the exception
// spec §10.3 and §5.2 write down: 400 for a form that cannot be acted on and
// 409 for a job that has moved, both of them with no row written. Those need a
// body to say which field or which state was the problem, and neither is a
// response a reload could turn into a second decision, because the refusal
// happens before anything is stored.

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/engine"
	"github.com/vipinm/sdlc-orchestrator/internal/jobs"
	"github.com/vipinm/sdlc-orchestrator/internal/review"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// prefDiffViewCookie remembers unified-vs-split between visits. It holds a
// display preference and nothing else, which is why it is allowed to be set by
// a POST that writes no row at all.
const prefDiffViewCookie = "sdlc_diff_view"

// prefMaxAge is a year. A preference that expired with the session would have
// to be re-chosen on every restart of `sdlc serve`, which is the thing it
// exists to avoid.
const prefMaxAge = 365 * 24 * time.Hour

// FormModel is what the named templates in _forms.html render from, and the
// contract the pages that invoke them supply: `{{template "decisionForm" .Form}}`.
// It carries the job because every form needs its id for the action URL and its
// state for the hidden `state` field, and the resumable states because the
// resume form renders engine.ResumableStates() rather than a list of its own.
type FormModel struct {
	CSRF            string
	Job             *store.Job
	Gate            string // review.GateFor(Job.State)
	ResumableStates []string
	// DiffView is the view the page is currently showing, carried for a page
	// that wants to name it. diffViewToggle does not read it — see the comment
	// there — so a page with no such notion can invoke the toggle too.
	DiffView string // "unified" | "split"
}

// formModel builds the model above. Pages call it rather than assembling a
// FormModel themselves so the gate and the resumable list have one derivation:
// review.GateFor is the only state-to-gate mapping in this codebase and
// engine.ResumableStates is the only list of resume targets.
func (s *Server) formModel(r *http.Request, j *store.Job, diffView string) FormModel {
	return FormModel{
		CSRF:            csrfToken(r),
		Job:             j,
		Gate:            review.GateFor(j.State),
		ResumableStates: engine.ResumableStates(),
		DiffView:        diffView,
	}
}

// --- the four decisions --------------------------------------------------

// handleApprove records an approval for whatever gate the job's state names.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request, j *store.Job) {
	gate, ok := s.approvalGate(w, r, j)
	if !ok {
		return
	}
	// Reason carries the note, exactly as cmdDecision does when no --reason was
	// given: the engine reads one field for the text a human attached to a
	// decision, and an approval has no rejection reason to compete with it.
	s.record(w, r, j, &store.Approval{
		JobID:    j.ID,
		Gate:     gate,
		Decision: "approve",
		Reason:   strings.TrimSpace(r.PostFormValue("note")),
	})
}

// handleReject records a rejection, and refuses one with no reason.
func (s *Server) handleReject(w http.ResponseWriter, r *http.Request, j *store.Job) {
	gate, ok := s.approvalGate(w, r, j)
	if !ok {
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		// The same rule cli.rejectReason enforces at the terminal, and for the
		// same reason: the text is handed to the next agent verbatim, so an
		// empty rejection tells it only that somebody said no — which is the
		// one thing it cannot act on. Refused before the row is written, so a
		// reasonless rejection leaves nothing behind for the engine to consume.
		s.fail(w, r, http.StatusBadRequest,
			"the reason field is required: a rejection is handed to the next agent verbatim, and an empty one tells it only that somebody said no")
		return
	}
	s.record(w, r, j, &store.Approval{
		JobID:    j.ID,
		Gate:     gate,
		Decision: "reject",
		Reason:   reason,
		// --cancel's web equivalent: reject and delete the branch and worktree
		// rather than sending the job back to FIXING.
		Cancel: formBool(r, "cancel"),
	})
}

// handleResume clears a hold, optionally naming the state to resume into.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request, j *store.Job) {
	to := strings.TrimSpace(r.PostFormValue("to"))
	// Validated before the row is written, against engine's list rather than a
	// copy of it. The form renders the same list as a <select>, so an invalid
	// value is unreachable from the UI; this check is what refuses one posted
	// by hand, which is the only way it can arrive.
	if to != "" && !engine.IsResumableState(to) {
		s.fail(w, r, http.StatusBadRequest, fmt.Sprintf(
			"%q is not a state a job can be resumed into; valid: %s",
			to, strings.Join(engine.ResumableStates(), ", ")))
		return
	}
	if !s.notAlreadyPending(w, r, j, "resume") {
		return
	}
	// Reason is the target state and Note is the instruction for the agent:
	// handleControls reads Reason as the --to target and an empty one means
	// "the state the job stopped in", which is cmdResume's shape exactly.
	s.record(w, r, j, &store.Approval{
		JobID:    j.ID,
		Gate:     "resume",
		Decision: "resume",
		Reason:   to,
		Note:     strings.TrimSpace(r.PostFormValue("note")),
	})
}

// handleCancel cancels a job, and refuses to do it without an explicit
// confirmation.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request, j *store.Job) {
	// promptHold asks `[y/N]` before this same row, because cancelling deletes
	// the worktree: work that is not committed anywhere is gone. One click is
	// the wrong affordance for that, so the form carries a checkbox the
	// operator has to tick and a POST without it is refused here.
	if r.PostFormValue("confirm") != "yes" {
		s.fail(w, r, http.StatusBadRequest,
			"cancelling deletes the job's worktree, so it needs an explicit confirmation — tick the box on the job page and post again")
		return
	}
	if !s.notAlreadyPending(w, r, j, "cancel") {
		return
	}
	s.record(w, r, j, &store.Approval{JobID: j.ID, Gate: "cancel", Decision: "cancel"})
}

// approvalGate returns the gate an approve/reject applies to, or answers 409
// and reports false.
//
// review.GateFor is the one state-to-gate mapping; deriving the gate from the
// job the handler just re-read, rather than from anything the form said, is
// what stops a posted gate name from deciding a gate the job is not at.
// GateHold is not an approval gate: a held job is cleared with resume or
// cancel, so approve and reject must refuse it just as cmdDecision does.
func (s *Server) approvalGate(w http.ResponseWriter, r *http.Request, j *store.Job) (string, bool) {
	gate := review.GateFor(j.State)
	if gate == "" || gate == review.GateHold {
		msg := fmt.Sprintf("%s is in state %s — there is no pending approval gate", j.ID, j.State)
		if gate == review.GateHold {
			msg += "; it is held, so resume or cancel it instead"
		}
		s.fail(w, r, http.StatusConflict, msg)
		return "", false
	}
	if !s.notAlreadyPending(w, r, j, gate) {
		return "", false
	}
	return gate, true
}

// notAlreadyPending refuses a second decision for a gate that already has an
// unconsumed row (AC-14), in the sentence reviewer.promptGate uses.
//
// A second row would race the first: the engine acts on the oldest, so the
// answer given second would be silently discarded — and "silently" is the part
// that matters, because the operator would have watched the job do the opposite
// of what they just clicked.
func (s *Server) notAlreadyPending(w http.ResponseWriter, r *http.Request, j *store.Job, gate string) bool {
	a, err := s.st.PendingApproval(j.ID, gate)
	if err != nil {
		s.log.Printf("reading pending %s approval for %s: %v", gate, j.ID, err)
		s.fail(w, r, http.StatusInternalServerError,
			"the pending decisions could not be read, so nothing was recorded — see the sdlc serve console")
		return false
	}
	if a != nil {
		s.fail(w, r, http.StatusConflict, fmt.Sprintf(
			"a %s decision is already pending for %s; the engine will act on its next tick", gate, j.ID))
		return false
	}
	return true
}

// record writes the row and answers 303 back to the job.
//
// The single place this package touches the approvals table. Every field the
// row carries was set by the caller above; nothing is defaulted here, so a
// reader comparing a handler against cli.cmdDecision is comparing the whole row.
func (s *Server) record(w http.ResponseWriter, r *http.Request, j *store.Job, a *store.Approval) {
	if err := s.st.AddApproval(a); err != nil {
		s.log.Printf("recording %s for %s: %v", a.Decision, j.ID, err)
		s.fail(w, r, http.StatusInternalServerError,
			"the decision could not be recorded — see the sdlc serve console")
		return
	}
	// 303 rather than 302: the reload that follows must be a GET of the job
	// page, and 302 leaves the method up to the browser.
	http.Redirect(w, r, "/jobs/"+url.PathEscape(j.ID), http.StatusSeeOther)
}

// --- submit --------------------------------------------------------------

// handleSubmit creates a job through jobs.Submit and nothing else.
//
// Every rule about which targets exist, what makes a title valid and what an
// empty body defaults to lives in that one function, which `sdlc submit` also
// calls. Re-validating here — even "just" the title — is how the two surfaces
// would come to disagree about what a valid submission is.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	job, err := jobs.Submit(s.cfg, s.st, jobs.SubmitRequest{
		Target: r.PostFormValue("target"),
		Title:  r.PostFormValue("title"),
		Body:   r.PostFormValue("body"),
	})
	if err != nil {
		// The sentinels are matched rather than the message text, which is what
		// jobs documents them for: the two size limits are a 413 and everything
		// else a 400. The message shown is jobs' own, because it already names
		// the limit it enforced or the targets the config does define.
		//
		// A store failure lands in that 400 as well, and is the one case where
		// the status overstates what the submitter can fix. Telling it apart
		// would mean a rule here about which of jobs.Submit's errors are the
		// caller's fault — a second copy of the thing this handler delegates —
		// so the error is logged to the console instead, where the operator
		// running `sdlc serve` is already watching for exactly that.
		code := http.StatusBadRequest
		if errors.Is(err, jobs.ErrTitleTooLong) || errors.Is(err, jobs.ErrBodyTooLong) {
			code = http.StatusRequestEntityTooLarge
		}
		s.log.Printf("submitting a job: %v", err)
		s.fail(w, r, code, err.Error())
		return
	}
	http.Redirect(w, r, "/jobs/"+url.PathEscape(job.ID), http.StatusSeeOther)
}

// --- the diff view preference -------------------------------------------

// handleDiffViewPref stores the unified/split choice in a cookie.
func (s *Server) handleDiffViewPref(w http.ResponseWriter, r *http.Request) {
	view := r.PostFormValue("view")
	// An allowlist, not a passthrough: the cookie is echoed back into the diff
	// handler's choice of template, and a value nothing recognises would leave
	// the viewer stuck on whatever that handler happens to default to.
	if view != "unified" && view != "split" {
		s.fail(w, r, http.StatusBadRequest, "the view must be unified or split")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:  prefDiffViewCookie,
		Value: view,
		Path:  "/",
		// Only the server reads it, so no script needs to, and HttpOnly keeps
		// it out of reach of anything that got a script onto the page.
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(prefMaxAge.Seconds()),
	})
	http.Redirect(w, r, backTo(r), http.StatusSeeOther)
}

// backTo returns the Referer when it is this server's own, and /jobs otherwise.
//
// Redirecting to an unchecked Referer is an open redirect: any page anywhere
// can send the operator's browser through this server's origin to a URL of its
// choosing, which is how a phishing page borrows the address the operator
// trusts. The header is attacker-influenced on every request, so it is treated
// as a hint that has to prove it is same-origin, never as a destination.
func backTo(r *http.Request) string {
	const fallback = "/jobs"
	ref := r.Header.Get("Referer")
	if ref == "" {
		return fallback
	}
	u, err := url.Parse(ref)
	if err != nil {
		return fallback
	}
	// A Referer with a host must name this server. An empty host is a
	// same-origin relative reference, which browsers do not send but which
	// costs nothing to accept.
	if u.Host != "" && u.Host != r.Host {
		return fallback
	}
	// A path that starts with // is a scheme-relative URL: the browser reads
	// //evil.example/x as another origin, so it must be refused even though it
	// arrives looking like a path. Anything that is not rooted is refused too,
	// rather than being resolved against a page this handler cannot know.
	if !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return fallback
	}
	out := u.EscapedPath()
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// --- form helpers --------------------------------------------------------

// formBool reads an HTML checkbox. An unticked box sends nothing at all, so
// presence is most of the answer; the values are listed rather than "anything
// non-empty" so a field carrying "false" cannot read as true.
func formBool(r *http.Request, name string) bool {
	switch strings.ToLower(strings.TrimSpace(r.PostFormValue(name))) {
	case "on", "1", "yes", "true":
		return true
	}
	return false
}
