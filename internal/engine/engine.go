// Package engine hosts the orchestrator core (SPEC §2, §3): the scheduler
// loop, the state machine dispatch, crash-resume reconciliation, quota
// suspension, human-gate handling, and the single-engine lock.
package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/agent"
	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/gitx"
	"github.com/vipinm/sdlc-orchestrator/internal/resource"
	"github.com/vipinm/sdlc-orchestrator/internal/review"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

type Engine struct {
	cfg    *config.Config
	st     *store.Store
	runner *agent.Runner
	res    *resource.Manager
	logger *log.Logger

	mu         sync.Mutex
	inFlight   map[string]bool
	reconciled map[string]bool
	announced  map[string]gateNotice
	sem        chan struct{}
	wg         sync.WaitGroup

	lockFile *os.File
}

func New(cfg *config.Config, st *store.Store, logger *log.Logger) *Engine {
	if logger == nil {
		logger = log.New(os.Stdout, "", log.LstdFlags)
	}
	return &Engine{
		cfg:        cfg,
		st:         st,
		runner:     agent.NewRunner(),
		res:        resource.NewManager(cfg.Resources),
		logger:     logger,
		inFlight:   map[string]bool{},
		reconciled: map[string]bool{},
		announced:  map[string]gateNotice{},
		sem:        make(chan struct{}, cfg.Orchestrator.MaxParallelJobs),
	}
}

// acquireLock enforces one engine per data dir. The lock file is held open
// for the process lifetime; on Windows an orphaned lock from a crashed engine
// is removable (no open handle), a live one is not.
func (e *Engine) acquireLock() error {
	path := e.cfg.Orchestrator.LockFile
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		if rmErr := os.Remove(path); rmErr != nil {
			return fmt.Errorf("another engine appears to be running (lock %s held): %v", path, rmErr)
		}
		e.logger.Printf("removed stale engine lock %s", path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("acquire engine lock %s: %w", path, err)
	}
	fmt.Fprintf(f, "pid=%d started=%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	e.lockFile = f
	return nil
}

func (e *Engine) releaseLock() {
	if e.lockFile != nil {
		e.lockFile.Close()
		os.Remove(e.cfg.Orchestrator.LockFile)
	}
}

// Run drives the engine until ctx is cancelled. If once is true it exits as
// soon as no job is runnable or in flight.
func (e *Engine) Run(ctx context.Context, once bool) error {
	if err := e.acquireLock(); err != nil {
		return err
	}
	defer e.releaseLock()
	e.logger.Printf("engine started (max_parallel_jobs=%d, poll=%s)",
		e.cfg.Orchestrator.MaxParallelJobs, e.cfg.Orchestrator.PollInterval)
	ticker := time.NewTicker(e.cfg.Orchestrator.PollInterval.D())
	defer ticker.Stop()
	for {
		dispatched, pending, err := e.tick(ctx)
		if err != nil {
			e.logger.Printf("tick error: %v", err)
		}
		if once && !dispatched && pending == 0 && !e.anyInFlight() {
			e.wg.Wait()
			// A finished step may have produced new runnable work; loop once
			// more to be sure before exiting.
			if d2, p2, _ := e.tick(ctx); !d2 && p2 == 0 && !e.anyInFlight() {
				e.logger.Printf("no runnable work; exiting (--once)")
				return nil
			}
			continue
		}
		select {
		case <-ctx.Done():
			e.logger.Printf("shutting down; waiting for in-flight steps...")
			e.wg.Wait()
			return nil
		case <-ticker.C:
		}
	}
}

func (e *Engine) anyInFlight() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.inFlight) > 0
}

