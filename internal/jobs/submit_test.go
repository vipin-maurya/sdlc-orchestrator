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
			wantBd:    "# Fix the thing",
		},
		{
			// Trimmed on both paths. It used to keep the file's trailing
			// newline only when the file had a single line, which is a
			// difference between two issue files nobody wrote on purpose.
			name:      "a one-line body is trimmed like a multi-line one",
			content:   "  # Fix the thing  \n\n",
			wantTitle: "Fix the thing",
			wantBd:    "# Fix the thing",
		},
		{
			// TrimPrefix stripped one '#' and titled the job "## Deep heading".
			name:      "a deeper heading loses all of its hashes",
			content:   "### Deep heading\n\nbody\n",
			wantTitle: "Deep heading",
			wantBd:    "body",
		},
		{
			name:      "a two-hash heading",
			content:   "## Two\n\nbody\n",
			wantTitle: "Two",
			wantBd:    "body",
		},
		{
			name:      "a one-hash heading",
			content:   "# One\n\nbody\n",
			wantTitle: "One",
			wantBd:    "body",
		},
		{
			// No space after the hash is still a heading to a reader, and the
			// hash is still not part of the title.
			name:      "no space after the hash",
			content:   "#NoSpace\n\nbody\n",
			wantTitle: "NoSpace",
			wantBd:    "body",
		},
		{
			// A hash inside the line is text, not a marker: only the leading run
			// is stripped.
			name:      "a hash in the middle of the title survives",
			content:   "## Fix #42 in the parser\n\nbody\n",
			wantTitle: "Fix #42 in the parser",
			wantBd:    "body",
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

// A title of only whitespace is as absent as an empty one: it names nothing,
// and the planner brief built from it would be blank. It must fail the same way
// so the CLI keeps answering it with the usage exit rather than creating a job.
func TestSubmitRejectsAWhitespaceTitle(t *testing.T) {
	for _, title := range []string{" ", "   ", "\t", "\n", " \t\r\n "} {
		cfg, st, _ := newEnv(t)
		_, err := Submit(cfg, st, SubmitRequest{Target: "demo", Title: title, Body: "a body"})
		if !errors.Is(err, ErrNoTitle) {
			t.Errorf("title %q: err = %v, want ErrNoTitle", title, err)
		}
		jobs, err := st.ListJobs()
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 0 {
			t.Errorf("title %q created %d job(s)", title, len(jobs))
		}
	}
}

// What is stored is what every prompt, branch description and commit message
// interpolates, so the padding has to be gone by the time it is written rather
// than trimmed again by each reader.
func TestSubmitStoresATrimmedTitle(t *testing.T) {
	cfg, st, _ := newEnv(t)
	job, err := Submit(cfg, st, SubmitRequest{Target: "demo", Title: "  Fix the thing \t", Body: "details"})
	if err != nil {
		t.Fatal(err)
	}
	if job.IssueTitle != "Fix the thing" {
		t.Errorf("title = %q, want %q", job.IssueTitle, "Fix the thing")
	}
	got, err := st.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IssueTitle != "Fix the thing" {
		t.Errorf("stored title = %q, want %q", got.IssueTitle, "Fix the thing")
	}
}

// A body of only whitespace hands the planner the same blank issue an empty one
// does, so it takes the same fallback.
func TestSubmitDefaultsAWhitespaceBodyToTitle(t *testing.T) {
	for _, body := range []string{"", " ", "\n\n", "\t \n"} {
		cfg, st, _ := newEnv(t)
		job, err := Submit(cfg, st, SubmitRequest{Target: "demo", Title: "Fix the thing", Body: body})
		if err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
		if job.IssueBody != "Fix the thing" {
			t.Errorf("body %q: stored body = %q, want the title", body, job.IssueBody)
		}
	}
}

// Submit is the entry point the coming HTTP server shares, and that server has
// no authentication, so an unbounded title or body is an anonymous write of
// arbitrary size. The sentinels are what lets that server answer 413 without
// matching on the message.
func TestSubmitCapsTitleAndBodySize(t *testing.T) {
	cfg, st, _ := newEnv(t)
	if _, err := Submit(cfg, st, SubmitRequest{Target: "demo", Title: strings.Repeat("a", MaxTitleBytes+1)}); !errors.Is(err, ErrTitleTooLong) {
		t.Errorf("an oversized title: err = %v, want ErrTitleTooLong", err)
	}
	if _, err := Submit(cfg, st, SubmitRequest{Target: "demo", Title: "t", Body: strings.Repeat("b", MaxBodyBytes+1)}); !errors.Is(err, ErrBodyTooLong) {
		t.Errorf("an oversized body: err = %v, want ErrBodyTooLong", err)
	}
	// The limit belongs in the message: the caller sees only this line.
	for _, tc := range []struct {
		err  error
		want string
	}{{ErrTitleTooLong, "500"}, {ErrBodyTooLong, "1048576"}} {
		if !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("message %q does not state the limit %s", tc.err, tc.want)
		}
	}
	// Exactly at the limit is still a submission, so the cap cannot creep.
	if _, err := Submit(cfg, st, SubmitRequest{Target: "demo", Title: strings.Repeat("a", MaxTitleBytes), Body: strings.Repeat("b", MaxBodyBytes)}); err != nil {
		t.Errorf("a submission at exactly the limits was refused: %v", err)
	}
	jobs, err := st.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Errorf("rejected submissions created rows: %d job(s), want 1", len(jobs))
	}
}

