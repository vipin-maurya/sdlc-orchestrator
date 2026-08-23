package engine

// Tests for the visible, interactive approval gates (SPEC §3 and §5): the
// console announcement a parked job produces, the reminder timer behind it,
// and the two optional human checkpoints `policies.human_gates` adds.
//
// The harness is engine_test.go's — newEnv builds the whole fake pipeline,
// runEngineLogged captures the console an operator would have been watching,
// and promptsFor reads back what an agent was actually told.

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/review"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// --- helpers --------------------------------------------------------------

// parkAt drops a job straight into a gate state. The announcement tests care
// about what the console does with a parked job, not about how it got there,
// and a synthetic park keeps them to a handful of ticks with no agent runs —
// which is also what makes them deterministic.
func (e *env) parkAt(state, title string) *store.Job {
	e.t.Helper()
	j := e.submit(title)
	j.State = state
	j.StateEnteredAt = time.Now()
	if err := e.st.UpdateJob(j); err != nil {
		e.t.Fatal(err)
	}
	return j
}

// tickingEngine is an engine the test drives one tick at a time, with its
// console captured. Nothing here calls Run, so the reminder tests never wait
// on wall-clock time: they age the notice instead (see backdateNotice).
func (e *env) tickingEngine() (*Engine, *syncBuf) {
	e.t.Helper()
	buf := &syncBuf{}
	return New(e.cfg, e.st, log.New(buf, "", 0)), buf
}

// backdateNotice ages the remembered notice by d, which is how these tests ask
// "what happens once the interval has passed" without sleeping for it.
func backdateNotice(t *testing.T, eng *Engine, id string, d time.Duration) time.Time {
	t.Helper()
	eng.mu.Lock()
	defer eng.mu.Unlock()
	n, ok := eng.announced[id]
	if !ok {
		t.Fatalf("no notice recorded for %s", id)
	}
	n.last = n.last.Add(-d)
	eng.announced[id] = n
	return n.last
}

func noticeLast(t *testing.T, eng *Engine, id string) (time.Time, bool) {
	t.Helper()
	eng.mu.Lock()
	defer eng.mu.Unlock()
	n, ok := eng.announced[id]
	return n.last, ok
}

// announcementCounts splits the two notice kinds apart. "STILL WAITING FOR
// YOU" contains "WAITING FOR YOU", so counting the shorter string alone
// reports a reminder as a fresh announcement and every count is off by one.
func announcementCounts(out string) (first, reminders int) {
	reminders = strings.Count(out, "STILL WAITING FOR YOU")
	return strings.Count(out, "WAITING FOR YOU") - reminders, reminders
}

// --- component C: the two optional gates ----------------------------------

// With `human_gates: [spec]` the job must stop after design review and before
// any code exists — that earlier checkpoint is the whole point of the key.
func TestSpecGateParksTheJobBeforeAnyCode(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.HumanGates = []string{"spec"}
	j := e.submit("spec gate stops the job")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SAwaitSpec {
		t.Fatalf("state=%s, want %s; hold=%q", got.State, SAwaitSpec, got.HoldReason)
	}
	if p := e.promptsFor(j.ID, SImplementing); len(p) != 0 {
		t.Errorf("implementation ran %d time(s) before the spec was approved", len(p))
	}
	// The engine, not the CLI, writes the document: the path the console
	// prints has to name a file that is already there.
	doc := review.DocPath(e.cfg.Orchestrator.DataDir, j.ID, review.GateSpec)
	if _, err := os.Stat(doc); err != nil {
		t.Errorf("spec gate document %s was not written: %v", doc, err)
	}
	if d := e.eventDetails(j.ID); !strings.Contains(d, "awaiting_spec_approval") {
		t.Errorf("no awaiting_spec_approval note in the event log:\n%s", d)
	}
}

