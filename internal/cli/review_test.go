package cli

// Tests for `sdlc review` (SPEC §4). runReview takes its reader, its writer
// and the "is this a terminal" answer as arguments precisely so these can run
// without a pty: the prompt loop is the path a human's decision travels, and
// a decision path nobody can test is one nobody can trust.

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/review"
	"github.com/vipinm/sdlc-orchestrator/internal/store"

	_ "modernc.org/sqlite"
)

// --- fixture --------------------------------------------------------------

type reviewEnv struct {
	t   *testing.T
	tmp string
	cfg *config.Config
	st  *store.Store
}

func newReviewEnv(t *testing.T) *reviewEnv {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.Orchestrator.DataDir = filepath.Join(tmp, "data")
	cfg.Database.Path = filepath.Join(tmp, "data", "sdlc.db")
	cfg.Targets = map[string]config.Target{
		"demo": {
			RepoPath:      filepath.Join(tmp, "repo"),
			DefaultBranch: "main",
			BranchPrefix:  "sdlc/",
			Ship:          config.ShipCfg{Command: []string{"scripts", "ship.sh"}},
		},
	}
	st, err := store.Open(cfg.Database.Path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &reviewEnv{t: t, tmp: tmp, cfg: cfg, st: st}
}

// job inserts a job already sitting in state. No worktree and no artifacts
// exist: the renderer is required to produce a readable document from whatever
// it can find, so the fixture deliberately gives it very little.
func (e *reviewEnv) job(state, title string) *store.Job {
	e.t.Helper()
	id, err := e.st.NextJobID("JOB")
	if err != nil {
		e.t.Fatal(err)
	}
	j := &store.Job{
		ID: id, Target: "demo", IssueTitle: title, IssueBody: "do the thing",
		Branch: "sdlc/" + id, WorktreePath: filepath.Join(e.tmp, "repo", ".worktrees", id),
		State: state,
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

// run drives the command the way Main would, minus the terminal.
func (e *reviewEnv) run(args []string, input string, tty bool) (int, string) {
	e.t.Helper()
	var out strings.Builder
	code := runReview(e.cfg, args, strings.NewReader(input), &out, tty)
	return code, out.String()
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
// store exposes only "the next pending row for one gate", and the property
// under test here is a counting one: a prompt that answers once must leave
// exactly one row, whatever gate it names.
func (e *reviewEnv) approvals() []approvalRow {
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

// only asserts the table holds exactly one row and returns it.
func (e *reviewEnv) only() approvalRow {
	e.t.Helper()
	rows := e.approvals()
	if len(rows) != 1 {
		e.t.Fatalf("approvals table holds %d row(s), want exactly 1: %+v", len(rows), rows)
	}
	return rows[0]
}

// captureStderr collects what the command wrote to os.Stderr. The error
// sentences are part of the contract — they are what tells an operator they
// asked about the wrong job — so they are asserted rather than discarded.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() {
		os.Stderr = old
	}()
	fn()
	w.Close()
	return <-done
}

// --- rendering ------------------------------------------------------------

// Every state a human can be waiting in must render, and must render the part
// that says what to do about it. `sdlc status` covered three of these states
// and printed context for exactly one of them.
func TestReviewRendersEveryGate(t *testing.T) {
	cases := []struct {
		state string
		gate  string
		want  string
	}{
		{"AWAITING_SPEC_APPROVAL", review.GateSpec, "Approve the spec and plan before any code is written."},
		{"AWAITING_CODE_APPROVAL", review.GateCode, "Approve the implementation before it goes to build and test."},
		{"AWAITING_MERGE_APPROVAL", review.GateMerge, "Approve merging this change into main."},
		{"AWAITING_RELEASE_APPROVAL", review.GateRelease, "Approve the release."},
		{"ESCALATED", review.GateHold, "This job stopped and needs a decision."},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			e := newReviewEnv(t)
			j := e.job(tc.state, "render "+tc.gate)
			code, out := e.run([]string{j.ID}, "", false)
			if code != 0 {
				t.Fatalf("exit=%d, want 0\n%s", code, out)
			}
			for _, w := range []string{j.ID, tc.want, "## What happens next"} {
				if !strings.Contains(out, w) {
					t.Errorf("%s document is missing %q:\n%s", tc.gate, w, out)
				}
			}
		})
	}
}

// Reading is not deciding. With stdin not a terminal — under `go test`, cron,
// CI, or `sdlc review JOB-1 | less` — and with --no-prompt on a terminal, the
// command prints and leaves the database and the filesystem exactly as it
// found them.
func TestReviewWithoutPromptingChangesNothing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		tty  bool
	}{
		{"stdin is not a terminal", nil, false},
		{"--no-prompt on a terminal", []string{"--no-prompt"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newReviewEnv(t)
			j := e.job("AWAITING_SPEC_APPROVAL", "read only")
			// An answer is offered; nothing may consume it.
			code, out := e.run(append(tc.args, j.ID), "a\n\n", tc.tty)
			if code != 0 {
				t.Fatalf("exit=%d, want 0\n%s", code, out)
			}
			if !strings.Contains(out, "## What happens next") {
				t.Errorf("the document was not printed:\n%s", out)
			}
			if strings.Contains(out, "[a/r/d/s/q]") {
				t.Errorf("prompted when it must not have:\n%s", out)
			}
			if rows := e.approvals(); len(rows) != 0 {
				t.Errorf("wrote %d approval row(s) without being asked: %+v", len(rows), rows)
			}
			if _, err := os.Stat(review.Dir(e.cfg.Orchestrator.DataDir, j.ID)); err == nil {
				t.Error("`sdlc review` wrote into the review directory; only the engine may")
			}
		})
	}
}

