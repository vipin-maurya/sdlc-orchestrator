package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