// The code gate is the same checkpoint one loop later: the change exists and
// has passed code review, but nothing has been built or merged yet.
func TestCodeGateParksTheJobBeforeBuild(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.HumanGates = []string{"code"}
	j := e.submit("code gate stops the job")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SAwaitCode {
		t.Fatalf("state=%s, want %s; hold=%q", got.State, SAwaitCode, got.HoldReason)
	}
	if !e.artifactExists(j.ID, "implementation.json") {
		t.Error("the code gate must be reached with the implementation already in hand")
	}
	doc := review.DocPath(e.cfg.Orchestrator.DataDir, j.ID, review.GateCode)
	if _, err := os.Stat(doc); err != nil {
		t.Errorf("code gate document %s was not written: %v", doc, err)
	}
	if d := e.eventDetails(j.ID); !strings.Contains(d, "awaiting_code_approval") {
		t.Errorf("no awaiting_code_approval note in the event log:\n%s", d)
	}
}

// The default config must behave exactly as it did before the feature: an
// unattended run does not acquire a new place to stop because the states now
// exist.
func TestNoHumanGateWhenPolicyIsEmpty(t *testing.T) {
	e := newEnv(t)
	j := e.submit("no human gates configured")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SAwaitMerge {
		t.Fatalf("state=%s, want %s; hold=%q", got.State, SAwaitMerge, got.HoldReason)
	}
	details := e.eventDetails(j.ID)
	for _, s := range []string{SAwaitSpec, SAwaitCode} {
		if strings.Contains(details, s) {
			t.Errorf("job passed through %s with human_gates empty:\n%s", s, details)
		}
	}
}

// --- component C: what each decision does ---------------------------------

func TestSpecApproveResumesTheImplementation(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.HumanGates = []string{"spec"}
	j := e.submit("spec is approved")
	e.runEngine()
	if s := e.jobState(j.ID).State; s != SAwaitSpec {
		t.Fatalf("setup: state=%s", s)
	}

	e.st.AddApproval(&store.Approval{JobID: j.ID, Gate: review.GateSpec, Decision: "approve"})
	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SAwaitMerge {
		t.Fatalf("after spec approval: state=%s hold=%q", got.State, got.HoldReason)
	}
	if p := e.promptsFor(j.ID, SImplementing); len(p) != 1 {
		t.Errorf("implementation ran %d time(s) after approval, want 1", len(p))
	}
}

// A rejected spec goes back to PLANNING with the objection attached, and the
// planner must be handed the reason verbatim — a rejection the next agent
// cannot read is just an unexplained restart.
func TestSpecRejectRePlansWithTheReasonVerbatim(t *testing.T) {
	e := newEnv(t)
	// The re-plan must be handed the spec and plan it is revising.
	t.Setenv("FAKE_PLAN_REQUIRE_CONTEXT", "1")
	e.cfg.Policies.HumanGates = []string{"spec"}
	j := e.submit("spec is rejected")
	e.runEngine()
	if s := e.jobState(j.ID).State; s != SAwaitSpec {
		t.Fatalf("setup: state=%s", s)
	}

	const reason = "use the existing FooCache instead of adding a second one"
	e.st.AddApproval(&store.Approval{
		JobID: j.ID, Gate: review.GateSpec, Decision: "reject", Reason: reason,
	})
	e.runEngine()
	got := e.jobState(j.ID)
	// The gate is still configured, so the revised plan parks at it again.
	if got.State != SAwaitSpec {
		t.Fatalf("after spec rejection: state=%s hold=%q", got.State, got.HoldReason)
	}
	prompts := e.promptsFor(j.ID, SPlanning)
	if len(prompts) != 2 {
		t.Fatalf("PLANNING ran %d time(s), want 2 (the original plus the re-plan)", len(prompts))
	}
	if !strings.Contains(prompts[1], reason) {
		t.Errorf("the re-plan prompt does not quote the rejection reason:\n%s", prompts[1])
	}
	// Scoped to one attempt, exactly as handleFixing scopes its own.
	if got.Counters.HumanRejectReason != "" {
		t.Errorf("HumanRejectReason=%q after a successful re-plan, want it cleared",
			got.Counters.HumanRejectReason)
	}
	// A human "no" must not spend the budget that bounds automated rework:
	// two human rejections would otherwise escalate the job.
	if got.Counters.DesignReviewRounds != 0 {
		t.Errorf("design_review_rounds=%d after a human rejection, want 0",
			got.Counters.DesignReviewRounds)
	}
}

