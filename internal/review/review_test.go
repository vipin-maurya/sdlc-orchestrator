package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// latestReview decides which round of a review the operator is shown. Getting
// it wrong is silent: the document renders fine, it just quotes a decision
// that was superseded two rounds ago.
func TestLatestReviewPicksTheHighestRoundAndPrefersTheVerifiedCopy(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name  string
		files []string
		want  string
	}{
		{"no reviews yet", nil, ""},
		{"single round", []string{"design_review.r1.json"}, "design_review.r1.json"},
		{
			// Round 10 must beat round 9: a lexical sort would not.
			"two-digit round beats single digit",
			[]string{"design_review.r9.json", "design_review.r10.json"},
			"design_review.r10.json",
		},
		{
			// The verified copy carries the refutations. Showing the raw review
			// instead would present a finding the verifiers already dismissed.
			"verified copy wins its round",
			[]string{"code_review.r2.json", "code_review.r2.verified.json"},
			"code_review.r2.verified.json",
		},
		{
			"latest round wins even when an earlier one was verified",
			[]string{"code_review.r1.verified.json", "code_review.r2.json"},
			"code_review.r2.json",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir = t.TempDir()
			for _, f := range tc.files {
				write(f)
			}
			prefix := "design_review"
			if len(tc.files) > 0 && filepath.Base(tc.files[0])[:4] == "code" {
				prefix = "code_review"
			}
			if got := latestReview(dir, prefix); got != tc.want {
				t.Errorf("latestReview(%q) = %q, want %q", prefix, got, tc.want)
			}
		})
	}
}

// A review family with no rounds at all (final_review.json) goes through
// preferVerified instead, which must not invent a file that is not there.
func TestPreferVerifiedReportsNothingWhenNeitherFileExists(t *testing.T) {
	dir := t.TempDir()
	if got := preferVerified(dir, "final_review.json"); got != "" {
		t.Errorf("preferVerified on an empty dir = %q, want \"\"", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "final_review.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := preferVerified(dir, "final_review.json"); got != "final_review.json" {
		t.Errorf("preferVerified = %q, want the raw review", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "final_review.verified.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := preferVerified(dir, "final_review.json"); got != "final_review.verified.json" {
		t.Errorf("preferVerified = %q, want the verified copy", got)
	}
}

// A path is only worth printing once the file behind it exists. Render does not
// write the patch, so only Write may name it — otherwise `sdlc review` sends the
// reader to open a file that is not there.
func TestOnlyTheWrittenDocumentNamesThePatchFile(t *testing.T) {
	dir := t.TempDir()
	job := &store.Job{
		ID: "JOB-9", Target: "app", IssueTitle: "t",
		Branch: "sdlc/JOB-9", WorktreePath: filepath.Join(dir, "gone"),
		State: "AWAITING_MERGE_APPROVAL",
	}
	// No worktree and no base sha: renderDiff bails early, so neither surface
	// has a patch to name. What matters is that Render never names one.
	doc, err := Render(context.Background(), Options{Job: job, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc.Body, DiffPath(dir, job.ID, GateMerge)) {
		t.Error("Render named the patch file; it never writes one")
	}
	if _, _, err := Write(context.Background(), Options{Job: job, DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(DocPath(dir, job.ID, GateMerge))
	if err != nil {
		t.Fatalf("Write did not leave a document: %v", err)
	}
	// With no diff there is still no patch file, so the written document must
	// not name one either.
	if strings.Contains(string(body), DiffPath(dir, job.ID, GateMerge)) {
		t.Error("written document named a patch file that was never written")
	}
}

func TestScopeGateTitleAndActions(t *testing.T) {
	if GateFor("AWAITING_SCOPE_APPROVAL") != GateScope {
		t.Fatal("scope state is not mapped to the scope gate")
	}
	// A gate with no title falls through to the generic "Decision needed.",
	// which tells the operator nothing about what they are deciding.
	got := gateTitle(GateScope, Options{})
	if got == "Decision needed." || got == "" {
		t.Errorf("scope gate has no title of its own: %q", got)
	}
	var bs []Block
	renderActions(&bs, GateScope, Options{Job: &store.Job{ID: "JOB-1"}})
	var actions []Action
	for _, b := range bs {
		if b.Kind == BlockActions {
			actions = b.Actions
		}
	}
	if len(actions) != 2 {
		t.Fatalf("scope gate has %d actions, want 2 (approve, reject)", len(actions))
	}
	for _, a := range actions {
		if len(a.Effect) == 0 {
			t.Errorf("action %q has no effect text", a.Decision)
		}
	}
}

func TestScopeGateDocument(t *testing.T) {
	dir := t.TempDir()
	j := &store.Job{
		ID: "JOB-3", IssueTitle: "fix the thing", IssueBody: "several things are broken",
		State: "AWAITING_SCOPE_APPROVAL", Branch: "sdlc/JOB-3", WorktreePath: filepath.Join(dir, "wt"),
		StateEnteredAt: time.Now().Add(-4 * time.Minute),
	}
	art := artifact.ArtifactsDir(dir, j.ID)
	if err := os.MkdirAll(art, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":"problem/1",
		"problem_statement":"SmsExpenseParser drops amounts written with a non-breaking space",
		"in_scope":["the SMS amount parser"],"out_of_scope":["the notification parser"],
		"success_criteria":["U+00A0 before the amount parses like a plain space"],
		"assumptions":[{"assumption":"only the SMS path is affected","basis":"NotificationParser has its own regex"}],
		"open_questions":[{"id":"Q1","question":"should the old format stay readable?","why_it_matters":"decides whether a migration is needed","blocking":true}],
		"clarity":"blocked"}`
	if err := os.WriteFile(filepath.Join(art, "problem.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := Render(context.Background(), Options{Job: j, DataDir: dir})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if d.Gate != GateScope {
		t.Errorf("gate=%q, want %q", d.Gate, GateScope)
	}
	for _, want := range []string{
		"SmsExpenseParser drops amounts",       // the statement
		"the notification parser",              // out of scope
		"U+00A0 before the amount",             // success criteria
		"NotificationParser has its own regex", // the assumption's basis
		"should the old format stay readable?", // the blocking question
		"decides whether a migration is needed",
	} {
		if !strings.Contains(d.Body, want) {
			t.Errorf("document is missing %q\n---\n%s", want, d.Body)
		}
	}
	// The operator must be told why the job is parked; the two causes need
	// different responses.
	if !strings.Contains(d.Body, "blocked") {
		t.Error("the document does not say the agent is blocked")
	}
}

// The policy-gate case has no blocking question and must not imply one.
func TestScopeGateDocumentPolicyGateWording(t *testing.T) {
	dir := t.TempDir()
	j := &store.Job{ID: "JOB-4", IssueTitle: "clear ticket", State: "AWAITING_SCOPE_APPROVAL",
		StateEnteredAt: time.Now()}
	art := artifact.ArtifactsDir(dir, j.ID)
	if err := os.MkdirAll(art, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":"problem/1","problem_statement":"a clear problem",
		"in_scope":["x"],"out_of_scope":["y"],"success_criteria":["z"],
		"assumptions":[],"open_questions":[],"clarity":"clear"}`
	if err := os.WriteFile(filepath.Join(art, "problem.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := Render(context.Background(), Options{Job: j, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Body, "Open questions") {
		t.Error("an open-questions section was rendered with no questions")
	}
	if !strings.Contains(d.Body, "human_gates") {
		t.Error("the document does not say why a clear problem is parked")
	}
}