// tick scans jobs once: control rows, holds, gates, and dispatch.
// Returns whether it dispatched work and how many jobs are still runnable.
func (e *Engine) tick(ctx context.Context) (dispatched bool, pending int, err error) {
	jobs, err := e.st.ListJobs()
	if err != nil {
		return false, 0, err
	}
	now := time.Now()
	for _, j := range jobs {
		if isTerminal(j.State) {
			continue
		}
		if e.isInFlight(j.ID) {
			pending++
			continue
		}
		if err := e.handleControls(ctx, j); err != nil {
			e.logger.Printf("%s: control handling: %v", j.ID, err)
		}
		if isTerminal(j.State) {
			continue
		}
		// Announced after handleControls, so a job whose cancel or resume row
		// just landed is no longer at a gate and is not announced, and before
		// the held-job skip, because a held job is exactly the case that used
		// to leave the console silent.
		if gate := review.GateFor(j.State); gate != "" {
			e.announceGate(ctx, j, gate)
		}
		if isHeld(j.State) {
			continue
		}
		// Job-age budget.
		if now.Sub(j.CreatedAt) > e.cfg.Limits.MaxJobDuration.D() {
			e.hold(j, STimedOut, fmt.Sprintf("exceeded limits.max_job_duration (%s)", e.cfg.Limits.MaxJobDuration))
			continue
		}
		// Quota suspension.
		if j.State == SBlockedQuota {
			if !j.ResumeAfter.IsZero() && now.After(j.ResumeAfter) {
				e.logger.Printf("%s: quota backoff elapsed; resuming %s", j.ID, j.PrevState)
				j.ResumeAfter = time.Time{}
				e.transition(j, j.PrevState, "quota backoff elapsed")
			} else {
				continue
			}
		}
		// Generic per-state delay (release retry backoff, device wait).
		if !j.ResumeAfter.IsZero() {
			if now.Before(j.ResumeAfter) {
				continue
			}
			j.ResumeAfter = time.Time{}
			_ = e.st.UpdateJob(j)
		}
		// Human gates.
		if isParked(j.State) {
			if err := e.handleGate(ctx, j); err != nil {
				e.logger.Printf("%s: gate: %v", j.ID, err)
			}
			if isParked(j.State) || isTerminal(j.State) || isHeld(j.State) {
				continue
			}
			// fallthrough: approval transitioned it to a runnable state
		}
		pending++
		select {
		case e.sem <- struct{}{}:
		default:
			continue // parallel cap reached
		}
		e.setInFlight(j.ID, true)
		dispatched = true
		e.wg.Add(1)
		go func(j *store.Job) {
			defer e.wg.Done()
			defer func() { e.setInFlight(j.ID, false); <-e.sem }()
			e.step(ctx, j)
		}(j)
	}
	return dispatched, pending, nil
}

func (e *Engine) isInFlight(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inFlight[id]
}

func (e *Engine) setInFlight(id string, v bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v {
		e.inFlight[id] = true
	} else {
		delete(e.inFlight, id)
	}
}

// handleControls consumes cancel/resume rows written by the CLI.
func (e *Engine) handleControls(ctx context.Context, j *store.Job) error {
	if a, err := e.st.PendingApproval(j.ID, "cancel"); err != nil {
		return err
	} else if a != nil {
		_ = e.st.ConsumeApproval(a.ID)
		e.event(j, "approval", map[string]any{"gate": "cancel", "reason": a.Reason})
		e.transition(j, SCancelled, "cancelled by user: "+a.Reason)
		e.cleanup(ctx, j)
		return nil
	}
	if a, err := e.st.PendingApproval(j.ID, "resume"); err != nil {
		return err
	} else if a != nil {
		if isHeld(j.State) || j.State == SBlockedQuota {
			_ = e.st.ConsumeApproval(a.ID)
			to := strings.TrimSpace(a.Reason)
			if to == "" {
				to = j.PrevState
			}
			if to == "" {
				e.logger.Printf("%s: resume requested but no prior state recorded", j.ID)
				return nil
			}
			if !IsResumableState(to) {
				e.logger.Printf("%s: refusing to resume into %q (not a runnable state)", j.ID, to)
				e.event(j, "note", map[string]any{"resume_rejected": to, "valid": ResumableStates()})
				return nil
			}
			j.ResumeAfter = time.Time{}
			j.HoldReason = ""
			detail := map[string]any{"gate": "resume", "to": to}
			// A note is an instruction addressed to the agent, not to the
			// orchestrator. It rides the same channel a merge-gate rejection
			// uses, which is the one mode where the FIXING prompt drops its
			// classification-derived restrictions and asks the agent to do
			// what the human said. Clearing LastAnalysis is what scopes the
			// grant to this one attempt.
			if note := strings.TrimSpace(a.Note); note != "" {
				j.Counters.HumanRejectReason = note
				j.Counters.FixSource = "human"
				j.Counters.LastAnalysis = ""
				detail["note"] = note
			}
			e.event(j, "approval", detail)
			e.transition(j, to, "resumed by user")
		} else {
			// Not held: consume so it doesn't linger.
			_ = e.st.ConsumeApproval(a.ID)
		}
	}
	return nil
}