func TestCodeApproveSendsTheChangeToBuild(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.HumanGates = []string{"code"}
	j := e.submit("code is approved")
	e.runEngine()
	if s := e.jobState(j.ID).State; s != SAwaitCode {
		t.Fatalf("setup: state=%s", s)
	}

	e.st.AddApproval(&store.Approval{JobID: j.ID, Gate: review.GateCode, Decision: "approve"})
	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SAwaitMerge {
		t.Fatalf("after code approval: state=%s hold=%q", got.State, got.HoldReason)
	}
	if got.Counters.FixAttempts != 0 {
		t.Errorf("fix_attempts=%d after an approval, want 0", got.Counters.FixAttempts)
	}
}

// A rejection at the code gate is the merge gate's rejection one loop earlier:
// a fix round with the human's reason as its instruction.
func TestCodeRejectRoutesToFixingWithTheReason(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.HumanGates = []string{"code"}
	j := e.submit("code is rejected")
	e.runEngine()
	if s := e.jobState(j.ID).State; s != SAwaitCode {
		t.Fatalf("setup: state=%s", s)
	}

	const reason = "this duplicates the retry loop that already lives in httpx"
	e.st.AddApproval(&store.Approval{
		JobID: j.ID, Gate: review.GateCode, Decision: "reject", Reason: reason,
	})
	e.runEngine()
	got := e.jobState(j.ID)
	// The fix agent runs, the pipeline re-verifies, and the job parks at merge.
	if got.State != SAwaitMerge {
		t.Fatalf("after code rejection: state=%s hold=%q", got.State, got.HoldReason)
	}
	if got.Counters.FixAttempts != 1 {
		t.Errorf("fix_attempts=%d, want 1", got.Counters.FixAttempts)
	}
	prompts := e.promptsFor(j.ID, SFixing)
	if len(prompts) != 1 {
		t.Fatalf("FIXING ran %d time(s), want 1", len(prompts))
	}
	if !strings.Contains(prompts[0], reason) {
		t.Errorf("the fix prompt does not quote the rejection reason:\n%s", prompts[0])
	}
	if got.Counters.CodeReviewRounds != 0 {
		t.Errorf("code_review_rounds=%d after a human rejection, want 0", got.Counters.CodeReviewRounds)
	}
}

// --cancel is the one way a rejection ends the job. It stays a flag rather
// than a keystroke because it deletes the branch and the worktree.
func TestSpecRejectWithCancelEndsTheJob(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.HumanGates = []string{"spec"}
	j := e.submit("spec is rejected outright")
	e.runEngine()
	if s := e.jobState(j.ID).State; s != SAwaitSpec {
		t.Fatalf("setup: state=%s", s)
	}

	e.st.AddApproval(&store.Approval{
		JobID: j.ID, Gate: review.GateSpec, Decision: "reject",
		Reason: "we are not doing this at all", Cancel: true,
	})
	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SCancelled {
		t.Fatalf("after reject --cancel: state=%s hold=%q", got.State, got.HoldReason)
	}
	if p := e.promptsFor(j.ID, SImplementing); len(p) != 0 {
		t.Errorf("implementation ran %d time(s) on a cancelled job", len(p))
	}
}

// --- component A: the console announcement --------------------------------

// A job that parks at 02:00 used to produce one transition line and then
// nothing, which from the console is indistinguishable from a wedged engine.
func TestParkedJobIsAnnouncedOnTheConsole(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.HumanGates = []string{"spec"}
	j := e.submit("Fix crash when rotating during checkout")

	out := e.runEngineLogged()
	if s := e.jobState(j.ID).State; s != SAwaitSpec {
		t.Fatalf("setup: state=%s", s)
	}
	doc := review.DocPath(e.cfg.Orchestrator.DataDir, j.ID, review.GateSpec)
	want := []string{
		"WAITING FOR YOU — spec gate: ",
		// The title comes from the document, so the console and the file say
		// the same words about what is being decided.
		"Approve the spec and plan before any code is written.",
		"Fix crash when rotating during checkout",
		"waiting ",
		"review: " + doc,
		"sdlc review " + j.ID,
		"sdlc approve " + j.ID,
		"sdlc reject " + j.ID + " --reason",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("console notice is missing %q:\n%s", w, out)
		}
	}
	// Every line of a notice is attributable to one job, so a notice
	// interleaved with another job's progress still reads.
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "sdlc review ") && !strings.Contains(line, "WAITING FOR YOU") {
			continue
		}
		if !strings.HasPrefix(line, j.ID+": ") {
			t.Errorf("notice line does not start with the job id: %q", line)
		}
	}
}