// The engine writes the gate document when the job parks, so a path printed
// here always names a file that exists. Nothing is offered when nothing does.
func TestReviewNamesTheDocumentOnlyWhenItIsOnDisk(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "document on disk")

	if _, out := e.run([]string{j.ID}, "", false); strings.Contains(out, "document: ") {
		t.Errorf("named a document that was never written:\n%s", out)
	}

	doc := review.DocPath(e.cfg.Orchestrator.DataDir, j.ID, review.GateMerge)
	if err := os.MkdirAll(filepath.Dir(doc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(doc, []byte("# written by the engine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out := e.run([]string{j.ID}, "", false)
	if !strings.Contains(out, "document: "+doc) {
		t.Errorf("did not name the document the engine wrote:\n%s", out)
	}
}

// --- selection ------------------------------------------------------------

func TestReviewRefusesAJobThatIsNotWaiting(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("IMPLEMENTING", "still working")
	var code int
	var out string
	errOut := captureStderr(t, func() { code, out = e.run([]string{j.ID}, "", true) })
	if code != 1 {
		t.Errorf("exit=%d, want 1", code)
	}
	if !strings.Contains(errOut, "is in state IMPLEMENTING — nothing is waiting on you") {
		t.Errorf("stderr = %q, want the \"nothing is waiting on you\" sentence", errOut)
	}
	if out != "" {
		t.Errorf("printed a document for a job that is not waiting:\n%s", out)
	}
	if rows := e.approvals(); len(rows) != 0 {
		t.Errorf("wrote %d approval row(s): %+v", len(rows), rows)
	}
}

func TestReviewReportsAnUnknownJob(t *testing.T) {
	e := newReviewEnv(t)
	var code int
	errOut := captureStderr(t, func() { code, _ = e.run([]string{"JOB-404"}, "", true) })
	if code != 1 {
		t.Errorf("exit=%d, want 1", code)
	}
	if !strings.Contains(errOut, "error: job not found: JOB-404") {
		t.Errorf("stderr = %q, want the job-not-found sentence", errOut)
	}
}

// With no id the command walks everything waiting on a human — the only way
// to find a job that parked overnight without already knowing its id.
func TestReviewWithNoIDVisitsEveryWaitingJob(t *testing.T) {
	e := newReviewEnv(t)
	spec := e.job("AWAITING_SPEC_APPROVAL", "first waiting job")
	busy := e.job("IMPLEMENTING", "not waiting on anybody")
	held := e.job("ESCALATED", "second waiting job")

	code, out := e.run(nil, "", false)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\n%s", code, out)
	}
	for _, id := range []string{spec.ID, held.ID} {
		if !strings.Contains(out, "# "+id+" — ") {
			t.Errorf("%s is waiting but its document was not printed:\n%s", id, out)
		}
	}
	if strings.Contains(out, "# "+busy.ID+" — ") {
		t.Errorf("%s is not waiting on anybody but was printed:\n%s", busy.ID, out)
	}
	if !strings.Contains(out, jobRule) {
		t.Errorf("two documents were printed with no rule between them:\n%s", out)
	}
}

func TestReviewSaysSoWhenNothingIsWaiting(t *testing.T) {
	e := newReviewEnv(t)
	e.job("IMPLEMENTING", "busy")
	code, out := e.run(nil, "", true)
	if code != 0 {
		t.Fatalf("exit=%d, want 0", code)
	}
	if strings.TrimSpace(out) != "no job is waiting on you" {
		t.Errorf("output = %q, want the \"no job is waiting on you\" line", out)
	}
}

// --- the approval prompt --------------------------------------------------

func TestReviewApproveWritesExactlyOneApprovalRow(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("AWAITING_SPEC_APPROVAL", "approve me")

	code, out := e.run([]string{j.ID}, "a\nlooks right\n", true)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\n%s", code, out)
	}
	got := e.only()
	want := approvalRow{jobID: j.ID, gate: review.GateSpec, decision: "approve", reason: "looks right"}
	if got != want {
		t.Errorf("approval row = %+v, want %+v", got, want)
	}
	if !strings.Contains(out, j.ID+" approve recorded for spec gate") {
		t.Errorf("the decision was not confirmed to the operator:\n%s", out)
	}
}

// A rejection is handed to the next agent verbatim, so an empty one tells it
// only that somebody said no — the one thing it cannot act on.
func TestReviewEmptyRejectReasonRepromptsUntilGiven(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("AWAITING_CODE_APPROVAL", "reject me")

	code, out := e.run([]string{j.ID}, "r\n\n\n  \nthe copy in the dialog is wrong\n", true)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\n%s", code, out)
	}
	got := e.only()
	want := approvalRow{
		jobID: j.ID, gate: review.GateCode, decision: "reject",
		reason: "the copy in the dialog is wrong",
	}
	if got != want {
		t.Errorf("approval row = %+v, want %+v", got, want)
	}
	if n := strings.Count(out, "a reason is required"); n != 3 {
		t.Errorf("re-prompted %d time(s) for the three empty answers, want 3:\n%s", n, out)
	}
}

