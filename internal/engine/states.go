package engine

import "github.com/vipinm/sdlc-orchestrator/internal/config"

// Full state enum (SPEC §3.1). Agent-state names are shared with the config
// package so the states: mapping uses the same strings.
const (
	SCreated      = "CREATED"
	SPlanning     = config.StPlanning
	SDesignReview = config.StDesignReview
	SImplementing = config.StImplementing
	SCodeReview   = config.StCodeReview
	SBuilding     = "BUILDING"
	STesting      = "TESTING"
	SFlakeCheck   = "FLAKE_CHECK"
	SAnalyzing    = config.StAnalyzing
	SFixing       = config.StFixing
	SFinalReview  = config.StFinalReview
	// SVerifying is a configuration key, not a pipeline state: the verify
	// pass runs inside the three review states, never as a state of its own.
	SVerifying = config.StVerifying
	// The spec and code gates exist only when policies.human_gates asks for
	// them; with the key absent the pipeline never reaches these two states.
	SAwaitSpec    = "AWAITING_SPEC_APPROVAL"
	SAwaitCode    = "AWAITING_CODE_APPROVAL"
	SAwaitMerge   = "AWAITING_MERGE_APPROVAL"
	SMerging      = "MERGING"
	SAwaitRelease = "AWAITING_RELEASE_APPROVAL"
	SReleasing    = "RELEASING"

	SCompleted    = "COMPLETED"
	SCancelled    = "CANCELLED"
	SFailed       = "FAILED"
	SEscalated    = "ESCALATED"
	STimedOut     = "TIMED_OUT"
	SBlockedQuota = "BLOCKED_ON_QUOTA"
)

// Terminal states: nothing will ever run again.
func isTerminal(s string) bool {
	switch s {
	case SCompleted, SCancelled, SFailed:
		return true
	}
	return false
}

// Held states: durable holds a human clears with `sdlc resume` (or cancel).
func isHeld(s string) bool {
	switch s {
	case SEscalated, STimedOut:
		return true
	}
	return false
}

// Parked states: waiting on an approval row, no work to dispatch.
func isParked(s string) bool {
	switch s {
	case SAwaitSpec, SAwaitCode, SAwaitMerge, SAwaitRelease:
		return true
	}
	return false
}

// resumable lists the states `sdlc resume --to` accepts, in pipeline order.
// A held job can only re-enter a state that actually runs work; sending it to
// a terminal, parked or held state would either do nothing or wedge it.
var resumable = []string{
	SPlanning, SDesignReview, SImplementing, SCodeReview,
	SBuilding, STesting, SFlakeCheck, SAnalyzing, SFixing, SFinalReview,
	SMerging, SReleasing,
}

// ResumableStates returns the states a job may be resumed into.
func ResumableStates() []string { return append([]string{}, resumable...) }

// IsResumableState reports whether s is a valid `sdlc resume --to` target.
func IsResumableState(s string) bool {
	for _, r := range resumable {
		if s == r {
			return true
		}
	}
	return false
}

// isAgentState reports whether the state invokes an agent.
func isAgentState(s string) bool {
	for _, a := range config.AgentStates {
		if s == a {
			return true
		}
	}
	return false
}

// producesCode reports whether a state's agent edits the source tree, and so
// whether its progress is worth checkpointing into commits. Reviewers and the
// planner are excluded: they must leave the tree clean, and a commit from one
// of them would be a policy violation rather than a checkpoint.
func producesCode(s string) bool {
	return s == SImplementing || s == SFixing
}

// needsWorktreeReconcile lists active states whose crash-resume semantics are
// "discard partial work and re-run" (SPEC §7).
func needsWorktreeReconcile(s string) bool {
	if isAgentState(s) {
		return true
	}
	switch s {
	case SBuilding, STesting, SFlakeCheck, SMerging:
		return true
	}
	return false
}