// The reminder is what keeps a gate visible on a console that has scrolled.
// It re-prints the cached notice with a fresh waiting time; it must not fire
// before the interval has passed.
func TestGateIsReannouncedOnlyAfterTheInterval(t *testing.T) {
	e := newEnv(t)
	e.cfg.Orchestrator.GateReminderInterval = config.Duration(50 * time.Millisecond)
	j := e.parkAt(SAwaitMerge, "waiting on a merge decision")
	eng, buf := e.tickingEngine()
	ctx := context.Background()

	if _, _, err := eng.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := eng.tick(ctx); err != nil {
		t.Fatal(err)
	}
	first, reminders := announcementCounts(buf.String())
	if first != 1 || reminders != 0 {
		t.Fatalf("two ticks inside the interval gave %d announcement(s) and %d reminder(s), want 1 and 0:\n%s",
			first, reminders, buf.String())
	}

	backdateNotice(t, eng, j.ID, time.Hour)
	if _, _, err := eng.tick(ctx); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	first, reminders = announcementCounts(out)
	if first != 1 || reminders != 1 {
		t.Fatalf("after the interval elapsed: %d announcement(s), %d reminder(s), want 1 and 1:\n%s",
			first, reminders, out)
	}
	if !strings.Contains(out, "STILL WAITING FOR YOU (waiting ") {
		t.Errorf("the reminder does not say how long the job has been waiting:\n%s", out)
	}
	if !strings.Contains(out, "merge gate: ") {
		t.Errorf("the reminder does not name the gate:\n%s", out)
	}
}

// gate_reminder_interval: 0 is the operator saying "tell me once". Three
// ticks, however far apart, must still produce one notice.
func TestGateIsAnnouncedOnceWhenTheIntervalIsZero(t *testing.T) {
	e := newEnv(t)
	e.cfg.Orchestrator.GateReminderInterval = config.Duration(0)
	j := e.parkAt(SAwaitMerge, "announced once and never again")
	eng, buf := e.tickingEngine()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, _, err := eng.tick(ctx); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			// Age it well past any plausible interval: only the zero setting
			// may keep the console quiet from here on.
			backdateNotice(t, eng, j.ID, 24*time.Hour)
		}
	}
	first, reminders := announcementCounts(buf.String())
	if first != 1 || reminders != 0 {
		t.Fatalf("three ticks gave %d announcement(s) and %d reminder(s), want 1 and 0:\n%s",
			first, reminders, buf.String())
	}
}

// A decision the operator has already made must not come back as a question.
func TestPendingDecisionSuppressesTheNotice(t *testing.T) {
	e := newEnv(t)
	j := e.parkAt(SAwaitRelease, "release decided before the first announcement")
	// Rejecting the release completes the job, so this tick decides the gate
	// without dispatching any work — the announcement is the only thing under
	// test here.
	e.st.AddApproval(&store.Approval{
		JobID: j.ID, Gate: review.GateRelease, Decision: "reject", Reason: "ship it tomorrow",
	})
	eng, buf := e.tickingEngine()

	if _, _, err := eng.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first, _ := announcementCounts(buf.String()); first != 0 {
		t.Errorf("announced a gate whose decision was already pending:\n%s", buf.String())
	}
	if s := e.jobState(j.ID).State; s != SCompleted {
		t.Errorf("state=%s, want %s — the engine did not act on the pending decision", s, SCompleted)
	}
}

