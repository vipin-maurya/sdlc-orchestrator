package jobs

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// newEnv builds the smallest config and store a submission needs. The target
// carries the values Load would have computed (branch_prefix, worktrees_dir),
// because Submit reads them and must not re-derive them itself.
func newEnv(t *testing.T) (*config.Config, *store.Store, string) {
	t.Helper()
	tmp := t.TempDir()
	wt := filepath.Join(tmp, "wt")
	cfg := config.Default()
	cfg.Targets = map[string]config.Target{
		"demo": {
			RepoPath:      filepath.Join(tmp, "repo"),
			DefaultBranch: "main",
			BranchPrefix:  "sdlc/",
			WorktreesDir:  wt,
		},
	}
	st, err := store.Open(filepath.Join(tmp, "sdlc.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return cfg, st, wt
}

func TestSubmitDerivesBranchAndWorktree(t *testing.T) {
	cfg, st, wt := newEnv(t)
	job, err := Submit(cfg, st, SubmitRequest{Target: "demo", Title: "Fix the thing", Body: "details"})
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "JOB-1" {
		t.Errorf("id = %q, want JOB-1", job.ID)
	}
	if job.Branch != "sdlc/JOB-1" {
		t.Errorf("branch = %q, want sdlc/JOB-1", job.Branch)
	}
	if want := filepath.Join(wt, "JOB-1"); job.WorktreePath != want {
		t.Errorf("worktree = %q, want %q", job.WorktreePath, want)
	}
	if job.State != "CREATED" {
		t.Errorf("state = %q, want CREATED", job.State)
	}
	// The row, not the return value, is what the engine picks up next.
	got, err := st.GetJob("JOB-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != job.Branch || got.WorktreePath != job.WorktreePath ||
		got.IssueTitle != "Fix the thing" || got.IssueBody != "details" || got.Target != "demo" {
		t.Errorf("stored job = %+v", got)
	}
}

// An unknown target must not consume an id or leave half a job behind: the
// operator retypes the target and expects the next job to be JOB-1.
func TestSubmitRejectsUnknownTarget(t *testing.T) {
	for _, target := range []string{"", "nope"} {
		cfg, st, _ := newEnv(t)
		_, err := Submit(cfg, st, SubmitRequest{Target: target, Title: "t"})
		if err == nil {
			t.Fatalf("target %q was accepted", target)
		}
		if errors.Is(err, ErrNoTitle) {
			t.Errorf("target %q reported as a missing title: %v", target, err)
		}
		// The CLI prints this line and exits 1; the message must still name the
		// target and the configured alternatives.
		if !strings.Contains(err.Error(), "unknown target") || !strings.Contains(err.Error(), "demo") {
			t.Errorf("target %q: error = %v", target, err)
		}
		jobs, err := st.ListJobs()
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 0 {
			t.Errorf("target %q created %d job(s)", target, len(jobs))
		}
	}
}

// The sentinel is the whole reason the CLI can keep answering a missing title
// with exit 2 and a bad config with exit 1. Matched with errors.Is so a future
// wrap does not collapse the two.
func TestSubmitWithoutATitleIsErrNoTitle(t *testing.T) {
	cfg, st, _ := newEnv(t)
	_, err := Submit(cfg, st, SubmitRequest{Target: "demo", Body: "a body but no title"})
	if !errors.Is(err, ErrNoTitle) {
		t.Fatalf("err = %v, want ErrNoTitle", err)
	}
	jobs, err := st.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Errorf("a titleless submission created %d job(s)", len(jobs))
	}
}

func TestSubmitDefaultsBodyToTitle(t *testing.T) {
	cfg, st, _ := newEnv(t)
	job, err := Submit(cfg, st, SubmitRequest{Target: "demo", Title: "Fix the thing"})
	if err != nil {
		t.Fatal(err)
	}
	if job.IssueBody != "Fix the thing" {
		t.Errorf("body = %q, want the title", job.IssueBody)
	}
	got, err := st.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IssueBody != "Fix the thing" {
		t.Errorf("stored body = %q, want the title", got.IssueBody)
	}
}

func TestTitleAndBodySplitsAPastedIssue(t *testing.T) {
	cases := []struct {
		name              string
		title, content    string
		wantTitle, wantBd string
	}{
		{
			name:      "markdown heading becomes the title",
			content:   "# Fix the thing\n\nIt breaks on Tuesdays.\n",
			wantTitle: "Fix the thing",
			wantBd:    "It breaks on Tuesdays.",
		},
		{
			name:      "a first line without a hash works too",
			content:   "Fix the thing\n\nIt breaks on Tuesdays.\n",
			wantTitle: "Fix the thing",
			wantBd:    "It breaks on Tuesdays.",
		},
		{
			name:      "leading blank lines are ignored",
			content:   "\n\n# Fix the thing\nbody\n",
			wantTitle: "Fix the thing",
			wantBd:    "body",
		},
		{
			// Submit defaults an empty body to the title, so keeping the raw
			// line here and defaulting there land in the same place.
			name:      "a one-line file keeps its text as the body",
			content:   "# Fix the thing\n",
			wantTitle: "Fix the thing",
			wantBd:    "# Fix the thing\n",
		},
		{
			name:      "an explicit title wins and the file is all body",
			title:     "Given on the command line",
			content:   "# Fix the thing\n\nbody\n",
			wantTitle: "Given on the command line",
			wantBd:    "# Fix the thing\n\nbody\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, body := TitleAndBody(tc.title, tc.content)
			if title != tc.wantTitle {
				t.Errorf("title = %q, want %q", title, tc.wantTitle)
			}
			if body != tc.wantBd {
				t.Errorf("body = %q, want %q", body, tc.wantBd)
			}
		})
	}
}
