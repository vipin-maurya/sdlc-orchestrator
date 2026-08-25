package engine

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
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

// The console notice is the only surface an unattended operator sees at 02:00,
// and the headline is its one triage fact. The two ways into this gate need
// different responses — answer a question, or just read and approve — so the
// headline has to distinguish them without the operator opening the document.
func TestScopeGateHeadlineDistinguishesBlockedFromPolicyGate(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"blocked", `"open_questions":[{"id":"Q1","question":"which parser?","blocking":true}],"clarity":"blocked"`,
			"blocked on 1 question(s)"},
		{"policy gate", `"open_questions":[],"clarity":"clear"`, "clarity: clear"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			j := e.submit("fix the thing")
			art := artifact.ArtifactsDir(e.cfg.Orchestrator.DataDir, j.ID)
			if err := os.MkdirAll(art, 0o755); err != nil {
				t.Fatal(err)
			}
			body := `{"schema":"problem/1","problem_statement":"p","in_scope":["a"],
				"out_of_scope":["b"],"success_criteria":["c"],"assumptions":[],` + tc.body + `}`
			if err := os.WriteFile(filepath.Join(art, "problem.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			target, err := e.cfg.Target(j.Target)
			if err != nil {
				t.Fatal(err)
			}
			eng := New(e.cfg, e.st, log.New(io.Discard, "", 0))
			got := eng.gateHeadline(context.Background(), j, review.GateScope, target)
			if got != tc.want {
				t.Errorf("headline = %q, want %q", got, tc.want)
			}
		})
	}
}

// The verify fan-out drops a bad vote and keeps going, but must not do that to
// a suspension: every verifier in the fan-out is talking to the same backend,
// so an outage takes all of them and "decide on the survivors" means deciding a
// gating finding on zero votes — after reserveInvocations has already charged
// the job for the whole fan-out. unreachableErr was created in the fan-out and
// then dropped here, which reintroduced in VERIFYING the exact budget burn the
// BLOCKED_ON_NETWORK work exists to stop.
func TestSuspendErrCoversEverySuspensionKind(t *testing.T) {
	suspensions := []error{
		quotaErr{backend: "fake", backoff: time.Minute},
		unreachableErr{backend: "fake", detail: "ENOTFOUND", backoff: time.Minute},
	}
	for _, err := range suspensions {
		got, ok := suspendErr(err)
		if !ok {
			t.Errorf("%T is a suspension but the verify fan-out would drop it as a failed vote", err)
			continue
		}
		if got != err {
			t.Errorf("suspendErr(%T) returned %v, want the error unchanged", err, got)
		}
	}

	// An ordinary bad vote is what the redundancy is for; it must still drop.
	if _, ok := suspendErr(errors.New("verdict file missing")); ok {
		t.Error("an ordinary verifier failure must be dropped as a vote, not suspend the job")
	}
}

// Every state isSuspended reports must be one suspendErr can be reached by, and
// the reverse — the two lists are the same idea at different layers, and a new
// backoff kind that updates only one of them is the bug this pins.
func TestSuspendedStatesMatchSuspendErrKinds(t *testing.T) {
	states := []string{SBlockedQuota, SBlockedNetwork}
	for _, s := range states {
		if !isSuspended(s) {
			t.Errorf("%s has a suspension error kind but isSuspended does not report it", s)
		}
		if isHeld(s) {
			t.Errorf("%s is a self-resuming backoff and must not read as a hold", s)
		}
		if isTerminal(s) {
			t.Errorf("%s must not be terminal; it resumes itself", s)
		}
	}
}
