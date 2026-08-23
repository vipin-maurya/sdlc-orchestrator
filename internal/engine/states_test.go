package engine

import (
	"testing"

	"github.com/vipinm/sdlc-orchestrator/internal/review"
)

func TestScopingStateClassification(t *testing.T) {
	if !isAgentState(SScoping) {
		t.Error("SCOPING is not an agent state")
	}
	if !isParked(SAwaitScope) {
		t.Error("AWAITING_SCOPE_APPROVAL is not parked")
	}
	if isTerminal(SScoping) || isHeld(SScoping) {
		t.Error("SCOPING must be neither terminal nor held")
	}
	if producesCode(SScoping) {
		t.Error("SCOPING must not be checkpointed as code-producing")
	}
	if !needsWorktreeReconcile(SScoping) {
		t.Error("a partial SCOPING run must be discarded and re-run")
	}
	if !IsResumableState(SScoping) {
		t.Error("SCOPING must be a valid `sdlc resume --to` target")
	}
	// Pipeline order: resume --to lists states in the order they run.
	if got := ResumableStates(); got[0] != SScoping {
		t.Errorf("resumable[0]=%s, want SCOPING first", got[0])
	}
	if review.GateFor(SAwaitScope) != review.GateScope {
		t.Errorf("GateFor(%s)=%q, want %q", SAwaitScope, review.GateFor(SAwaitScope), review.GateScope)
	}
}
