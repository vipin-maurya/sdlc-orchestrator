package server

// Shared shaping for the review dashboard (design doc: Review Dashboard).
//
// The dashboard draws a job's position in the pipeline in three places — the
// strip above a gate document, the bar in the job grid, and the colour a state
// name is printed in — and all three have to agree. They agree because they are
// all derived here, from one table, rather than each page deciding for itself
// what "nearly done" looks like.
//
// Nothing here is a second opinion about the state machine. The phase table
// groups the states engine/states.go already defines; a state this file has
// never heard of gets no phase and draws as such, which is the honest answer
// and the one that shows up the moment the engine grows a state.

import "strings"

// phase is one column of the pipeline strip and the set of states that live
// under it. A state parked at a gate sits in the phase the gate belongs to —
// AWAITING_MERGE_APPROVAL is the merge phase, not a tenth phase of its own —
// because the reader is asking how far the work has got, not which of the two
// halves of a phase the engine is in.
type phase struct {
	Label  string
	states []string
}

var phases = []phase{
	{"PLANNING", []string{"CREATED", "SCOPING", "AWAITING_SCOPE_APPROVAL", "PLANNING"}},
	{"DESIGN_REVIEW", []string{"DESIGN_REVIEW", "AWAITING_SPEC_APPROVAL"}},
	{"IMPLEMENTING", []string{"IMPLEMENTING"}},
	{"CODE_REVIEW", []string{"CODE_REVIEW", "AWAITING_CODE_APPROVAL"}},
	{"BUILDING", []string{"BUILDING"}},
	{"TESTING", []string{"TESTING", "FLAKE_CHECK", "ANALYZING", "FIXING"}},
	{"FINAL_REVIEW", []string{"FINAL_REVIEW"}},
	{"MERGE", []string{"AWAITING_MERGE_APPROVAL", "MERGING"}},
	{"RELEASE", []string{"AWAITING_RELEASE_APPROVAL", "RELEASING", "COMPLETED"}},
}

// phaseView is one cell of the strip. Class is from a closed set, never built
// from the state string: everything on these pages that becomes a class
// attribute has to be a name this package chose (render.go, findingView).
type phaseView struct {
	Label string
	Class string // "done" | "now" | "todo"
}

// phaseIndex locates a state in the table, or -1.
//
// A stopped job — failed, cancelled, held, out of quota — is located by the
// state it stopped in, because "where did this get to" is the question the
// strip answers and CANCELLED is not an answer to it. PrevState is the
// engine's own record of that and is not re-derived here.
func phaseIndex(state, prev string) int {
	if i := indexOfState(state); i >= 0 {
		return i
	}
	return indexOfState(prev)
}

func indexOfState(state string) int {
	if state == "" {
		return -1
	}
	for i, p := range phases {
		for _, s := range p.states {
			if s == state {
				return i
			}
		}
	}
	return -1
}

// pipelineFor shapes the whole strip for one job. An unplaceable state gets
// every cell as "todo": a strip that guessed would be worse than one that
// visibly knows nothing.
func pipelineFor(state, prev string) []phaseView {
	at := phaseIndex(state, prev)
	out := make([]phaseView, 0, len(phases))
	for i, p := range phases {
		v := phaseView{Label: p.Label, Class: "todo"}
		switch {
		case at < 0:
		case i < at:
			v.Class = "done"
		case i == at:
			v.Class = "now"
		}
		out = append(out, v)
	}
	return out
}

// phasePct is the width of the job grid's progress bar, 0..100. It counts
// completed phases rather than interpolating inside one: the engine publishes
// no progress within a state, and a bar that crept along on a timer would be
// inventing one.
func phasePct(state, prev string) int {
	if state == "COMPLETED" {
		return 100
	}
	at := phaseIndex(state, prev)
	if at < 0 {
		return 0
	}
	return (at * 100) / len(phases)
}

// stateClass is the colour a state name is printed in. Four outcomes, and the
// default is "run" rather than nothing: a state nobody has classified is still
// a state the engine is working in.
func stateClass(state string) string {
	switch state {
	case "COMPLETED":
		return "ok"
	case "FAILED", "CANCELLED":
		return "bad"
	case "ESCALATED", "TIMED_OUT", "BLOCKED_ON_QUOTA", "BLOCKED_ON_NETWORK":
		return "warn"
	}
	if strings.HasPrefix(state, "AWAITING_") {
		return "warn"
	}
	return "run"
}

// gateClass is the pill colour per gate. Same closed-set rule: review.GateFor
// returns one of five strings and anything else draws neutral.
func gateClass(gate string) string {
	switch gate {
	case "hold":
		return "bad"
	case "merge", "release":
		return "warn"
	case "scope", "spec", "code":
		return "info"
	}
	return ""
}

// The four sets below are this package's single copy of the membership
// questions these pages keep asking: is this job finished, parked at a gate,
// held for a human, or waiting out a backoff timer. They are restated here
// rather than imported because internal/engine keeps the same predicates
// unexported and this file must not become a reason to export them — but they
// are stated *once*, because the four call sites that each carried their own
// literal list are how BLOCKED_ON_QUOTA came to be counted as running.
var (
	// terminalStates: nothing will ever run again.
	terminalStates = []string{"COMPLETED", "CANCELLED", "FAILED"}
	// parkedStates: waiting on an approval row, no work to dispatch.
	parkedStates = []string{
		"AWAITING_SCOPE_APPROVAL", "AWAITING_SPEC_APPROVAL", "AWAITING_CODE_APPROVAL",
		"AWAITING_MERGE_APPROVAL", "AWAITING_RELEASE_APPROVAL",
	}
	// heldStates: durable holds a human clears with `sdlc resume`.
	heldStates = []string{"ESCALATED", "TIMED_OUT"}
	// backoffStates: no gate and no human to wait for, but the engine's
	// dispatch loop skips them while a timer runs down, so there is no work
	// queued for them either.
	backoffStates = []string{"BLOCKED_ON_QUOTA", "BLOCKED_ON_NETWORK"}
)

func inSet(set []string, state string) bool {
	for _, s := range set {
		if s == state {
			return true
		}
	}
	return false
}

// isTerminalState reports whether the job is finished, one way or another.
func isTerminalState(state string) bool { return inSet(terminalStates, state) }

// isBackoffState reports whether the job is sitting out an engine-side timer.
// It draws neither as running nor as waiting on anybody: nobody has to do
// anything, and the engine has not given up.
func isBackoffState(state string) bool { return inSet(backoffStates, state) }

// isRunning reports whether the engine would have work to dispatch for this
// state — which is what the "Running" list under the review queue means. It is
// the complement of the four sets above, and a state none of them names counts
// as running, which is the honest default for a state this file has not met.
func isRunning(state string) bool {
	return !isTerminalState(state) &&
		!inSet(parkedStates, state) &&
		!inSet(heldStates, state) &&
		!isBackoffState(state)
}
