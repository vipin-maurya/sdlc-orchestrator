package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
	"github.com/vipinm/sdlc-orchestrator/internal/execx"
	"github.com/vipinm/sdlc-orchestrator/internal/guard"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// --- CREATED -------------------------------------------------------------

func (c *jobCtx) handleCreated(ctx context.Context) (string, error) {
	if err := c.repo.GitOK(ctx); err != nil {
		return "", fmt.Errorf("target repo %s is not a git repository: %w", c.target.RepoPath, err)
	}
	for _, pb := range c.e.cfg.Policies.ProtectedBranches {
		if c.job.Branch == pb {
			return "", fmt.Errorf("job branch %q is a protected branch", c.job.Branch)
		}
	}
	if err := c.repo.EnsureWorktree(ctx, c.job.WorktreePath, c.job.Branch, c.target.DefaultBranch); err != nil {
		return "", fmt.Errorf("provision worktree: %w", err)
	}
	if err := c.repo.ExcludePattern(".sdlc/"); err != nil {
		return "", err
	}
	if err := artifact.EnsureLayout(c.dataDir(), c.job.ID, c.job.IssueTitle, c.job.IssueBody); err != nil {
		return "", err
	}
	sha, err := c.repo.HeadSHA(ctx, c.job.WorktreePath)
	if err != nil {
		return "", err
	}
	c.job.HeadSHA = sha
	c.job.Counters.BaseSHA = sha
	if err := c.e.st.UpdateJob(c.job); err != nil {
		return "", err
	}
	return SPlanning, nil
}

// --- PLANNING ------------------------------------------------------------