// handleGate consumes the approval row a parked job is waiting on.
//
// The gate name comes from review.GateFor so that the state->gate mapping
// lives in exactly one place: the document the operator reads, the row the CLI
// writes and the arm chosen here are then guaranteed to agree, and adding a
// gate does not mean remembering to update a second switch in here.
func (e *Engine) handleGate(ctx context.Context, j *store.Job) error {
	gate := review.GateFor(j.State)
	if gate == "" || gate == review.GateHold {
		// A held job is cleared by handleControls with resume or cancel; there
		// is no approval row for it to consume.
		return nil
	}
	a, err := e.st.PendingApproval(j.ID, gate)
	if err != nil || a == nil {
		return err
	}
	_ = e.st.ConsumeApproval(a.ID)
	e.event(j, "approval", map[string]any{"gate": gate, "decision": a.Decision, "reason": a.Reason})
	switch {
	case gate == review.GateScope && a.Decision == "approve":
		// Approval with questions outstanding is a waiver: planning proceeds on
		// the recorded assumptions. Naming the waived questions is the point —
		// a waiver nobody can find later is indistinguishable from a question
		// that was never asked.
		if prob, err := artifact.LoadProblem(
			filepath.Join(artifact.ArtifactsDir(e.cfg.Orchestrator.DataDir, j.ID), "problem.json"),
		); err == nil {
			if blocking := prob.BlockingQuestions(); len(blocking) > 0 {
				ids := make([]string, 0, len(blocking))
				for _, q := range blocking {
					ids = append(ids, q.ID)
				}
				e.event(j, "note", map[string]any{"scope_questions_waived": ids})
			}
		}
		e.transition(j, SPlanning, "scope approved")
	case gate == review.GateScope && a.Decision == "reject":
		if a.Cancel {
			e.transition(j, SCancelled, "scope rejected (cancelled): "+a.Reason)
			e.cleanup(ctx, j)
		} else {
			// Unlike the spec gate, this counter IS spent on a human decision:
			// re-scoping is a conversation between the operator and the agent,
			// and max_scope_rounds is what stops it running forever.
			j.Counters.ScopeRounds++
			j.Counters.HumanRejectReason = a.Reason
			e.transition(j, SScoping, "scope rejected: "+a.Reason)
		}
	case gate == review.GateSpec && a.Decision == "approve":
		e.transition(j, SImplementing, "spec approved")
	case gate == review.GateSpec && a.Decision == "reject":
		if a.Cancel {
			e.transition(j, SCancelled, "spec rejected (cancelled): "+a.Reason)
			e.cleanup(ctx, j)
		} else {
			// Back to PLANNING with the objection attached. The review-round
			// counters are deliberately not touched: they budget automated
			// rework, and spending them on a human decision would turn two
			// human "no"s into an escalation.
			j.Counters.HumanRejectReason = a.Reason
			e.transition(j, SPlanning, "spec rejected: "+a.Reason)
		}
	case gate == review.GateCode && a.Decision == "approve":
		e.transition(j, SBuilding, "code approved")
	case gate == review.GateCode && a.Decision == "reject":
		if a.Cancel {
			e.transition(j, SCancelled, "code rejected (cancelled): "+a.Reason)
			e.cleanup(ctx, j)
		} else {
			j.Counters.HumanRejectReason = a.Reason
			j.Counters.FixSource = "human"
			e.transition(j, SFixing, "code rejected: "+a.Reason)
		}
	case gate == "merge" && a.Decision == "approve":
		e.transition(j, SMerging, "merge approved")
	case gate == "merge" && a.Decision == "reject":
		if a.Cancel {
			e.transition(j, SCancelled, "merge rejected (cancelled): "+a.Reason)
			e.cleanup(ctx, j)
		} else {
			j.Counters.HumanRejectReason = a.Reason
			j.Counters.FixSource = "human"
			e.transition(j, SFixing, "merge rejected: "+a.Reason)
		}
	case gate == "release" && a.Decision == "approve":
		e.transition(j, SReleasing, "release approved")
	case gate == "release" && a.Decision == "reject":
		j.HoldReason = "merged; release rejected: " + a.Reason
		e.transition(j, SCompleted, j.HoldReason)
		e.cleanup(ctx, j)
	}
	return nil
}