// A tick that printed nothing must not count as this interval's reminder.
// Otherwise a row the engine cannot consume yet buys ten minutes of silence,
// and the reminder the operator is relying on skips a beat for free.
func TestSuppressedNoticeDoesNotRestartTheReminderClock(t *testing.T) {
	e := newEnv(t)
	e.cfg.Orchestrator.GateReminderInterval = config.Duration(time.Hour)
	j := e.parkAt(SAwaitMerge, "a decision lands between reminders")
	eng, _ := e.tickingEngine()
	ctx := context.Background()

	eng.announceGate(ctx, j, review.GateMerge)
	aged := backdateNotice(t, eng, j.ID, 2*time.Hour)

	e.st.AddApproval(&store.Approval{JobID: j.ID, Gate: review.GateMerge, Decision: "approve"})
	eng.announceGate(ctx, j, review.GateMerge)

	last, ok := noticeLast(t, eng, j.ID)
	if !ok {
		t.Fatal("the notice was dropped by a suppressed announcement")
	}
	if !last.Equal(aged) {
		t.Errorf("last=%s, want it left at %s: a silent tick must not count as a reminder", last, aged)
	}
}

// A held job asks a different question, and the notice has to ask that one:
// approve/reject do nothing to an ESCALATED job.
func TestHeldJobIsAnnouncedWithResumeAndCancel(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_IMPL_BREAK", "1")
	t.Setenv("FAKE_ANALYSIS", "environment")
	j := e.submit("environment failure needs a human")

	out := e.runEngineLogged()
	got := e.jobState(j.ID)
	if got.State != SEscalated {
		t.Fatalf("setup: state=%s hold=%q", got.State, got.HoldReason)
	}
	if !strings.Contains(out, "WAITING FOR YOU — hold gate: ") {
		t.Errorf("the held job was not announced as a hold:\n%s", out)
	}
	for _, w := range []string{
		"sdlc review " + j.ID,
		"sdlc resume " + j.ID + " [--to STATE]",
		"sdlc cancel " + j.ID,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("hold notice is missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "sdlc approve "+j.ID) {
		t.Errorf("hold notice offers approve, which does nothing to a held job:\n%s", out)
	}
	// The reason it stopped is the one fact that lets the operator triage the
	// line without opening anything.
	if !strings.Contains(out, "environment") {
		t.Errorf("hold notice does not carry the hold reason:\n%s", out)
	}
}

// Moving from one gate to another is a new question, not a repeat of the old
// one, so it announces afresh rather than waiting out the reminder interval.
func TestANewGateAnnouncesAfreshWithinTheInterval(t *testing.T) {
	e := newEnv(t)
	e.cfg.Orchestrator.GateReminderInterval = config.Duration(time.Hour)
	j := e.parkAt(SAwaitMerge, "merge then release")
	eng, buf := e.tickingEngine()
	ctx := context.Background()

	eng.announceGate(ctx, j, review.GateMerge)
	j.State = SAwaitRelease
	if err := e.st.UpdateJob(j); err != nil {
		t.Fatal(err)
	}
	eng.announceGate(ctx, j, review.GateRelease)

	out := buf.String()
	first, reminders := announcementCounts(out)
	if first != 2 || reminders != 0 {
		t.Fatalf("%d announcement(s) and %d reminder(s) across two gates, want 2 and 0:\n%s",
			first, reminders, out)
	}
	if !strings.Contains(out, "release gate: ") {
		t.Errorf("the second gate was not named:\n%s", out)
	}
}

// The state->gate mapping lives in review.GateFor. pendingGatesFor turns it
// into the rows that would move the job on, and a hold is cleared with
// resume/cancel rather than with an approval.
func TestPendingGatesForNamesTheRowsThatWouldMoveTheJob(t *testing.T) {
	cases := []struct {
		gate string
		want []string
	}{
		{review.GateSpec, []string{"spec", "cancel"}},
		{review.GateCode, []string{"code", "cancel"}},
		{review.GateMerge, []string{"merge", "cancel"}},
		{review.GateRelease, []string{"release", "cancel"}},
		{review.GateHold, []string{"resume", "cancel"}},
	}
	for _, tc := range cases {
		t.Run(tc.gate, func(t *testing.T) {
			got := pendingGatesFor(tc.gate)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("pendingGatesFor(%q) = %v, want %v", tc.gate, got, tc.want)
			}
		})
	}
}
