package prompt

import (
	"strings"
	"testing"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
)

func TestScopingPromptRenders(t *testing.T) {
	got, hash, err := Render(config.StScoping, "", "", Ctx{
		JobID: "JOB-9", Branch: "sdlc/JOB-9",
		IssueTitle: "Fix 1.0.6 issues", IssueBody: "several things are broken",
		Round: 1, MaxRounds: 2,
	})
	if err != nil {
		t.Fatalf("no default prompt for SCOPING: %v", err)
	}
	if hash == "" {
		t.Error("prompt hash is empty")
	}
	// runFakeAgent in internal/engine dispatches on this phrase, and so does a
	// human reading the prompt log. Changing it breaks the engine tests.
	if !strings.Contains(got, "scoping agent") {
		t.Error(`rendered prompt does not contain "scoping agent"`)
	}
	for _, want := range []string{"JOB-9", "Fix 1.0.6 issues", "problem/1", ".sdlc/problem.json", "clarity"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered prompt is missing %q", want)
		}
	}
	// With no rejection there must be no re-scope section.
	if strings.Contains(got, "A human sent this back") {
		t.Error("re-scope section rendered on a first attempt")
	}
}

func TestScopingPromptCarriesTheRejection(t *testing.T) {
	got, _, err := Render(config.StScoping, "", "", Ctx{
		JobID: "JOB-9", Round: 2, MaxRounds: 2,
		RejectReason: "Q1: only the SMS parser. Q2: keep the old format readable.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Q1: only the SMS parser") {
		t.Error("the operator's answers are not in the re-scope prompt")
	}
	if !strings.Contains(got, ".sdlc/context/problem.json") {
		t.Error("the re-scope prompt does not point at the previous problem statement")
	}
}

func TestPlanningPromptMentionsProblemStatement(t *testing.T) {
	// When scoping is enabled and problem.json is present, it is inlined.
	got, _, err := Render(config.StPlanning, "", "", Ctx{
		JobID: "JOB-9", Branch: "sdlc/JOB-9",
		IssueTitle: "Fix things", IssueBody: "body",
		ScopedProblem: `{"schema": "problem/1", "problem_statement": "broken"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, ".sdlc/context/problem.json") || !strings.Contains(got, `{"schema": "problem/1"`) {
		t.Error("planning prompt does not reference the scoped problem statement or inline it")
	}

	// When scoping is off, no ScopedProblem block is rendered.
	gotNoScope, _, err := Render(config.StPlanning, "", "", Ctx{
		JobID: "JOB-9", Branch: "sdlc/JOB-9",
		IssueTitle: "Fix things", IssueBody: "body",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotNoScope, "# Scoped problem") {
		t.Error("planning prompt rendered scoped problem block when ScopedProblem is empty")
	}
}

func TestDesignReviewPromptMentionsProblemStatement(t *testing.T) {
	got, _, err := Render(config.StDesignReview, "", "", Ctx{
		JobID: "JOB-9", Branch: "sdlc/JOB-9",
		IssueTitle: "Fix things", IssueBody: "body",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, ".sdlc/context/problem.json") {
		t.Error("design review prompt does not reference the scoped problem statement")
	}
}
