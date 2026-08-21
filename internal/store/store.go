// Package store persists jobs, events, and approvals in SQLite
// (modernc.org/sqlite, pure Go). WAL mode; writes serialized through a
// single connection (MaxOpenConns=1), which is the single-writer discipline
// SPEC §4 requires.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// Counters are the independent per-loop budgets (SPEC §3.3).
type Counters struct {
	DesignReviewRounds int `json:"design_review_rounds"`
	CodeReviewRounds   int `json:"code_review_rounds"`
	FixAttempts        int `json:"fix_attempts"`
	FlakeRetries       int `json:"flake_retries"`
	AgentRetries       int `json:"agent_retries"` // reset on state change
	ReleaseRetries     int `json:"release_retries"`
	AgentInvocations   int `json:"agent_invocations"`
	// LastAnalysis records the classification driving the current fix loop
	// so the test-file policy knows whether it applies.
	LastAnalysis string `json:"last_analysis,omitempty"`
	// FixSource records which state sent the job into FIXING so the loop
	// returns to the right place semantics-wise (always via BUILDING).
	FixSource string `json:"fix_source,omitempty"`
	// AnalysisRound / FixRound / skipped-UI markers for artifact naming.
	AnalysisRound int  `json:"analysis_round,omitempty"`
	FixRound      int  `json:"fix_round,omitempty"`
	UISkipped     bool `json:"ui_skipped,omitempty"`
	// FailedPhase remembers which phase failed (build|unit|ui|lint) for
	// FLAKE_CHECK and ANALYZING.
	FailedPhase string `json:"failed_phase,omitempty"`
	// BaseSHA is the worktree HEAD at job creation — the base every review
	// diffs against.
	BaseSHA string `json:"base_sha,omitempty"`
	// LastFailureLog is the log file of the most recent failed exec phase.
	LastFailureLog string `json:"last_failure_log,omitempty"`
	// HumanRejectReason carries the reason from a merge-gate rejection into
	// the next FIXING prompt.
	HumanRejectReason string `json:"human_reject_reason,omitempty"`
	// LastDesignReview names the design-review artifact whose findings passed
	// the gate but still need forwarding to IMPLEMENTING. Empty when the
	// review was clean. DesignReviewRounds cannot serve this purpose: it only
	// increments on rejection, so it is 0 for a review that passed.
	LastDesignReview string `json:"last_design_review,omitempty"`
}

