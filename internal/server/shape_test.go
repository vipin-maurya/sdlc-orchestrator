package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/vipinm/sdlc-orchestrator/internal/engine"
)

// TestEveryEngineStateHasAPhase is the exhaustiveness walk for the pipeline
// strip. It is the same argument the block model rests on: a state with no
// phase draws an empty strip, which looks exactly like a job that has not
// started, and comparing rendered output would pass either way.
//
// The four states that deliberately have none — the terminal and held ones —
// are named here rather than inferred, so adding a state to the engine fails
// this test instead of quietly falling into the exempt set.
func TestEveryEngineStateHasAPhase(t *testing.T) {
	noPhase := map[string]bool{
		"CANCELLED": true, "FAILED": true,
		"ESCALATED": true, "TIMED_OUT": true, "BLOCKED_ON_QUOTA": true,
	}
	// Every state the engine can put a job in, from its own list plus the
	// ones resume cannot target.
	states := append(engine.ResumableStates(),
		"CREATED", "AWAITING_SPEC_APPROVAL", "AWAITING_CODE_APPROVAL",
		"AWAITING_MERGE_APPROVAL", "AWAITING_RELEASE_APPROVAL", "COMPLETED",
		"CANCELLED", "FAILED", "ESCALATED", "TIMED_OUT", "BLOCKED_ON_QUOTA")

	for _, st := range states {
		at := phaseIndex(st, "")
		if noPhase[st] {
			if at >= 0 {
				t.Errorf("%s was given phase %d; a stopped job is located by the state it stopped in", st, at)
			}
			continue
		}
		if at < 0 {
			t.Errorf("%s has no phase, so the pipeline strip draws nothing for it", st)
		}
	}
}

// TestAStoppedJobIsPlacedByWhereItStopped is the whole reason phaseIndex takes
// two arguments: "how far did this get" is not answered by CANCELLED.
func TestAStoppedJobIsPlacedByWhereItStopped(t *testing.T) {
	strip := pipelineFor("FAILED", "TESTING")
	var now string
	for _, p := range strip {
		if p.Class == "now" {
			now = p.Label
		}
	}
	if now != "TESTING" {
		t.Errorf("a job that failed in TESTING is drawn at %q", now)
	}
	if pct := phasePct("FAILED", "TESTING"); pct == 0 {
		t.Error("a job that failed most of the way through drew an empty bar")
	}
	// And a job that never got anywhere still draws nothing, rather than
	// guessing.
	for _, p := range pipelineFor("FAILED", "") {
		if p.Class != "todo" {
			t.Errorf("an unplaceable state drew %q", p.Class)
		}
	}
}

// TestClassesComeFromAClosedSet is AC-17's sibling for the two helpers that
// feed a class attribute. Neither may ever echo what it was given: severity,
// state and gate all arrive from data this UI does not author.
func TestClassesComeFromAClosedSet(t *testing.T) {
	allowedState := map[string]bool{"run": true, "ok": true, "warn": true, "bad": true}
	allowedGate := map[string]bool{"": true, "warn": true, "bad": true, "info": true}
	hostile := `x" onload="alert(1)`
	for _, in := range []string{"PLANNING", "COMPLETED", "FAILED", "AWAITING_MERGE_APPROVAL", hostile, ""} {
		if got := stateClass(in); !allowedState[got] {
			t.Errorf("stateClass(%q) = %q, which is not one of this page's classes", in, got)
		}
	}
	for _, in := range []string{"merge", "hold", "spec", hostile, ""} {
		if got := gateClass(in); !allowedGate[got] {
			t.Errorf("gateClass(%q) = %q, which is not one of this page's classes", in, got)
		}
	}
}

// TestJobListFiltersAreLinkable covers the property the chips are links for: a
// filtered list has to survive being reloaded and pasted to somebody else.
func TestJobListFiltersAreLinkable(t *testing.T) {
	e := newEnv(t)
	e.job("AWAITING_MERGE_APPROVAL", "waiting-job")
	e.job("TESTING", "running-job")
	e.job("COMPLETED", "finished-job")

	// The default hides finished work, which is the filtering this page has
	// always done — now named rather than implied.
	body := getOK(t, e, "/jobs")
	if strings.Contains(body, "finished-job") {
		t.Error("the default list showed a completed job")
	}
	for _, want := range []string{"waiting-job", "running-job"} {
		if !strings.Contains(body, want) {
			t.Errorf("the default list is missing %q", want)
		}
	}

	body = getOK(t, e, "/jobs?show=done")
	if !strings.Contains(body, "finished-job") {
		t.Error("?show=done did not show the completed job")
	}
	if strings.Contains(body, "running-job") {
		t.Error("?show=done showed a job that is not done")
	}

	body = getOK(t, e, "/jobs?show=all")
	for _, want := range []string{"waiting-job", "running-job", "finished-job"} {
		if !strings.Contains(body, want) {
			t.Errorf("?show=all is missing %q", want)
		}
	}
	// A filtered list tells app.js not to reload when a job it is hiding shows
	// up in the stream, which would otherwise loop forever.
	if !strings.Contains(body, `data-show="all"`) {
		t.Error("the list does not carry the active filter, so the live reload cannot tell it is filtered")
	}
}

