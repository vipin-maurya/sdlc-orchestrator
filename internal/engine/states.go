package engine

import "github.com/vipinm/sdlc-orchestrator/internal/config"

// Full state enum (SPEC §3.1). Agent-state names are shared with the config
// package so the states: mapping uses the same strings.
const (
	SCreated      = "CREATED"
	SScoping      = config.StScoping
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
	// SAwaitScope is where a job waits when the scoping agent raised a blocking
	// question, or when policies.human_gates asks for a scope checkpoint.
	SAwaitScope = "AWAITING_SCOPE_APPROVAL"
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
	// SBlockedNetwork is BLOCKED_ON_QUOTA's sibling: same suspend-and-retry
	// shape, different cause and a much shorter backoff. It is a state of its
	// own rather than a reuse of the quota one because an operator debugging a
	// DNS outage must not be told the job is waiting out a rate limit.
	SBlockedNetwork = "BLOCKED_ON_NETWORK"
)

// isSuspended reports the states that are waiting out a backoff and will
// resume themselves. They are not holds — nobody has to do anything — and they
// spend no retry budget.
func isSuspended(s string) bool {
	switch s {
	case SBlockedQuota, SBlockedNetwork:
		return true
	}
	return false
}

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
	case SAwaitScope, SAwaitSpec, SAwaitCode, SAwaitMerge, SAwaitRelease:
		return true
	}
	return false
}

// resumable lists the states `sdlc resume --to` accepts, in pipeline order.
// A held job can only re-enter a state that actually runs work; sending it to
// a terminal, parked or held state would either do nothing or wedge it.
var resumable = []string{
	SScoping, SPlanning, SDesignReview, SImplementing, SCodeReview,
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

// suspendErr reports the errors that must interrupt a fan-out rather than being
// counted as one failed vote among several.
//
// The distinction is who is at fault. A verifier that wrote no verdict, or an
// unparseable one, is a vote the redundancy exists to absorb: drop it and
// decide on the rest. A quota hit or an unreachable backend is neither a vote
// nor a failure of the work — it will hit every remaining verifier in the same
// fan-out for the same reason, so deciding a finding on the survivors means
// deciding it on however many votes happened to run before the network went
// away. The job suspends and runs the whole fan-out again instead.
//
// Kept here beside isSuspended, and as a type switch rather than two inline
// assertions, because the failure it guards against is a third suspension kind
// being added and one of the call sites not learning about it — which is
// exactly how unreachableErr came to be dropped as a failed vote.
func suspendErr(err error) (error, bool) {
	switch e := err.(type) {
	case quotaErr:
		return e, true
	case unreachableErr:
		return e, true
	}
	return nil, false
}