type Job struct {
	ID             string
	Target         string
	IssueTitle     string
	IssueBody      string
	Branch         string
	WorktreePath   string
	State          string
	PrevState      string
	StateEnteredAt time.Time
	HeadSHA        string
	ResumeAfter    time.Time // zero when unset
	HoldReason     string
	Counters       Counters
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Event struct {
	ID            int64
	JobID         string
	State         string
	Kind          string // enter|agent_run|exec_run|transition|approval|error|note
	Agent         string
	Backend       string
	Model         string
	Effort        string
	BinaryVersion string
	PromptHash    string
	InputRef      string
	OutputRef     string
	ExitCode      sql.NullInt64
	DurationMS    int64
	HeadBefore    string
	HeadAfter     string
	TokensIn      int64
	TokensOut     int64
	Detail        string
	CreatedAt     time.Time
}

type Approval struct {
	ID       int64
	JobID    string
	Gate     string // merge | release | resume | cancel
	Decision string // approve | reject | resume | cancel
	// Reason is gate-specific: the rejection reason for a merge/release gate,
	// the target state for a resume.
	Reason string
	// Note is free text the operator addresses to the *agent* that runs next,
	// as opposed to Reason which the orchestrator interprets. It is the only
	// channel by which a human can answer an escalation ("yes, those fixtures
	// are stale — update them") without hand-editing the repository.
	Note      string
	Cancel    bool
	Consumed  bool
	CreatedAt time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY,
  target TEXT NOT NULL,
  issue_title TEXT NOT NULL,
  issue_body TEXT NOT NULL,
  branch TEXT NOT NULL,
  worktree_path TEXT NOT NULL,
  state TEXT NOT NULL,
  prev_state TEXT NOT NULL DEFAULT '',
  state_entered_at TEXT NOT NULL,
  head_sha TEXT NOT NULL DEFAULT '',
  resume_after TEXT NOT NULL DEFAULT '',
  hold_reason TEXT NOT NULL DEFAULT '',
  counters TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id TEXT NOT NULL,
  state TEXT NOT NULL,
  kind TEXT NOT NULL,
  agent TEXT NOT NULL DEFAULT '',
  backend TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  effort TEXT NOT NULL DEFAULT '',
  binary_version TEXT NOT NULL DEFAULT '',
  prompt_hash TEXT NOT NULL DEFAULT '',
  input_ref TEXT NOT NULL DEFAULT '',
  output_ref TEXT NOT NULL DEFAULT '',
  exit_code INTEGER,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  head_before TEXT NOT NULL DEFAULT '',
  head_after TEXT NOT NULL DEFAULT '',
  tokens_in INTEGER NOT NULL DEFAULT 0,
  tokens_out INTEGER NOT NULL DEFAULT 0,
  detail TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_job ON events(job_id, id);
CREATE TABLE IF NOT EXISTS approvals (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id TEXT NOT NULL,
  gate TEXT NOT NULL,
  decision TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  cancel INTEGER NOT NULL DEFAULT 0,
  consumed INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`

const timeFmt = time.RFC3339Nano

func Open(path string, busyTimeout time.Duration) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)&_pragma=foreign_keys(ON)",
		filepath.ToSlash(path), busyTimeout.Milliseconds())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single-writer discipline
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Columns added after the first release. CREATE TABLE IF NOT EXISTS does
	// nothing to a table that already exists, so each new column needs its own
	// idempotent ALTER; "duplicate column name" means it is already there.
	for _, stmt := range []string{
		`ALTER TABLE approvals ADD COLUMN note TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("migrate: %s: %w", stmt, err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFmt)
}

func parseTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, _ := time.Parse(timeFmt, v)
	return t
}

// NextJobID allocates a monotonically increasing job id like PREFIX-7.
func (s *Store) NextJobID(prefix string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var cur int
	err = tx.QueryRow(`SELECT value FROM meta WHERE key='job_seq'`).Scan(&cur)
	if err == sql.ErrNoRows {
		cur = 0
	} else if err != nil {
		return "", err
	}
	cur++
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('job_seq',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprint(cur)); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%d", prefix, cur), nil
}

func (s *Store) CreateJob(j *Job) error {
	cj, _ := json.Marshal(j.Counters)
	now := time.Now()
	j.CreatedAt, j.UpdatedAt = now, now
	if j.StateEnteredAt.IsZero() {
		j.StateEnteredAt = now
	}
	_, err := s.db.Exec(`INSERT INTO jobs
		(id,target,issue_title,issue_body,branch,worktree_path,state,prev_state,
		 state_entered_at,head_sha,resume_after,hold_reason,counters,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Target, j.IssueTitle, j.IssueBody, j.Branch, j.WorktreePath, j.State, j.PrevState,
		fmtTime(j.StateEnteredAt), j.HeadSHA, fmtTime(j.ResumeAfter), j.HoldReason, string(cj),
		fmtTime(j.CreatedAt), fmtTime(j.UpdatedAt))
	return err
}

func (s *Store) UpdateJob(j *Job) error {
	cj, _ := json.Marshal(j.Counters)
	j.UpdatedAt = time.Now()
	_, err := s.db.Exec(`UPDATE jobs SET target=?,issue_title=?,issue_body=?,branch=?,
		worktree_path=?,state=?,prev_state=?,state_entered_at=?,head_sha=?,resume_after=?,
		hold_reason=?,counters=?,updated_at=? WHERE id=?`,
		j.Target, j.IssueTitle, j.IssueBody, j.Branch, j.WorktreePath, j.State, j.PrevState,
		fmtTime(j.StateEnteredAt), j.HeadSHA, fmtTime(j.ResumeAfter), j.HoldReason, string(cj),
		fmtTime(j.UpdatedAt), j.ID)
	return err
}

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var entered, resume, created, updated, counters string
	err := row.Scan(&j.ID, &j.Target, &j.IssueTitle, &j.IssueBody, &j.Branch, &j.WorktreePath,
		&j.State, &j.PrevState, &entered, &j.HeadSHA, &resume, &j.HoldReason, &counters,
		&created, &updated)
	if err != nil {
		return nil, err
	}
	j.StateEnteredAt = parseTime(entered)
	j.ResumeAfter = parseTime(resume)
	j.CreatedAt = parseTime(created)
	j.UpdatedAt = parseTime(updated)
	_ = json.Unmarshal([]byte(counters), &j.Counters)
	return &j, nil
}

const jobCols = `id,target,issue_title,issue_body,branch,worktree_path,state,prev_state,
	state_entered_at,head_sha,resume_after,hold_reason,counters,created_at,updated_at`

func (s *Store) GetJob(id string) (*Job, error) {
	return scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
}

func (s *Store) ListJobs() ([]*Job, error) {
	rows, err := s.db.Query(`SELECT ` + jobCols + ` FROM jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) AddEvent(e *Event) error {
	e.CreatedAt = time.Now()
	res, err := s.db.Exec(`INSERT INTO events
		(job_id,state,kind,agent,backend,model,effort,binary_version,prompt_hash,
		 input_ref,output_ref,exit_code,duration_ms,head_before,head_after,
		 tokens_in,tokens_out,detail,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.JobID, e.State, e.Kind, e.Agent, e.Backend, e.Model, e.Effort, e.BinaryVersion,
		e.PromptHash, e.InputRef, e.OutputRef, e.ExitCode, e.DurationMS, e.HeadBefore,
		e.HeadAfter, e.TokensIn, e.TokensOut, e.Detail, fmtTime(e.CreatedAt))
	if err != nil {
		return err
	}
	e.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) ListEvents(jobID string) ([]*Event, error) {
	rows, err := s.db.Query(`SELECT id,job_id,state,kind,agent,backend,model,effort,
		binary_version,prompt_hash,input_ref,output_ref,exit_code,duration_ms,
		head_before,head_after,tokens_in,tokens_out,detail,created_at
		FROM events WHERE job_id=? ORDER BY id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		var created string
		if err := rows.Scan(&e.ID, &e.JobID, &e.State, &e.Kind, &e.Agent, &e.Backend, &e.Model,
			&e.Effort, &e.BinaryVersion, &e.PromptHash, &e.InputRef, &e.OutputRef, &e.ExitCode,
			&e.DurationMS, &e.HeadBefore, &e.HeadAfter, &e.TokensIn, &e.TokensOut, &e.Detail,
			&created); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(created)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// CountEvents returns the number of events of a kind for a job (used for
// sequence numbers in artifact/log file names).
func (s *Store) CountEvents(jobID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE job_id=?`, jobID).Scan(&n)
	return n, err
}

func (s *Store) AddApproval(a *Approval) error {
	a.CreatedAt = time.Now()
	res, err := s.db.Exec(`INSERT INTO approvals(job_id,gate,decision,reason,note,cancel,consumed,created_at)
		VALUES (?,?,?,?,?,?,0,?)`,
		a.JobID, a.Gate, a.Decision, a.Reason, a.Note, boolInt(a.Cancel), fmtTime(a.CreatedAt))
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	return nil
}

// PendingApproval returns the oldest unconsumed approval for a job+gate.
func (s *Store) PendingApproval(jobID, gate string) (*Approval, error) {
	row := s.db.QueryRow(`SELECT id,job_id,gate,decision,reason,note,cancel,consumed,created_at
		FROM approvals WHERE job_id=? AND gate=? AND consumed=0 ORDER BY id LIMIT 1`, jobID, gate)
	var a Approval
	var cancel, consumed int
	var created string
	err := row.Scan(&a.ID, &a.JobID, &a.Gate, &a.Decision, &a.Reason, &a.Note, &cancel, &consumed, &created)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.Cancel = cancel != 0
	a.Consumed = consumed != 0
	a.CreatedAt = parseTime(created)
	return &a, nil
}

func (s *Store) ConsumeApproval(id int64) error {
	_, err := s.db.Exec(`UPDATE approvals SET consumed=1 WHERE id=?`, id)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