func (c *jobCtx) handlePlanning(ctx context.Context) (string, error) {
	if err := c.resetExchange(); err != nil {
		return "", err
	}
	pctx := c.baseCtx()
	pctx.Round = c.job.Counters.DesignReviewRounds + 1
	pctx.MaxRounds = c.e.cfg.Limits.MaxDesignReviewRounds
	rejected := c.job.Counters.HumanRejectReason
	// A human rejection does not increment DesignReviewRounds — that counter
	// budgets automated rework — so staging on the counter alone would send
	// the planner back in with nothing to revise, and it would write a new
	// spec from scratch instead of answering the objection.
	if c.job.Counters.DesignReviewRounds > 0 || rejected != "" {
		c.stageArtifact("spec.json")
		c.stageArtifact("plan.json")
	}
	if c.job.Counters.DesignReviewRounds > 0 {
		pctx.PrevFindings = c.findingsJSON(fmt.Sprintf("design_review.r%d.json", c.job.Counters.DesignReviewRounds))
	}
	pctx.RejectReason = rejected
	err := c.runAgent(ctx, SPlanning, pctx, func() error {
		if _, err := artifact.LoadSpec(filepath.Join(c.sdlcDir(), "spec.json")); err != nil {
			return err
		}
		if _, err := artifact.LoadPlan(filepath.Join(c.sdlcDir(), "plan.json")); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if _, err := c.harvest("spec.json", "spec.json"); err != nil {
		return "", err
	}
	if _, err := c.harvest("plan.json", "plan.json"); err != nil {
		return "", err
	}
	// The objection applied to this one attempt. Clearing it here — the same
	// scoping handleFixing uses — is what stops the next re-plan, for whatever
	// reason, from being handed a stale instruction it has already answered.
	c.job.Counters.HumanRejectReason = ""
	c.job.Counters.FixSource = ""
	c.discardTreeChanges(ctx) // planner must not leave code edits behind
	return SDesignReview, nil
}

// --- DESIGN_REVIEW -------------------------------------------------------

func (c *jobCtx) handleDesignReview(ctx context.Context) (string, error) {
	round := c.job.Counters.DesignReviewRounds + 1
	if err := c.resetExchange(); err != nil {
		return "", err
	}
	c.stageArtifact("spec.json")
	c.stageArtifact("plan.json")
	pctx := c.baseCtx()
	pctx.Round = round
	pctx.MaxRounds = c.e.cfg.Limits.MaxDesignReviewRounds
	reviewPath := filepath.Join(c.sdlcDir(), "review.json")
	err := c.runAgent(ctx, SDesignReview, pctx, func() error {
		if _, err := artifact.LoadReview(reviewPath); err != nil {
			return err
		}
		return c.requireCleanTree(ctx)
	})
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("design_review.r%d.json", round)
	if _, err := c.harvest("review.json", name); err != nil {
		return "", err
	}
	rev, err := artifact.LoadReview(filepath.Join(c.artDir(), name))
	if err != nil {
		return "", err
	}
	threshold := c.e.cfg.Policies.DesignReviewBlocksAt
	// Nothing downstream can tell a correct finding from a fluent wrong one, so
	// anything about to gate is put to independent verifiers first. rev is
	// updated in place: refuted findings survive as annotated nits.
	if _, err := c.verifyFindings(ctx, rev, name, threshold); err != nil {
		return "", err
	}
	if blocking := rev.Blocking(threshold); len(blocking) > 0 {
		c.job.Counters.DesignReviewRounds = round
		if round >= c.e.cfg.Limits.MaxDesignReviewRounds {
			return "", escalate("design review still has %d finding(s) at or above %s after %d round(s); see %s",
				len(blocking), threshold, round, name)
		}
		return SPlanning, nil
	}
	// The gate passed, but findings below the threshold are real observations
	// about a plan nobody has implemented yet. Remember which artifact holds
	// them so IMPLEMENTING can be handed them instead of discarding them.
	c.job.Counters.LastDesignReview = ""
	if len(rev.Findings) > 0 {
		// Prefer the verified copy when there is one: a finding the verifiers
		// refuted must reach the implementer *with* its refutation attached,
		// or the implementer reworks correct code on a dismissed claim.
		c.job.Counters.LastDesignReview = c.preferVerified(name)
		c.e.event(c.job, "note", map[string]any{
			"design_review_passed_with_findings": len(rev.Findings), "threshold": threshold, "artifact": name,
		})
	}
	// The optional human gate sits below the LastDesignReview bookkeeping on
	// purpose: approving into IMPLEMENTING must still forward the non-blocking
	// findings, or the checkpoint the operator asked for becomes the place
	// those findings quietly disappear.
	if c.e.cfg.Policies.HumanGate("spec") {
		c.e.event(c.job, "note", map[string]any{
			"awaiting_spec_approval": true,
			"design_summary":         rev.Summary,
			"findings":               len(rev.Findings),
		})
		return SAwaitSpec, nil
	}
	return SImplementing, nil
}

// --- IMPLEMENTING --------------------------------------------------------

func (c *jobCtx) handleImplementing(ctx context.Context) (string, error) {
	if err := c.resetExchange(); err != nil {
		return "", err
	}
	c.stageArtifact("spec.json")
	c.stageArtifact("plan.json")
	pctx := c.baseCtx()
	// Non-blocking design-review findings: the reviewer read this plan and saw
	// something the planner did not. Nobody else will look at the plan again,
	// so this is the last point at which those findings can reach anyone.
	if name := c.job.Counters.LastDesignReview; name != "" {
		c.stageArtifactAs(name, "design_review.json")
		pctx.PrevFindings = c.findingsJSON(name)
	}
	plan, err := artifact.LoadPlan(filepath.Join(c.artDir(), "plan.json"))
	if err != nil {
		return "", err
	}
	implPath := filepath.Join(c.sdlcDir(), "implementation.json")
	entryHead, err := c.stateBaseline(ctx)
	if err != nil {
		return "", err
	}
	var unreported []string
	err = c.runAgent(ctx, SImplementing, pctx, func() error {
		impl, err := artifact.LoadImplementation(implPath)
		if err != nil {
			return err
		}
		// Every plan step must be accounted for. Silence about a step is
		// indistinguishable from "I forgot", so it fails the state (SPEC §6.1).
		if missing := impl.CheckStepCoverage(plan); len(missing) > 0 {
			return fmt.Errorf("steps_completed does not account for plan step(s) %s; every step in plan.json must appear, either done or skipped with a note",
				strings.Join(missing, ", "))
		}
		changed, err := c.repo.DiffNamesSince(ctx, c.job.WorktreePath, entryHead)
		if err != nil {
			return err
		}
		if len(changed) == 0 {
			return fmt.Errorf("implementation produced no code changes")
		}
		claimed := append(append([]string{}, impl.FilesChanged...), impl.TestsAddedOrChanged...)
		var phantom []string
		phantom, unreported = reconcileClaims(claimed, changed)
		if len(phantom) > 0 {
			return fmt.Errorf("reported as changed but unmodified in the worktree: %s",
				strings.Join(phantom, ", "))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if _, err := c.harvest("implementation.json", "implementation.json"); err != nil {
		return "", err
	}
	impl, err := artifact.LoadImplementation(filepath.Join(c.artDir(), "implementation.json"))
	if err != nil {
		return "", err
	}
	// Divergences that are defensible but must not be silent: work done off
	// the report, and planned steps deliberately not done.
	if len(unreported) > 0 {
		c.e.event(c.job, "note", map[string]any{"changed_but_unreported": unreported})
	}
	if skipped := impl.SkippedSteps(); len(skipped) > 0 {
		c.e.event(c.job, "note", map[string]any{"plan_steps_skipped": skipped})
	}
	if err := c.commit(ctx, SImplementing, impl.Summary); err != nil {
		return "", err
	}
	return SCodeReview, nil
}

// --- CODE_REVIEW ---------------------------------------------------------

func (c *jobCtx) handleCodeReview(ctx context.Context) (string, error) {
	round := c.job.Counters.CodeReviewRounds + 1
	if err := c.resetExchange(); err != nil {
		return "", err
	}
	c.stageArtifact("spec.json")
	c.stageArtifact("plan.json")
	c.stageArtifact("implementation.json")
	if err := c.stageDiffPatch(ctx); err != nil {
		return "", err
	}
	pctx := c.baseCtx()
	pctx.Round = round
	pctx.MaxRounds = c.e.cfg.Limits.MaxCodeReviewRounds
	if round > 1 {
		pctx.PrevFindings = c.findingsJSON(c.preferVerified(fmt.Sprintf("code_review.r%d.json", round-1)))
	}
	reviewPath := filepath.Join(c.sdlcDir(), "review.json")
	err := c.runAgent(ctx, SCodeReview, pctx, func() error {
		if _, err := artifact.LoadReview(reviewPath); err != nil {
			return err
		}
		return c.requireCleanTree(ctx)
	})
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("code_review.r%d.json", round)
	if _, err := c.harvest("review.json", name); err != nil {
		return "", err
	}
	rev, err := artifact.LoadReview(filepath.Join(c.artDir(), name))
	if err != nil {
		return "", err
	}
	threshold := c.e.cfg.Policies.CodeReviewBlocksAt
	if _, err := c.verifyFindings(ctx, rev, name, threshold); err != nil {
		return "", err
	}
	if blocking := rev.Blocking(threshold); len(blocking) > 0 {
		c.job.Counters.CodeReviewRounds = round
		if round >= c.e.cfg.Limits.MaxCodeReviewRounds {
			return "", escalate("code review still has %d finding(s) at or above %s after %d round(s); see %s",
				len(blocking), threshold, round, name)
		}
		if c.job.Counters.FixAttempts >= c.e.cfg.Limits.MaxFixAttempts {
			return "", escalate("code review has %d finding(s) at or above %s but fix budget (%d) is exhausted",
				len(blocking), threshold, c.e.cfg.Limits.MaxFixAttempts)
		}
		c.job.Counters.FixSource = "code_review"
		return SFixing, nil
	}
	// A human gate is a checkpoint on the pass path only. The blocking branch
	// above still routes to FIXING without asking anybody: the automated
	// verdict is not something a human is invited to override here.
	if c.e.cfg.Policies.HumanGate("code") {
		stat, _ := c.repo.DiffStatSince(ctx, c.job.WorktreePath, c.job.Counters.BaseSHA)
		c.e.event(c.job, "note", map[string]any{
			"awaiting_code_approval": true, "diff_stat": stat, "code_review_summary": rev.Summary,
		})
		return SAwaitCode, nil
	}
	return SBuilding, nil
}

// --- BUILDING ------------------------------------------------------------

func (c *jobCtx) handleBuilding(ctx context.Context) (string, error) {
	cmds := append([][]string{}, c.target.Build.Commands...)
	if c.target.Lint.RunIn == "building" && len(c.target.Lint.Command) > 0 {
		cmds = append(cmds, c.target.Lint.Command)
	}
	for _, argv := range cmds {
		exit, logPath, err := c.runPhase(ctx, "build", argv, c.target.Build.Timeout.D(), nil)
		if err != nil {
			return "", err
		}
		if exit != 0 {
			c.job.Counters.FailedPhase = "build"
			c.job.Counters.LastFailureLog = logPath
			return SAnalyzing, nil
		}
	}
	return STesting, nil
}

// --- TESTING -------------------------------------------------------------

func (c *jobCtx) handleTesting(ctx context.Context) (string, error) {
	// Unit tests.
	exit, logPath, err := c.runPhase(ctx, "unit", c.target.UnitTest.Command, c.target.UnitTest.Timeout.D(), nil)
	if err != nil {
		return "", err
	}
	if exit != 0 {
		c.job.Counters.FailedPhase = "unit"
		c.job.Counters.LastFailureLog = logPath
		return SFlakeCheck, nil
	}
	// Lint (when configured to run here).
	if c.target.Lint.RunIn == "testing" && len(c.target.Lint.Command) > 0 {
		exit, logPath, err := c.runPhase(ctx, "lint", c.target.Lint.Command, c.target.Lint.Timeout.D(), nil)
		if err != nil {
			return "", err
		}
		if exit != 0 {
			c.job.Counters.FailedPhase = "lint"
			c.job.Counters.LastFailureLog = logPath
			return SFlakeCheck, nil
		}
	}
	// UI tests on a leased device.
	if c.target.UITest.Enabled {
		serial, release, err := c.e.res.AcquireDevice(ctx)
		if err != nil {
			return "", fmt.Errorf("device acquisition: %w", err)
		}
		if serial == "" {
			switch c.target.UITest.OnNoDevice {
			case "skip":
				c.job.Counters.UISkipped = true
				c.e.event(c.job, "note", map[string]any{"ui_tests": "skipped, no device available"})
			case "fail":
				c.job.Counters.FailedPhase = "ui"
				c.job.Counters.LastFailureLog = ""
				return SAnalyzing, nil
			case "wait":
				c.job.ResumeAfter = time.Now().Add(5 * time.Minute)
				c.e.event(c.job, "note", map[string]any{"ui_tests": "no device; waiting"})
				return STesting, nil
			}
		} else {
			defer release()
			c.e.event(c.job, "note", map[string]any{"ui_device": serial})
			exit, logPath, err := c.runPhase(ctx, "ui", c.target.UITest.Command, c.target.UITest.Timeout.D(),
				map[string]string{"ANDROID_SERIAL": serial})
			if err != nil {
				return "", err
			}
			if exit != 0 {
				c.job.Counters.FailedPhase = "ui"
				c.job.Counters.LastFailureLog = logPath
				return SFlakeCheck, nil
			}
			c.job.Counters.UISkipped = false
		}
	}
	return SFinalReview, nil
}

// --- FLAKE_CHECK (deterministic, no agent — SPEC §3.2) -------------------

func (c *jobCtx) phaseCommand(phase string) ([]string, time.Duration) {
	switch phase {
	case "unit":
		return c.target.UnitTest.Command, c.target.UnitTest.Timeout.D()
	case "lint":
		return c.target.Lint.Command, c.target.Lint.Timeout.D()
	case "ui":
		return c.target.UITest.Command, c.target.UITest.Timeout.D()
	}
	return nil, 0
}

func (c *jobCtx) handleFlakeCheck(ctx context.Context) (string, error) {
	phase := c.job.Counters.FailedPhase
	argv, timeout := c.phaseCommand(phase)
	if len(argv) == 0 {
		return SAnalyzing, nil // build failures skip flake check by design
	}
	var env map[string]string
	var releaseDevice func()
	if phase == "ui" {
		serial, release, err := c.e.res.AcquireDevice(ctx)
		if err != nil {
			return "", err
		}
		if serial == "" {
			return "", escalate("device unavailable during flake check of UI tests")
		}
		releaseDevice = release
		env = map[string]string{"ANDROID_SERIAL": serial}
	}
	if releaseDevice != nil {
		defer releaseDevice()
	}
	n := c.e.cfg.Limits.FlakeRerunCount
	passes := 0
	for i := 0; i < n; i++ {
		// Three full suite reruns look identical from outside without this:
		// say which one is in flight and how the earlier ones went.
		c.e.event(c.job, "progress", map[string]any{
			"flake_rerun": i + 1, "of": n, "phase": phase, "passes_so_far": passes,
		})
		c.e.logger.Printf("%s: FLAKE_CHECK %s rerun %d/%d (%d passed so far)", c.job.ID, phase, i+1, n, passes)
		exit, _, err := c.runPhase(ctx, fmt.Sprintf("flake-rerun-%d", i+1), argv, timeout, env)
		if err != nil {
			return "", err
		}
		if exit == 0 {
			passes++
		}
	}
	c.e.event(c.job, "note", map[string]any{"flake_check": map[string]any{"phase": phase, "reruns": n, "passes": passes}})
	if passes > 0 {
		// Non-deterministic ⇒ flaky.
		c.job.Counters.FlakeRetries++
		if c.job.Counters.FlakeRetries > c.e.cfg.Limits.MaxFlakeRetries {
			return "", escalate("phase %s is flaky (%d/%d reruns passed) and limits.max_flake_retries exhausted", phase, passes, n)
		}
		return STesting, nil
	}
	return SAnalyzing, nil
}

// --- ANALYZING -----------------------------------------------------------

func (c *jobCtx) handleAnalyzing(ctx context.Context) (string, error) {
	round := c.job.Counters.AnalysisRound + 1
	if err := c.resetExchange(); err != nil {
		return "", err
	}
	c.stageArtifact("spec.json")
	c.stageArtifact("implementation.json")
	pctx := c.baseCtx()
	pctx.Round = round
	pctx.FailedPhase = c.job.Counters.FailedPhase
	pctx.FailureExcerpt = c.excerpt(c.job.Counters.LastFailureLog)
	analysisPath := filepath.Join(c.sdlcDir(), "analysis.json")
	err := c.runAgent(ctx, SAnalyzing, pctx, func() error {
		if _, err := artifact.LoadAnalysis(analysisPath); err != nil {
			return err
		}
		return c.requireCleanTree(ctx)
	})
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("analysis.a%d.json", round)
	if _, err := c.harvest("analysis.json", name); err != nil {
		return "", err
	}
	c.job.Counters.AnalysisRound = round
	an, err := artifact.LoadAnalysis(filepath.Join(c.artDir(), name))
	if err != nil {
		return "", err
	}
	switch an.Classification {
	case "environment":
		return "", escalate("failure classified as environment problem: %s", an.Reasoning)
	case "unknown":
		return "", escalate("failure classification unknown: %s", an.Reasoning)
	}
	if c.job.Counters.FixAttempts >= c.e.cfg.Limits.MaxFixAttempts {
		return "", escalate("%s diagnosed but fix budget (%d) is exhausted", an.Classification, c.e.cfg.Limits.MaxFixAttempts)
	}
	c.job.Counters.LastAnalysis = an.Classification
	c.job.Counters.FixSource = "analysis"
	return SFixing, nil
}

// --- FIXING --------------------------------------------------------------

func (c *jobCtx) handleFixing(ctx context.Context) (string, error) {
	attempt := c.job.Counters.FixAttempts + 1
	if err := c.resetExchange(); err != nil {
		return "", err
	}
	c.stageArtifact("spec.json")
	c.stageArtifact("plan.json")
	if c.job.Counters.AnalysisRound > 0 {
		c.stageArtifactAs(fmt.Sprintf("analysis.a%d.json", c.job.Counters.AnalysisRound), "analysis.json")
	}
	pctx := c.baseCtx()
	pctx.Round = attempt
	pctx.MaxRounds = c.e.cfg.Limits.MaxFixAttempts
	classification := ""
	switch c.job.Counters.FixSource {
	case "analysis":
		classification = c.job.Counters.LastAnalysis
		pctx.Classification = classification
		pctx.FailureExcerpt = c.excerpt(c.job.Counters.LastFailureLog)
		if an, err := artifact.LoadAnalysis(filepath.Join(c.artDir(), fmt.Sprintf("analysis.a%d.json", c.job.Counters.AnalysisRound))); err == nil {
			pctx.FixHint = an.FixHint
			pctx.FailingTargets = strings.Join(an.FailingTargets, ", ")
		}
	case "code_review":
		pctx.PrevFindings = c.findingsJSON(c.preferVerified(fmt.Sprintf("code_review.r%d.json", c.job.Counters.CodeReviewRounds)))
	case "final_review":
		pctx.PrevFindings = c.findingsJSON(c.preferVerified("final_review.json"))
	case "human":
		pctx.RejectReason = c.job.Counters.HumanRejectReason
	}

	// Read the branch head now rather than trusting the job record: this sha
	// is what CheckFixDiff attributes to the agent, and what a guard violation
	// resets the branch back to. If a human committed on this branch since the
	// last orchestrator commit, a stale baseline would blame them for it and
	// then delete their work.
	entryHead, err := c.stateBaseline(ctx)
	if err != nil {
		return "", err
	}
	fixPath := filepath.Join(c.sdlcDir(), "fix.json")
	err = c.runAgent(ctx, SFixing, pctx, func() error {
		if _, err := artifact.LoadImplementation(fixPath); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	fix, err := artifact.LoadImplementation(fixPath)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("fix.f%d.json", attempt)
	if _, err := c.harvest("fix.json", name); err != nil {
		return "", err
	}
	c.job.Counters.FixAttempts = attempt

	// Legitimate escape hatch: the fix agent may request a test change
	// instead of making one (SPEC §5.1) — always a human decision.
	if fix.TestChangeRequested != nil && *fix.TestChangeRequested != "" {
		return "", escalate("fix agent requests a test change (attempt %d): %s", attempt, *fix.TestChangeRequested)
	}

	changed, err := c.repo.DiffNamesSince(ctx, c.job.WorktreePath, entryHead)
	if err != nil {
		return "", err
	}
	if len(changed) == 0 {
		return "", escalate("fix attempt %d produced no changes and no test_change_requested", attempt)
	}

	// Test-file policy (SPEC §13.1): mechanical, orchestrator-side.
	globs, err := guard.CompileGlobs(c.e.cfg.Policies.TestFileGlobs)
	if err != nil {
		return "", err
	}
	if viol := guard.CheckFixDiff(changed, classification, c.e.cfg.Policies.ProtectTestsOnCodeBugFix, globs); len(viol) > 0 {
		_ = c.repo.ResetHardClean(ctx, c.job.WorktreePath, entryHead)
		// The fix agent's own checkpoints went with the reset. Move the record
		// back with the branch: leaving it on a commit the branch no longer
		// contains makes the next reconcile classify the job as "behind" and
		// report a divergence that is really just this discard.
		c.job.HeadSHA = entryHead
		_ = c.e.st.UpdateJob(c.job)
		return "", escalate("fix for code_bug touched test files %v — change discarded, human review required", viol)
	}

	if err := c.commit(ctx, SFixing, fix.Summary); err != nil {
		return "", err
	}
	c.job.Counters.FixSource = ""
	c.job.Counters.HumanRejectReason = ""
	return SBuilding, nil
}

// --- FINAL_REVIEW --------------------------------------------------------

func (c *jobCtx) handleFinalReview(ctx context.Context) (string, error) {
	if err := c.resetExchange(); err != nil {
		return "", err
	}
	c.stageArtifact("spec.json")
	c.stageArtifact("plan.json")
	c.stageArtifact("implementation.json")
	if err := c.stageDiffPatch(ctx); err != nil {
		return "", err
	}
	pctx := c.baseCtx()
	reviewPath := filepath.Join(c.sdlcDir(), "review.json")
	err := c.runAgent(ctx, SFinalReview, pctx, func() error {
		if _, err := artifact.LoadReview(reviewPath); err != nil {
			return err
		}
		return c.requireCleanTree(ctx)
	})
	if err != nil {
		return "", err
	}
	if _, err := c.harvest("review.json", "final_review.json"); err != nil {
		return "", err
	}
	rev, err := artifact.LoadReview(filepath.Join(c.artDir(), "final_review.json"))
	if err != nil {
		return "", err
	}
	threshold := c.e.cfg.Policies.FinalReviewBlocksAt
	if _, err := c.verifyFindings(ctx, rev, "final_review.json", threshold); err != nil {
		return "", err
	}
	if blocking := rev.Blocking(threshold); len(blocking) > 0 {
		if c.job.Counters.FixAttempts >= c.e.cfg.Limits.MaxFixAttempts {
			return "", escalate("final review has %d finding(s) at or above %s and fix budget is exhausted",
				len(blocking), threshold)
		}
		c.job.Counters.FixSource = "final_review"
		return SFixing, nil
	}
	// Park for the human: surface a diff stat in the event log, plus any
	// findings that did not meet the blocking threshold — the approver is the
	// only remaining reader, so they must not be dropped here either.
	stat, _ := c.repo.DiffStatSince(ctx, c.job.WorktreePath, c.job.Counters.BaseSHA)
	note := map[string]any{"awaiting_merge_approval": true, "diff_stat": stat, "final_summary": rev.Summary}
	if len(rev.Findings) > 0 {
		note["non_blocking_findings"] = rev.Findings
	}
	c.e.event(c.job, "note", note)
	return SAwaitMerge, nil
}

// --- MERGING -------------------------------------------------------------

func (c *jobCtx) handleMerging(ctx context.Context) (string, error) {
	hasRemote, err := c.repo.HasRemote(ctx)
	if err != nil {
		return "", err
	}
	if hasRemote {
		if err := c.repo.Fetch(ctx); err != nil {
			c.e.event(c.job, "note", map[string]any{"fetch_failed": err.Error()})
		}
	}
	if c.target.Merge.RebaseBeforeMerge {
		if err := c.repo.RebaseOnto(ctx, c.job.WorktreePath, c.target.DefaultBranch); err != nil {
			return "", escalate("rebase onto %s: %v", c.target.DefaultBranch, err)
		}
		sha, _ := c.repo.HeadSHA(ctx, c.job.WorktreePath)
		c.job.HeadSHA = sha
		_ = c.e.st.UpdateJob(c.job)
		if c.target.Merge.VerifyAfterRebase == "build" {
			for _, argv := range c.target.Build.Commands {
				exit, logPath, err := c.runPhase(ctx, "verify-rebase", argv, c.target.Build.Timeout.D(), nil)
				if err != nil {
					return "", err
				}
				if exit != 0 {
					return "", escalate("post-rebase verification build failed; log: %s", logPath)
				}
			}
		}
	}
	cur, err := c.repo.CurrentBranch(ctx, c.target.RepoPath)
	if err != nil {
		return "", err
	}
	if cur != c.target.DefaultBranch {
		if err := c.repo.CheckoutBranch(ctx, c.target.DefaultBranch); err != nil {
			return "", escalate("checkout %s in %s: %v", c.target.DefaultBranch, c.target.RepoPath, err)
		}
	}
	msg := fmt.Sprintf("[sdlc %s] merge: %s", c.job.ID, c.job.IssueTitle)
	if err := c.repo.MergeNoFF(ctx, c.job.Branch, msg); err != nil {
		return "", escalate("merge %s into %s: %v", c.job.Branch, c.target.DefaultBranch, err)
	}
	if c.target.Merge.Push == "auto" && hasRemote {
		if err := c.repo.Push(ctx, c.target.DefaultBranch); err != nil {
			return "", escalate("merged locally but push failed: %v", err)
		}
	}
	c.e.event(c.job, "note", map[string]any{"merged": true, "into": c.target.DefaultBranch, "pushed": c.target.Merge.Push == "auto" && hasRemote})
	return SAwaitRelease, nil
}

// --- RELEASING -----------------------------------------------------------

func (c *jobCtx) handleReleasing(ctx context.Context) (string, error) {
	attempt := c.job.Counters.ReleaseRetries + 1
	if attempt > 1 && c.target.Ship.AutoResumeHalt && len(c.target.Ship.ResumeCommand) > 0 {
		seq := c.seq()
		logPath := filepath.Join(c.logsDir(), fmt.Sprintf("%03d_RELEASING_resume.log", seq))
		_, _ = execx.Run(ctx, execx.Cmd{
			Argv: c.target.Ship.ResumeCommand, Dir: c.target.RepoPath,
			Timeout: 5 * time.Minute, LogPath: logPath,
		})
	}
	seq := c.seq()
	logPath := filepath.Join(c.logsDir(), fmt.Sprintf("%03d_RELEASING_ship.log", seq))
	start := time.Now()
	res, err := execx.Run(ctx, execx.Cmd{
		Argv:    c.target.Ship.Command,
		Dir:     c.target.RepoPath,
		Timeout: c.target.Ship.Timeout.D(),
		LogPath: logPath,
	})
	ev := &store.Event{
		JobID: c.job.ID, State: SReleasing, Kind: "exec_run",
		OutputRef: logPath, DurationMS: time.Since(start).Milliseconds(),
		Detail: jsonStr(map[string]any{"phase": "ship", "argv": c.target.Ship.Command, "attempt": attempt}),
	}
	ev.ExitCode.Valid = true
	ev.ExitCode.Int64 = int64(res.ExitCode)
	_ = c.e.st.AddEvent(ev)
	if err != nil {
		return "", err
	}
	if res.ExitCode == 0 {
		c.e.event(c.job, "note", map[string]any{"shipped": true, "attempt": attempt})
		return SCompleted, nil
	}
	c.job.Counters.ReleaseRetries = attempt
	if attempt > c.e.cfg.Limits.MaxReleaseRetries {
		return "", escalate("ship command failed %d times; last log: %s", attempt, logPath)
	}
	c.job.ResumeAfter = time.Now().Add(c.e.cfg.Limits.ReleaseRetryBackoff.D())
	c.e.event(c.job, "note", map[string]any{"ship_failed": true, "attempt": attempt, "retry_after": c.job.ResumeAfter, "log": logPath})
	return SReleasing, nil
}