// An interactive rejection never cancels: --cancel deletes the branch and the
// worktree, and one keystroke is the wrong affordance for that.
func TestInteractiveRejectNeverCancelsTheJob(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "reject but do not destroy")
	if _, out := e.run([]string{j.ID}, "r\nnot yet\n", true); !strings.Contains(out, "reject recorded") {
		t.Fatalf("no rejection recorded:\n%s", out)
	}
	if got := e.only(); got.cancel {
		t.Errorf("interactive reject set Cancel: %+v", got)
	}
}

func TestReviewUnrecognisedAnswerRepromptsAndDecidesNothing(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("AWAITING_SPEC_APPROVAL", "fat fingers")

	code, out := e.run([]string{j.ID}, "x\n\nq\n", true)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, `unrecognised answer "x"`) {
		t.Errorf("the bad answer was not reported back:\n%s", out)
	}
	// The bad answer, the empty line and the quit each re-print the question.
	if n := strings.Count(out, "[a/r/d/s/q]"); n != 3 {
		t.Errorf("prompted %d time(s), want 3:\n%s", n, out)
	}
	if rows := e.approvals(); len(rows) != 0 {
		t.Errorf("a typo produced %d approval row(s): %+v", len(rows), rows)
	}
}

// skip and quit are the two exits that must leave everything as they found
// it; quit additionally leaves the rest of the queue unvisited.
func TestReviewSkipAndQuitWriteNothing(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantSecond  bool
		description string
	}{
		{"skip", "s\ns\n", true, "skip moves to the next job"},
		{"quit", "q\n", false, "quit leaves the rest of the queue untouched"},
		{"end of input", "", false, "Ctrl-D is a quit, not a decision"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newReviewEnv(t)
			first := e.job("AWAITING_SPEC_APPROVAL", "first")
			second := e.job("AWAITING_MERGE_APPROVAL", "second")

			code, out := e.run(nil, tc.input, true)
			if code != 0 {
				t.Fatalf("exit=%d, want 0\n%s", code, out)
			}
			if !strings.Contains(out, "# "+first.ID+" — ") {
				t.Errorf("the first job was not printed:\n%s", out)
			}
			if got := strings.Contains(out, "# "+second.ID+" — "); got != tc.wantSecond {
				t.Errorf("second job printed = %v, want %v (%s):\n%s", got, tc.wantSecond, tc.description, out)
			}
			if rows := e.approvals(); len(rows) != 0 {
				t.Errorf("%s wrote %d approval row(s): %+v", tc.name, len(rows), rows)
			}
		})
	}
}

