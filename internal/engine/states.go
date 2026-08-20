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
	SAwaitMerge   = "AWAITING_MERGE_APPROVAL"
	SMerging      = "MERGING"
	SAwaitRelease = "AWAITING_RELEASE_APPROVAL"
	SReleasing    = "RELEASING"

	SCompleted     = "COMPLETED"
	SCancelled     = "CANCELLED"
	SFailed        = "FAILED"
	SEscalated     = "ESCALATED"
	STimedOut      = "TIMED_OUT"
	SBlockedQuota  = "BLOCKED_ON_QUOTA"
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
	case SAwaitMerge, SAwaitRelease:
		return true
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
