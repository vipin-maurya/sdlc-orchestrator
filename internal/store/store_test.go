package store

import (
	"path/filepath"
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