// A second row would race the first one the engine has not consumed yet, and
// the engine acts on the oldest — so the answer typed here could be silently
// discarded. Say what is already pending instead of asking again.
func TestReviewReportsAPendingDecisionInsteadOfPrompting(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("AWAITING_SPEC_APPROVAL", "already decided")
	// Through the one writer, exactly as `sdlc approve` would have.
	if err := recordDecision(e.st, &store.Approval{
		JobID: j.ID, Gate: review.GateSpec, Decision: "approve",
	}); err != nil {
		t.Fatal(err)
	}

	code, out := e.run([]string{j.ID}, "r\nchanged my mind\n", true)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "a spec decision is already pending for "+j.ID) {
		t.Errorf("the pending decision was not reported:\n%s", out)
	}
	if strings.Contains(out, "[a/r/d/s/q]") {
		t.Errorf("prompted over a decision that is already pending:\n%s", out)
	}
	if got := e.only(); got.decision != "approve" {
		t.Errorf("the pending decision was overwritten: %+v", got)
	}
}

// --- the hold prompt ------------------------------------------------------

// A held job is cleared with resume or cancel, not approve/reject, and the
// letters deliberately do not overlap: muscle memory from one gate must not
// record a decision at the other.
func TestHeldJobPromptsResumeRatherThanApprove(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("ESCALATED", "needs a human")

	code, out := e.run([]string{j.ID}, "r\nthose fixtures are stale, update them\n", true)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "["+j.ID+" · hold] resume / cancel / diff / skip / quit  [r/c/d/s/q]: ") {
		t.Errorf("the hold prompt does not offer resume/cancel:\n%s", out)
	}
	if strings.Contains(out, "approve / reject") {
		t.Errorf("the hold prompt offers approve/reject, which do nothing to a held job:\n%s", out)
	}
	got := e.only()
	want := approvalRow{
		jobID: j.ID, gate: "resume", decision: "resume",
		note: "those fixtures are stale, update them",
	}
	if got != want {
		t.Errorf("approval row = %+v, want %+v", got, want)
	}
	// Reason is the --to target for a resume row. A note in it would be read
	// as a state name and the resume refused.
	if got.reason != "" {
		t.Errorf("resume row carries Reason %q; it must stay empty", got.reason)
	}
}

// Cancelling deletes the worktree, so it is confirmed. Anything but y goes
// back to the question rather than through with it.
func TestHoldCancelIsConfirmedBeforeItIsRecorded(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("TIMED_OUT", "cancel me, maybe")

	code, out := e.run([]string{j.ID}, "c\nn\nc\ny\n", true)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\n%s", code, out)
	}
	if n := strings.Count(out, "cancel "+j.ID+" and delete its worktree? [y/N]: "); n != 2 {
		t.Errorf("asked for confirmation %d time(s), want 2:\n%s", n, out)
	}
	got := e.only()
	want := approvalRow{jobID: j.ID, gate: "cancel", decision: "cancel"}
	if got != want {
		t.Errorf("approval row = %+v, want %+v", got, want)
	}
}

// `d` re-renders the same job with the patch inlined and asks again. It is
// the one answer that must not be a decision: an operator who wants to see
// more has not yet said anything.
func TestReviewDiffReprintsTheDocumentAndAsksAgain(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "show me the patch")

	code, out := e.run([]string{j.ID}, "d\nq\n", true)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\n%s", code, out)
	}
	if n := strings.Count(out, "## What happens next"); n != 2 {
		t.Errorf("the document was printed %d time(s), want 2 (once, then again for `d`):\n%s", n, out)
	}
	if n := strings.Count(out, "[a/r/d/s/q]"); n != 2 {
		t.Errorf("prompted %d time(s), want 2:\n%s", n, out)
	}
	if rows := e.approvals(); len(rows) != 0 {
		t.Errorf("`d` recorded %d approval row(s): %+v", len(rows), rows)
	}
}

// /dev/null is a character device, so a mode-bits-only terminal check calls it
// a terminal. That is exactly what a service manager, a cron entry or an
// explicit `< /dev/null` hands the process: the fan-out would print one
// document, read EOF, take it as "quit" and stop, reviewing one job out of
// however many were waiting.
func TestDevNullIsNotATerminal(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer null.Close()
	saved := os.Stdin
	os.Stdin = null
	defer func() { os.Stdin = saved }()
	if interactive() {
		t.Error("interactive() reports a terminal for os.DevNull; a headless run would be prompted")
	}
}

// The regular-file and pipe cases must keep answering false too — this is the
// check that decides whether the command may write to the database at all.
func TestRedirectedInputIsNotATerminal(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "answers"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	saved := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = saved }()
	if interactive() {
		t.Error("interactive() reports a terminal for a regular file")
	}
}