// TestGatePageCarriesTheQueue is the review layout's one load-bearing claim:
// deciding a gate does not mean going back to the list to find the next one.
func TestGatePageCarriesTheQueue(t *testing.T) {
	e := newEnv(t)
	here := e.job("AWAITING_MERGE_APPROVAL", "the one being decided")
	next := e.job("AWAITING_SPEC_APPROVAL", "the one waiting behind it")
	running := e.job("TESTING", "not waiting on anybody")

	res := e.get("/jobs/" + here.ID + "/gate")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET gate = %d", res.StatusCode)
	}
	body := e.body(res)
	for _, want := range []string{
		`href="/jobs/` + next.ID + `/gate"`, // the queue reaches the next decision
		`href="/jobs/` + running.ID + `"`,   // and the running list reaches the rest
		"Findings checklist",
		"Gate facts",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the gate page is missing %q", want)
		}
	}
	// The decision bar's buttons submit the panel's forms rather than being a
	// second form: two forms posting the same decision would be two places for
	// the hidden state field to be wrong.
	if !strings.Contains(body, `form="approve-form"`) {
		t.Error("the decision bar does not submit the approve form")
	}
	if n := strings.Count(body, `action="/jobs/`+here.ID+`/approve"`); n != 1 {
		t.Errorf("the page carries %d approve forms; there must be exactly one", n)
	}
}

// TestGridHeaderAnswersToItsColumnClasses pins the half of the narrow-width
// layout that lives in the template.
//
// Below 900px the grid drops to three columns by hiding .target, .phase and
// .gate. That works only if the header cells answer to the same classes as
// the data cells: a header that did not would keep all six labels over three
// columns, wrap onto a second line, and label whatever slid into the vacated
// place. The CSS cannot be asserted here, but this is the half that rots — a
// column added to the row and not to the header would reintroduce it.
func TestGridHeaderAnswersToItsColumnClasses(t *testing.T) {
	e := newEnv(t)
	e.job("AWAITING_MERGE_APPROVAL", "a job to draw a row for")
	body := getOK(t, e, "/jobs")

	head := between(t, body, `<div class="head">`, "</div>")
	for _, col := range []string{"title", "target", "phase", "gate", "age"} {
		if !strings.Contains(head, `class="`+col+`"`) && !strings.Contains(head, `class="`+col+` `) {
			t.Errorf("the grid header has no %q cell, so hiding that column would leave its label behind", col)
		}
	}
}

// between returns the text between the first open and the next close after it.
func between(t *testing.T, s, open, close string) string {
	t.Helper()
	i := strings.Index(s, open)
	if i < 0 {
		t.Fatalf("the page does not contain %q", open)
	}
	rest := s[i+len(open):]
	k := strings.Index(rest, close)
	if k < 0 {
		t.Fatalf("%q is never closed by %q", open, close)
	}
	return rest[:k]
}

// TestIsRunningIsTheComplementOfTheStoppedStates is finding #1's regression.
//
// BLOCKED_ON_QUOTA was absent from the old literal exclusion list, so a job
// sleeping off a rate limit fell through to "running" and was listed under the
// review queue as work in progress with nothing behind it. The sets in
// shape.go are now one source of truth, and this walks the engine's whole
// state list against them so the next state to be added has to choose.
func TestIsRunningIsTheComplementOfTheStoppedStates(t *testing.T) {
	stopped := []string{
		"COMPLETED", "CANCELLED", "FAILED",
		"AWAITING_SPEC_APPROVAL", "AWAITING_CODE_APPROVAL",
		"AWAITING_MERGE_APPROVAL", "AWAITING_RELEASE_APPROVAL",
		"ESCALATED", "TIMED_OUT",
		// No gate, no human to wait for, and no work dispatched either: the
		// engine skips it while the backoff timer runs.
		"BLOCKED_ON_QUOTA",
	}
	for _, st := range stopped {
		if isRunning(st) {
			t.Errorf("%s counts as running, but the engine has no work queued for it", st)
		}
	}
	// Every state resume can send a job into is a state work runs in.
	for _, st := range engine.ResumableStates() {
		if !isRunning(st) {
			t.Errorf("%s does not count as running, but the engine dispatches work for it", st)
		}
	}
	if !isRunning("CREATED") {
		t.Error("a freshly submitted job does not count as running")
	}
}

// TestABlockedJobIsNotListedAsRunning is the same finding at the page level:
// the count, the chip and the rows all have to agree that nobody is being
// asked to do anything about a rate-limited job.
func TestABlockedJobIsNotListedAsRunning(t *testing.T) {
	e := newEnv(t)
	e.job("BLOCKED_ON_QUOTA", "waiting out a rate limit")
	e.job("TESTING", "actually running")

	body := getOK(t, e, "/jobs?show=running")
	if strings.Contains(body, "waiting out a rate limit") {
		t.Error("the running filter listed a job the engine is not dispatching work for")
	}
	if !strings.Contains(body, "actually running") {
		t.Error("the running filter dropped a job that is running")
	}
	// It is still open work, reachable under its own chip rather than hidden.
	blocked := getOK(t, e, "/jobs?show=blocked")
	if !strings.Contains(blocked, "waiting out a rate limit") {
		t.Error("the blocked filter did not list the blocked job")
	}
	if strings.Contains(blocked, "actually running") {
		t.Error("the blocked filter listed a job that is not blocked")
	}
	if !strings.Contains(getOK(t, e, "/jobs"), "waiting out a rate limit") {
		t.Error("the default open list hid the blocked job")
	}
}
