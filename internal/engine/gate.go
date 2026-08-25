package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/gitx"
	"github.com/vipinm/sdlc-orchestrator/internal/review"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// gateNotice is what the engine remembers about one open gate. In memory only:
// a restart must re-announce, because the operator who restarted the engine is
// looking at a fresh console and has no way to scroll back to a notice printed
// by the previous process.
type gateNotice struct {
	gate     string    // review.Gate* value
	state    string    // the job state that produced it
	title    string    // the document's headline, cached from the first render
	docPath  string    // "" when rendering failed
	headline string    // "12 files changed, 340 insertions(+)" / "plan: 7 step(s)"
	last     time.Time // when the notice was last printed
}

// pendingGatesFor names the approval gates whose rows would move this job on
// the next tick. A notice is suppressed while one of them is pending: the
// decision has already been made and re-announcing it as "waiting for you"
// would ask the operator to decide something they just decided.
func pendingGatesFor(gate string) []string {
	if gate == review.GateHold {
		// A held job is cleared with resume or cancel, never approve/reject.
		return []string{"resume", "cancel"}
	}
	return []string{gate, "cancel"}
}

// announceGate prints — and, on the first announcement, renders — the notice
// for a job that is waiting on a human. Without it a job that parks at 02:00
// produces one transition line and then nothing, which from the console is
// indistinguishable from an engine that has wedged.
//
// e.mu guards only the notice map. It is never held across review.Write or the
// git call in gateHeadline: both shell out under their own timeouts, and tick
// is a serial loop, so holding the lock there would stall dispatch for every
// other job in the run.
func (e *Engine) announceGate(ctx context.Context, j *store.Job, gate string) {
	e.mu.Lock()
	n, seen := e.announced[j.ID]
	e.mu.Unlock()
	if seen && (n.gate != gate || n.state != j.State) {
		// The job moved from one gate to another. That is a new question, so
		// it is announced afresh rather than treated as a repeat of the old.
		seen, n = false, gateNotice{}
	}
	interval := e.cfg.Orchestrator.GateReminderInterval.D()
	if seen && (interval == 0 || time.Since(n.last) < interval) {
		return
	}
	for _, g := range pendingGatesFor(gate) {
		a, err := e.st.PendingApproval(j.ID, g)
		if err != nil {
			// An announcement the operator does not need is cheaper than
			// silence they cannot interpret, so a store error announces.
			e.logger.Printf("%s: check pending %s decision: %v", j.ID, g, err)
			continue
		}
		if a != nil {
			// Deliberately leaves n.last alone: this tick printed nothing, so
			// it must not count as the reminder for this interval.
			return
		}
	}

	if !seen {
		n.gate, n.state = gate, j.State
		n.title = "a decision is needed"
		// An unknown target must not swallow the announcement: the operator
		// still needs to know the job is stopped, even if the config that
		// would let us render the document has gone missing.
		if t, err := e.cfg.Target(j.Target); err != nil {
			e.logger.Printf("%s: gate document: %v", j.ID, err)
		} else {
			var evs []*store.Event
			if gate == review.GateHold {
				// The hold document is the only one that quotes the event
				// trail; loading it for the others would be pure cost.
				evs, _ = e.st.ListEvents(j.ID)
			}
			doc, path, err := review.Write(ctx, review.Options{
				Job:           j,
				Gate:          gate,
				DataDir:       e.cfg.Orchestrator.DataDir,
				RepoPath:      t.RepoPath,
				DefaultBranch: t.DefaultBranch,
				ShipCommand:   t.Ship.Command,
				FullDiff:      false,
				Events:        evs,
			})
			if err != nil {
				e.logger.Printf("%s: render %s gate document: %v", j.ID, gate, err)
			} else {
				n.docPath = path
				if doc.Title != "" {
					n.title = doc.Title
				}
			}
			n.headline = e.gateHeadline(ctx, j, gate, t)
		}
	}

	e.printGateNotice(j, n, seen)
	n.last = time.Now()
	e.mu.Lock()
	e.announced[j.ID] = n
	e.mu.Unlock()
}

// printGateNotice writes the notice in the house style: every line begins with
// the job id, so a notice interleaved with other jobs' progress lines is still
// attributable to one job.
func (e *Engine) printGateNotice(j *store.Job, n gateNotice, reminder bool) {
	if reminder {
		e.logger.Printf("%s: STILL WAITING FOR YOU (waiting %s) — %s gate: %s",
			j.ID, elapsed(j.StateEnteredAt), n.gate, n.title)
	} else {
		e.logger.Printf("%s: WAITING FOR YOU — %s gate: %s", j.ID, n.gate, n.title)
	}
	e.logger.Printf("%s:   %s", j.ID, truncateOneLine(j.IssueTitle, 80))
	if n.headline != "" {
		e.logger.Printf("%s:   waiting %s   %s", j.ID, elapsed(j.StateEnteredAt), n.headline)
	} else {
		e.logger.Printf("%s:   waiting %s", j.ID, elapsed(j.StateEnteredAt))
	}
	if n.docPath != "" {
		e.logger.Printf("%s:   review: %s", j.ID, n.docPath)
	}
	if n.gate == review.GateHold {
		e.logger.Printf("%s:   sdlc review %s   |   sdlc resume %s [--to STATE]   |   sdlc cancel %s",
			j.ID, j.ID, j.ID, j.ID)
	} else {
		e.logger.Printf("%s:   sdlc review %s   |   sdlc approve %s   |   sdlc reject %s --reason \"...\"",
			j.ID, j.ID, j.ID, j.ID)
	}
}

// gateHeadline is one cheap fact about the thing being decided, so the
// operator can triage the console line without opening the document. It runs
// once per gate entry — reminders re-print what it returned — because the git
// call below is a process spawn and tick walks every job in series.
func (e *Engine) gateHeadline(ctx context.Context, j *store.Job, gate string, t config.Target) string {
	switch gate {
	case review.GateScope:
		p, err := artifact.LoadProblem(filepath.Join(artifact.ArtifactsDir(e.cfg.Orchestrator.DataDir, j.ID), "problem.json"))
		if err != nil {
			return ""
		}
		// The two ways into this gate need different responses, and which one
		// it is decides whether the operator can skim the document or has to
		// answer something. That is the fact worth the one line.
		if n := len(p.BlockingQuestions()); n > 0 {
			return fmt.Sprintf("blocked on %d question(s)", n)
		}
		return "clarity: " + p.Clarity
	case review.GateSpec:
		p, err := artifact.LoadPlan(filepath.Join(artifact.ArtifactsDir(e.cfg.Orchestrator.DataDir, j.ID), "plan.json"))
		if err != nil {
			return ""
		}
		return fmt.Sprintf("plan: %d step(s)", len(p.Steps))
	case review.GateCode, review.GateMerge:
		if j.Counters.BaseSHA == "" {
			return ""
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, err := gitx.Repo{Root: t.RepoPath}.DiffStatSince(ctx, j.WorktreePath, j.Counters.BaseSHA)
		if err != nil {
			return ""
		}
		return lastNonEmptyLine(out)
	case review.GateRelease:
		return "merged into " + t.DefaultBranch
	case review.GateHold:
		return truncateOneLine(j.HoldReason, 90)
	}
	return ""
}

// lastNonEmptyLine picks the summary line off the end of `git diff --stat`,
// which is the only line of it that fits on a console notice.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