// step executes one state handler for the job.
func (e *Engine) step(ctx context.Context, j *store.Job) {
	defer func() {
		if r := recover(); r != nil {
			e.logger.Printf("%s: PANIC in state %s: %v", j.ID, j.State, r)
			e.hold(j, SEscalated, fmt.Sprintf("panic in %s: %v", j.State, r))
		}
	}()
	target, err := e.cfg.Target(j.Target)
	if err != nil {
		e.hold(j, SEscalated, err.Error())
		return
	}
	jc := &jobCtx{e: e, job: j, target: target, repo: gitx.Repo{Root: target.RepoPath}}

	// Lazy crash-resume reconciliation, once per job per engine start.
	e.mu.Lock()
	needRec := !e.reconciled[j.ID]
	e.reconciled[j.ID] = true
	e.mu.Unlock()
	if needRec && j.State != SCreated && needsWorktreeReconcile(j.State) {
		if err := jc.reconcile(ctx); err != nil {
			e.hold(j, SEscalated, fmt.Sprintf("resume reconciliation failed: %v", err))
			return
		}
	}

	// The branch can move without the orchestrator: a human inspecting an
	// escalated job commits a fix on it. Nothing else notices — HeadSHA is
	// written only by our own commit() — so every downstream "what did the
	// agent change since state entry" diff would charge those commits to the
	// agent and the test-file guard would roll them back. Re-read the branch
	// before dispatching so the record matches reality.
	if j.State != SCreated && needsWorktreeReconcile(j.State) {
		if err := jc.syncHead(ctx); err != nil {
			e.logger.Printf("%s: head resync: %v", j.ID, err)
		}
	}

	// Agent-invocation budget guardrail (SPEC §13.5).
	if isAgentState(j.State) && j.Counters.AgentInvocations >= e.cfg.Limits.MaxAgentInvocationsPerJob {
		e.hold(j, SEscalated, fmt.Sprintf("agent budget exhausted (%d invocations)", j.Counters.AgentInvocations))
		return
	}

	e.logger.Printf("%s: running %s", j.ID, j.State)
	var next string
	var herr error
	switch j.State {
	case SCreated:
		next, herr = jc.handleCreated(ctx)
	case SScoping:
		next, herr = jc.handleScoping(ctx)
	case SPlanning:
		next, herr = jc.handlePlanning(ctx)
	case SDesignReview:
		next, herr = jc.handleDesignReview(ctx)
	case SImplementing:
		next, herr = jc.handleImplementing(ctx)
	case SCodeReview:
		next, herr = jc.handleCodeReview(ctx)
	case SBuilding:
		next, herr = jc.handleBuilding(ctx)
	case STesting:
		next, herr = jc.handleTesting(ctx)
	case SFlakeCheck:
		next, herr = jc.handleFlakeCheck(ctx)
	case SAnalyzing:
		next, herr = jc.handleAnalyzing(ctx)
	case SFixing:
		next, herr = jc.handleFixing(ctx)
	case SFinalReview:
		next, herr = jc.handleFinalReview(ctx)
	case SMerging:
		next, herr = jc.handleMerging(ctx)
	case SReleasing:
		next, herr = jc.handleReleasing(ctx)
	default:
		e.hold(j, SEscalated, "no handler for state "+j.State)
		return
	}
	if herr != nil {
		if q, ok := herr.(quotaErr); ok {
			j.ResumeAfter = time.Now().Add(q.backoff)
			e.event(j, "note", map[string]any{"quota": true, "backend": q.backend, "resume_after": j.ResumeAfter})
			e.transition(j, SBlockedQuota, fmt.Sprintf("quota/rate limit on backend %s", q.backend))
			return
		}
		// Whatever the state produced before it failed is exactly what the
		// operator is about to inspect, and a dirty worktree is one reconcile
		// away from deletion. Commit it before parking the job.
		jc.preserveWork(ctx, herr)
		if h, ok := herr.(holdErr); ok {
			e.hold(j, h.state, h.reason)
			return
		}
		e.logger.Printf("%s: %s failed: %v", j.ID, j.State, herr)
		e.hold(j, SEscalated, fmt.Sprintf("%s: %v", j.State, herr))
		return
	}
	if next == j.State {
		_ = e.st.UpdateJob(j) // stay (e.g. retry with ResumeAfter); persist counters
		return
	}
	e.transition(j, next, "")
	if next == SCompleted {
		e.cleanup(ctx, j)
	}
}

