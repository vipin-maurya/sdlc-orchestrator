// Package jobs owns job creation. It exists so `sdlc submit` and the web UI
// cannot disagree about which targets exist, what makes a title valid, how a
// branch name is derived or what an empty body defaults to. It depends on
// config and store only, so both callers can import it and the engine's state
// machine stays untouched. If this looks small enough to inline back into cli,
// that is the inlining this package exists to prevent.
package jobs

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

type SubmitRequest struct{ Target, Title, Body string }

// ErrNoTitle distinguishes "you did not say what this is" from a configuration
// error, because the CLI answers the first with a usage exit (2) and the second
// with a failure exit (1). Callers test with errors.Is.
var ErrNoTitle = errors.New("a title is required")

// Submit validates the request against cfg, allocates the id, derives the
// branch and worktree paths, and creates the job row.
func Submit(cfg *config.Config, st *store.Store, req SubmitRequest) (*store.Job, error) {
	// An empty target arrives here rather than being checked separately: it is
	// an unknown target, and cfg.Target already answers with the list of the
	// ones that do exist. The error is returned undecorated because the CLI
	// prints it verbatim and a prefix added here would change a line the CLI's
	// own test holds to the byte.
	t, err := cfg.Target(req.Target)
	if err != nil {
		return nil, err
	}
	if req.Title == "" {
		return nil, ErrNoTitle
	}
	body := req.Body
	if body == "" {
		// Every agent prompt interpolates the body, so an empty one hands the
		// planner a blank issue. The title is the one thing the submitter did
		// say, and it is a better brief than nothing.
		body = req.Title
	}
	// The id is allocated only after the request is known to be good: a
	// rejected submission that still burned JOB-7 leaves a gap in the sequence
	// that reads like a job somebody deleted.
	id, err := st.NextJobID(cfg.Orchestrator.JobIDPrefix)
	if err != nil {
		return nil, err
	}
	job := &store.Job{
		ID:           id,
		Target:       req.Target,
		IssueTitle:   req.Title,
		IssueBody:    body,
		Branch:       t.BranchPrefix + id,
		WorktreePath: filepath.Join(t.WorktreesDir, id),
		State:        "CREATED",
	}
	if err := st.CreateJob(job); err != nil {
		return nil, err
	}
	return job, nil
}

// TitleAndBody splits a pasted issue file the way `sdlc submit --file` does:
// when title is empty the first line, stripped of a leading '#', becomes the
// title and the remainder becomes the body.
func TitleAndBody(title, content string) (title2, body string) {
	if title != "" {
		return title, content
	}
	lines := strings.SplitN(strings.TrimSpace(content), "\n", 2)
	title2 = strings.TrimSpace(strings.TrimPrefix(lines[0], "#"))
	// A single-line file keeps its whole text as the body rather than ending up
	// with an empty one. Submit would default the body to the title anyway, so
	// the two agree; this way the raw line survives whichever path ran.
	body = content
	if len(lines) > 1 {
		body = strings.TrimSpace(lines[1])
	}
	return title2, body
}