// A title is one line and is printed raw into the approver's terminal, the
// engine log and a commit message. An escape sequence there dresses up the text
// somebody is being asked to approve; a newline turns one title into two lines.
func TestSubmitRejectsControlCharactersInTitle(t *testing.T) {
	cases := map[string]string{
		"ANSI colour": "\x1b[31mURGENT\x1b[0m ship it",
		"NUL":         "Fix the\x00 thing",
		"embedded LF": "Fix the thing\nApproved by: nobody",
		"embedded CR": "Fix the thing\rApproved",
		"tab":         "Fix\tthe thing",
		"backspace":   "Fix the thing\b\b\b",
	}
	// DEL and the C1 range are built from their code points rather than written
	// out: pasted into a source file they are invisible.
	for name, r := range map[string]rune{"DEL": 0x7f, "C1 NEL": 0x85, "C1 CSI": 0x9b} {
		cases[name] = "Fix the thing" + string(r) + "and more"
	}
	for name, title := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, st, _ := newEnv(t)
			_, err := Submit(cfg, st, SubmitRequest{Target: "demo", Title: title, Body: "body"})
			if !errors.Is(err, ErrTitleControlChars) {
				t.Errorf("title %q: err = %v, want ErrTitleControlChars", title, err)
			}
			jobs, err := st.ListJobs()
			if err != nil {
				t.Fatal(err)
			}
			if len(jobs) != 0 {
				t.Errorf("title %q created %d job(s)", title, len(jobs))
			}
		})
	}
	cfg, st, _ := newEnv(t)
	// A body is many lines by definition, so only NUL - which truncates the
	// text for anything that treats it as a C string - is refused there.
	if _, err := Submit(cfg, st, SubmitRequest{Target: "demo", Title: "Fix the thing", Body: "line one\nline two\ttabbed"}); err != nil {
		t.Errorf("a multi-line body was refused: %v", err)
	}
	if _, err := Submit(cfg, st, SubmitRequest{Target: "demo", Title: "Fix the thing", Body: "line one\x00line two"}); !errors.Is(err, ErrBodyControlChars) {
		t.Errorf("a NUL in the body: err = %v, want ErrBodyControlChars", err)
	}
	// Text that is merely not ASCII is not a control character and still goes
	// through: the rule is about what a terminal executes, not about scripts.
	if _, err := Submit(cfg, st, SubmitRequest{Target: "demo", Title: "Corrige l'affichage — 日本語", Body: "b"}); err != nil {
		t.Errorf("a unicode title was refused: %v", err)
	}
}

// A failed insert must not hand the caller the driver's own text ("constraint
// failed: UNIQUE constraint failed: jobs.id (1555)"): it names the schema, and
// the id is the part of it a caller can act on. The same test pins the id
// behaviour the comment in Submit now describes - the failed id is spent.
func TestSubmitWrapsACreateFailureAndSpendsTheID(t *testing.T) {
	cfg, st, _ := newEnv(t)
	// Plant the id the sequence is about to hand out, so CreateJob collides.
	planted := &store.Job{ID: "JOB-1", Target: "demo", IssueTitle: "planted", IssueBody: "planted", Branch: "sdlc/JOB-1", State: "CREATED"}
	if err := st.CreateJob(planted); err != nil {
		t.Fatal(err)
	}
	_, err := Submit(cfg, st, SubmitRequest{Target: "demo", Title: "collides"})
	if err == nil {
		t.Fatal("a colliding submission was accepted")
	}
	if !strings.Contains(err.Error(), "create job JOB-1") {
		t.Errorf("err = %v, want it to name the job it failed to create", err)
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("err = %v, want the cause to survive the wrap", err)
	}
	// JOB-1 is gone for good: NextJobID committed before CreateJob ran. This is
	// the behaviour Submit's comment now states, rather than the one it used to
	// claim.
	job, err := Submit(cfg, st, SubmitRequest{Target: "demo", Title: "next"})
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "JOB-2" {
		t.Errorf("id after a failed create = %q, want JOB-2", job.ID)
	}
}