// quotaErr suspends a job instead of failing it (SPEC §10).
type quotaErr struct {
	backend string
	backoff time.Duration
}

func (q quotaErr) Error() string { return "quota exhausted on " + q.backend }

// holdErr moves a job to a durable hold (usually ESCALATED) with a reason.
type holdErr struct {
	state  string
	reason string
}

func (h holdErr) Error() string { return h.state + ": " + h.reason }

func escalate(format string, a ...any) holdErr {
	return holdErr{state: SEscalated, reason: fmt.Sprintf(format, a...)}
}

// transition moves the job to next, recording an event and resetting the
// per-state agent-retry counter.
func (e *Engine) transition(j *store.Job, next, note string) {
	prev := j.State
	j.PrevState = prev
	j.State = next
	j.StateEnteredAt = time.Now()
	j.Counters.AgentRetries = 0
	// The next state measures its work from wherever the branch is when it
	// starts — including any commits a human made while the job was held.
	j.Counters.StateEntryHead = ""
	// The job is leaving whatever gate it was at, so its notice is stale. Drop
	// it here rather than in the gate handlers: this is the one place every
	// state change goes through, and it is what makes a job that parks again
	// later, on a different gate, announce again instead of staying silent.
	e.mu.Lock()
	delete(e.announced, j.ID)
	e.mu.Unlock()
	if err := e.st.UpdateJob(j); err != nil {
		e.logger.Printf("%s: persist transition %s->%s: %v", j.ID, prev, next, err)
	}
	e.event(j, "transition", map[string]any{"from": prev, "to": next, "note": note})
	e.logger.Printf("%s: %s -> %s %s", j.ID, prev, next, note)
}

func (e *Engine) hold(j *store.Job, state, reason string) {
	j.HoldReason = reason
	e.transition(j, state, reason)
}

func (e *Engine) event(j *store.Job, kind string, detail map[string]any) {
	ev := &store.Event{JobID: j.ID, State: j.State, Kind: kind, Detail: jsonStr(detail)}
	if err := e.st.AddEvent(ev); err != nil {
		e.logger.Printf("%s: record event: %v", j.ID, err)
	}
}

// cleanup removes the worktree (and optionally branch) per git.* config.
func (e *Engine) cleanup(ctx context.Context, j *store.Job) {
	mode := e.cfg.Git.CleanupWorktrees
	success := j.State == SCompleted
	if mode == "never" || (mode == "on_success" && !success) {
		return
	}
	target, err := e.cfg.Target(j.Target)
	if err != nil {
		return
	}
	repo := gitx.Repo{Root: target.RepoPath}
	if err := repo.RemoveWorktree(ctx, j.WorktreePath); err != nil {
		e.logger.Printf("%s: cleanup worktree: %v", j.ID, err)
	}
	if success && e.cfg.Git.DeleteBranchOnSuccess {
		if err := repo.DeleteBranch(ctx, j.Branch); err != nil {
			e.logger.Printf("%s: delete branch: %v", j.ID, err)
		}
	}
}
