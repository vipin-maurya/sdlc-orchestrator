package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestJobRoundtrip(t *testing.T) {
	s := open(t)
	id, err := s.NextJobID("JOB")
	if err != nil {
		t.Fatal(err)
	}
	if id != "JOB-1" {
		t.Errorf("first id = %s", id)
	}
	j := &Job{
		ID: id, Target: "demo", IssueTitle: "t", IssueBody: "b",
		Branch: "sdlc/JOB-1", WorktreePath: `C:\wt`, State: "CREATED",
		Counters: Counters{FixAttempts: 2, LastAnalysis: "code_bug", BaseSHA: "abc"},
	}
	if err := s.CreateJob(j); err != nil {
		t.Fatal(err)
	}
	j.State = "PLANNING"
	j.ResumeAfter = time.Now().Add(time.Hour)
	if err := s.UpdateJob(j); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "PLANNING" || got.Counters.FixAttempts != 2 ||
		got.Counters.LastAnalysis != "code_bug" || got.Counters.BaseSHA != "abc" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if got.ResumeAfter.IsZero() {
		t.Error("resume_after lost")
	}
	if id2, _ := s.NextJobID("JOB"); id2 != "JOB-2" {
		t.Errorf("second id = %s", id2)
	}
}

func TestApprovalsConsumeOnce(t *testing.T) {
	s := open(t)
	if err := s.AddApproval(&Approval{JobID: "J", Gate: "merge", Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	a, err := s.PendingApproval("J", "merge")
	if err != nil || a == nil {
		t.Fatalf("pending: %v %v", a, err)
	}
	if err := s.ConsumeApproval(a.ID); err != nil {
		t.Fatal(err)
	}
	a2, err := s.PendingApproval("J", "merge")
	if err != nil {
		t.Fatal(err)
	}
	if a2 != nil {
		t.Error("consumed approval still pending")
	}
	if p, _ := s.PendingApproval("J", "release"); p != nil {
		t.Error("wrong gate returned")
	}
}

func TestEvents(t *testing.T) {
	s := open(t)
	e := &Event{JobID: "J", State: "PLANNING", Kind: "agent_run", Agent: "opus", TokensOut: 42}
	if err := s.AddEvent(e); err != nil {
		t.Fatal(err)
	}
	evs, err := s.ListEvents("J")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Agent != "opus" || evs[0].TokensOut != 42 {
		t.Errorf("events mismatch: %+v", evs)
	}
	if n, _ := s.CountEvents("J"); n != 1 {
		t.Errorf("count = %d", n)
	}
}

// A second unconsumed row for the same job and gate is refused by the
// database, not merely by whichever caller remembered to look first.
//
// The two callers that write approvals both check before they write, and a
// check is not a guarantee: two clicks on a slow page read before either
// wrote. The row that got through would not be a harmless duplicate — every
// gate is re-enterable, so the engine consumes one now and the other on the
// next visit, as a decision nobody made against a job that has moved on.
func TestSecondPendingDecisionIsRefused(t *testing.T) {
	s := open(t)
	first := &Approval{JobID: "J", Gate: "spec", Decision: "reject", Reason: "use FooCache"}
	if err := s.AddApproval(first); err != nil {
		t.Fatal(err)
	}
	err := s.AddApproval(&Approval{JobID: "J", Gate: "spec", Decision: "reject", Reason: "use FooCache"})
	if !errors.Is(err, ErrDecisionPending) {
		t.Fatalf("second decision: err=%v, want ErrDecisionPending", err)
	}
	// The message names the job and gate, because the caller prints this line
	// and "already pending" alone does not say what is in the way.
	if !strings.Contains(err.Error(), "J") || !strings.Contains(err.Error(), "spec") {
		t.Errorf("error does not name the job and gate: %v", err)
	}
	// The first decision is untouched: the refusal must not cost the operator
	// the answer they actually gave.
	a, err := s.PendingApproval("J", "spec")
	if err != nil || a == nil {
		t.Fatalf("first decision: %v %v", a, err)
	}
	if a.ID != first.ID || a.Reason != "use FooCache" {
		t.Errorf("pending row is %+v, want the first one (%d)", a, first.ID)
	}
}

// The constraint is scoped to the rows still waiting. Once a decision is
// consumed the same job may reach the same gate again and be decided again —
// which is the ordinary path for a rejected spec, so an index that forbade it
// would break the feature it exists to protect.
func TestConsumedDecisionDoesNotBlockTheNextOne(t *testing.T) {
	s := open(t)
	if err := s.AddApproval(&Approval{JobID: "J", Gate: "spec", Decision: "reject", Reason: "first"}); err != nil {
		t.Fatal(err)
	}
	a, _ := s.PendingApproval("J", "spec")
	if err := s.ConsumeApproval(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddApproval(&Approval{JobID: "J", Gate: "spec", Decision: "approve"}); err != nil {
		t.Fatalf("second visit to the same gate refused: %v", err)
	}
	// A different gate and a different job were never in the way at all.
	if err := s.AddApproval(&Approval{JobID: "J", Gate: "merge", Decision: "approve"}); err != nil {
		t.Fatalf("different gate refused: %v", err)
	}
	if err := s.AddApproval(&Approval{JobID: "K", Gate: "spec", Decision: "approve"}); err != nil {
		t.Fatalf("different job refused: %v", err)
	}
}

// isPendingConflict matches the driver's wording, so this pins it: a driver
// upgrade that rephrases the constraint error must fail here, where the cause
// is obvious, rather than in production by quietly reporting every duplicate
// as an internal error.
func TestPendingConflictIsRecognised(t *testing.T) {
	s := open(t)
	if err := s.AddApproval(&Approval{JobID: "J", Gate: "spec", Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	_, raw := s.db.Exec(`INSERT INTO approvals(job_id,gate,decision,reason,note,cancel,consumed,created_at)
		VALUES ('J','spec','approve','','',0,0,'now')`)
	if raw == nil {
		t.Fatal("the index did not refuse a duplicate at all")
	}
	if !isPendingConflict(raw) {
		t.Errorf("isPendingConflict did not recognise the driver's wording: %v", raw)
	}
	// It must not swallow an unrelated constraint failure and report it as a
	// decision that is merely queued.
	if isPendingConflict(errors.New("UNIQUE constraint failed: approvals.id")) {
		t.Error("a primary-key collision was reported as a pending decision")
	}
}

// A database written before the index can already hold the pair the index
// forbids. Open must migrate it rather than refuse to start — a store that
// cannot be opened takes every command down with it, including the ones that
// would let an operator clear the mess.
func TestOpenMigratesADatabaseThatAlreadyHasDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dirty.db")
	s, err := Open(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Dropped so the rows below can be written the way a build without the
	// index wrote them. Recreating that state is the only way to test the
	// migration; with the index in force the duplicates cannot exist to be
	// migrated, and the test would pass without exercising anything.
	if _, err := s.db.Exec(`DROP INDEX idx_approvals_pending`); err != nil {
		t.Fatal(err)
	}
	// Written behind AddApproval's back, which is how they got there before
	// the index existed.
	for _, reason := range []string{"first", "second", "third"} {
		if _, err := s.db.Exec(`INSERT INTO approvals(job_id,gate,decision,reason,note,cancel,consumed,created_at)
			VALUES ('J','spec','reject',?,'',0,0,'now')`, reason); err != nil {
			t.Fatal(err)
		}
	}
	// A row on another gate must survive untouched: the dedupe is per job and
	// gate, and consuming a decision nobody duplicated would lose it.
	if _, err := s.db.Exec(`INSERT INTO approvals(job_id,gate,decision,reason,note,cancel,consumed,created_at)
		VALUES ('J','merge','approve','','',0,0,'now')`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path, 5*time.Second)
	if err != nil {
		t.Fatalf("Open refused a database that already had duplicates: %v", err)
	}
	defer s2.Close()
	// The oldest survives — it is the decision the operator gave first, and
	// the one the engine would have acted on.
	a, err := s2.PendingApproval("J", "spec")
	if err != nil || a == nil {
		t.Fatalf("pending after migration: %v %v", a, err)
	}
	if a.Reason != "first" {
		t.Errorf("survivor is %q, want the oldest (%q)", a.Reason, "first")
	}
	if m, _ := s2.PendingApproval("J", "merge"); m == nil {
		t.Error("the merge decision was consumed by a dedupe scoped to the spec gate")
	}
	// And the index is now in force on the migrated database.
	if err := s2.AddApproval(&Approval{JobID: "J", Gate: "spec", Decision: "approve"}); !errors.Is(err, ErrDecisionPending) {
		t.Errorf("after migration a duplicate was accepted: %v", err)
	}
}

// The job page wants the newest progress line and the total, and asks for
// exactly those two things rather than reading the history to derive them.
func TestLastEventOfKind(t *testing.T) {
	s := open(t)
	for _, e := range []*Event{
		{JobID: "J", Kind: "progress", Detail: `{"n":1}`},
		{JobID: "J", Kind: "agent_run", Detail: "not this one"},
		{JobID: "J", Kind: "progress", Detail: `{"n":2}`},
		{JobID: "K", Kind: "progress", Detail: "another job's"},
	} {
		if err := s.AddEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LastEventOfKind("J", "progress")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Detail != `{"n":2}` {
		t.Errorf("last progress = %+v, want the newest one for this job", got)
	}
	// A job with no event of that kind is not an error: the page draws no
	// progress line and everything else on it still renders.
	none, err := s.LastEventOfKind("J", "no_such_kind")
	if err != nil || none != nil {
		t.Errorf("missing kind returned (%v, %v), want (nil, nil)", none, err)
	}
}
