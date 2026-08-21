// Package jobs owns job creation. It exists so `sdlc submit` and the web UI
// cannot disagree about which targets exist, what makes a title valid, how a
// branch name is derived or what an empty body defaults to. It depends on
// config and store only, so both callers can import it and the engine's state
// machine stays untouched. If this looks small enough to inline back into cli,
// that is the inlining this package exists to prevent.
package jobs

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

type SubmitRequest struct{ Target, Title, Body string }

// MaxTitleBytes and MaxBodyBytes bound what one submission may store. Submit is
// the shared entry point for the forthcoming HTTP server, which has no
// authentication of its own, so anything reachable through it is an anonymous
// write: without a cap here a single request can put an arbitrary number of
// bytes into the database and into every agent prompt that interpolates them.
// The limits are generous for the thing they describe — a one-line title and an
// issue body a person typed — and are counted in bytes, not runes, because it
// is bytes that the store and the prompts have to carry.
const (
	MaxTitleBytes = 500
	MaxBodyBytes  = 1 << 20 // 1 MiB
)

// ErrNoTitle distinguishes "you did not say what this is" from a configuration
// error, because the CLI answers the first with a usage exit (2) and the second
// with a failure exit (1). Callers test with errors.Is.
var ErrNoTitle = errors.New("a title is required")

// The remaining sentinels exist so an HTTP layer can map a rejection to a
// status (413 for the two size limits, 400 for the rest) without matching on
// message text, and so the CLI can exit non-zero on the same distinction. Each
// message states the limit it enforces, because the caller sees only this line.
var (
	ErrTitleTooLong = fmt.Errorf("a title may be at most %d bytes", MaxTitleBytes)
	ErrBodyTooLong  = fmt.Errorf("a body may be at most %d bytes (%d MiB)", MaxBodyBytes, MaxBodyBytes>>20)
	// A title is one line by definition and is printed raw into the approver's
	// terminal, the engine log and a commit message, so a control character in
	// it is either an escape sequence dressing up the text somebody is being
	// asked to approve, or a newline that turns one title into two lines.
	ErrTitleControlChars = errors.New("a title may not contain control characters; it is a single line of text")
	// A body may legitimately be many lines, so only NUL is refused: it
	// truncates the text for anything that hands it to a C string, which means
	// a body can end where the reader does not expect it to.
	ErrBodyControlChars = errors.New("a body may not contain a NUL byte")
)

// hasControlChars reports whether s contains a C0 or C1 control character. DEL
// is included: it is not printable either, and nothing that belongs in a title
// contains one.
func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return true
		}
	}
	return false
}

// Submit validates the request against cfg, allocates the id, derives the
// branch and worktree paths, and creates the job row.
func Submit(cfg *config.Config, st *store.Store, req SubmitRequest) (*store.Job, error) {
	// An empty target arrives here rather than being checked separately: it is
	// an unknown target, and cfg.Target already answers with the list of the
	// ones that do exist. The error is returned undecorated because the CLI
	// prints this line verbatim, so any prefix added here would show up in
	// front of a message that already reads as a complete sentence.
	t, err := cfg.Target(req.Target)
	if err != nil {
		return nil, err
	}
	// Trim before the check and store the trimmed value, so the two agree: a
	// title of "   " is as absent as "" is, and one of " Fix it " must not
	// reach a prompt, a branch description or a commit message with the
	// whitespace still on it.
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return nil, ErrNoTitle
	}
	if len(title) > MaxTitleBytes {
		return nil, ErrTitleTooLong
	}
	if hasControlChars(title) {
		return nil, ErrTitleControlChars
	}
	// The body is measured as it arrived, before the fallback below can
	// replace it: an oversized body is refused whatever it happens to contain.
	if len(req.Body) > MaxBodyBytes {
		return nil, ErrBodyTooLong
	}
	if strings.ContainsRune(req.Body, 0) {
		return nil, ErrBodyControlChars
	}
	body := req.Body
	if strings.TrimSpace(body) == "" {
		// Every agent prompt interpolates the body, so an empty one hands the
		// planner a blank issue — and a body of only whitespace hands it the
		// same blank issue while looking like content. The title is the one
		// thing the submitter did say, and it is a better brief than nothing.
		body = title
	}
	// NextJobID commits its own transaction, so the id is spent the moment it
	// is handed out: if CreateJob below fails, that id is skipped for good and
	// the sequence reads like a job somebody deleted. Validating first keeps
	// that rare rather than impossible — making it impossible means allocating
	// the id inside the same transaction that inserts the row, which is a
	// change to store, not one this function can make.
	id, err := st.NextJobID(cfg.Orchestrator.JobIDPrefix)
	if err != nil {
		return nil, err
	}
	job := &store.Job{
		ID:           id,
		Target:       req.Target,
		IssueTitle:   title,
		IssueBody:    body,
		Branch:       t.BranchPrefix + id,
		WorktreePath: filepath.Join(t.WorktreesDir, id),
		State:        "CREATED",
	}
	if err := st.CreateJob(job); err != nil {
		// Wrapped because the driver's own text ("constraint failed: UNIQUE
		// constraint failed: jobs.id (1555)") is not an API surface: it names
		// the schema, and the id is the part of it a caller can act on.
		return nil, fmt.Errorf("create job %s: %w", id, err)
	}
	return job, nil
}

// TitleAndBody splits a pasted issue file the way `sdlc submit --file` does:
// when title is empty the first line, stripped of its leading '#' characters,
// becomes the title and the remainder becomes the body.
func TitleAndBody(title, content string) (title2, body string) {
	if title != "" {
		return title, content
	}
	lines := strings.SplitN(strings.TrimSpace(content), "\n", 2)
	// TrimLeft, not TrimPrefix: a deeper heading ("### Fix it") is still a
	// heading, and leaving the remaining hashes on would title the job "## Fix
	// it".
	title2 = strings.TrimSpace(strings.TrimLeft(lines[0], "#"))
	// A single-line file keeps its whole text as the body rather than ending up
	// with an empty one. Submit would default the body to the title anyway, so
	// the two agree; this way the raw line survives whichever path ran.
	body = content
	if len(lines) > 1 {
		body = strings.TrimSpace(lines[1])
	}
	return title2, body
}
