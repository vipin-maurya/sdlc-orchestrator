package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/review"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// decisionRecorded is the sentence every surface prints once a row has landed.
// `sdlc approve` and the review prompt write the same kind of row, so they say
// so with the same words; two wordings would read as two mechanisms.
const decisionRecorded = "%s %s recorded for %s gate; the engine will act on its next tick\n"

// jobRule separates two jobs when `sdlc review` walks all of them. Without it
// the second document's heading reads as a continuation of the first one's
// last section.
const jobRule = "────────────────────────────────────────"

// cmdReview is the dispatch shim. Everything it does lives in runReview, which
// takes its reader, its writer and the terminal answer as arguments: a prompt
// loop bound to os.Stdin can only be exercised behind a pty, and a decision
// path nobody can test is a decision path nobody can trust.
func cmdReview(cfg *config.Config, args []string) int {
	return runReview(cfg, args, os.Stdin, os.Stdout, interactive())
}

// runReview prints what each waiting job is asking of a human and, when tty is
// true and --no-prompt was not given, records the answer. With no job id it
// walks every job that is waiting on somebody, which is the only way to find a
// job that parked overnight without already knowing its id.
func runReview(cfg *config.Config, args []string, in io.Reader, out io.Writer, tty bool) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	diffFlag := fs.Bool("diff", false, "inline the whole patch in the document")
	noPrompt := fs.Bool("no-prompt", false, "print and exit; never prompt for a decision")
	pos, err := parseArgsRange(fs, args, 0, 1, "sdlc review [JOB-ID] [--diff] [--no-prompt]")
	if err != nil {
		return argFail(err)
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()

	var jobs []*store.Job
	if len(pos) == 1 {
		j, err := st.GetJob(pos[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: job not found: %s\n", pos[0])
			return 1
		}
		if review.GateFor(j.State) == "" {
			// The same sentence review.Render produces, so asking about the
			// wrong job reads alike whichever surface answered.
			fmt.Fprintf(os.Stderr, "error: %s is in state %s — nothing is waiting on you\n", j.ID, j.State)
			return 1
		}
		jobs = []*store.Job{j}
	} else {
		all, err := st.ListJobs()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		for _, j := range all {
			if review.GateFor(j.State) != "" {
				jobs = append(jobs, j)
			}
		}
		if len(jobs) == 0 {
			fmt.Fprintln(out, "no job is waiting on you")
			return 0
		}
	}

	// One reader for the whole call: a second one per job would swallow the
	// bytes the first had already buffered, so a fast typist's answer to job
	// two would vanish.
	rv := &reviewer{cfg: cfg, st: st, in: bufio.NewReader(in), out: out}
	prompting := tty && !*noPrompt
	ctx := context.Background()

	for i, j := range jobs {
		if i > 0 {
			fmt.Fprintf(out, "\n%s\n", jobRule)
		}
		t, err := cfg.Target(j.Target)
		if err != nil {
			// One job pointing at a target that has been renamed must not hide
			// the rest of the queue.
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", j.ID, err)
			continue
		}
		gate := review.GateFor(j.State)
		// Only the hold document quotes the event trail; asking for events at
		// the other gates is a query nobody reads.
		var evs []*store.Event
		if gate == review.GateHold {
			evs, _ = st.ListEvents(j.ID)
		}
		doc, err := rv.render(ctx, j, t, evs, *diffFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Fprint(out, doc.Body)
		// The engine writes the document when the job parks; `sdlc review`
		// only renders. A path printed here therefore always names a file that
		// exists, which is what makes it safe to paste into an editor.
		if p := review.DocPath(cfg.Orchestrator.DataDir, j.ID, gate); fileExists(p) {
			fmt.Fprintf(out, "document: %s\n", p)
		}
		if p := review.DiffPath(cfg.Orchestrator.DataDir, j.ID, gate); fileExists(p) {
			fmt.Fprintf(out, "patch: %s\n", p)
		}
		if !prompting {
			continue
		}
		var goOn bool
		if gate == review.GateHold {
			goOn = rv.promptHold(ctx, j, t, evs)
		} else {
			goOn = rv.promptGate(ctx, j, t, gate)
		}
		if !goOn {
			return 0
		}
	}
	return 0
}

// reviewer is what both prompt loops need to answer `d` without re-deriving
// the target and re-reading the event trail for every re-render.
type reviewer struct {
	cfg *config.Config
	st  *store.Store
	in  *bufio.Reader
	out io.Writer
}

func (rv *reviewer) render(ctx context.Context, j *store.Job, t config.Target, evs []*store.Event, full bool) (*review.Doc, error) {
	// Gate is left empty on purpose: Render derives it from the job state, so
	// there is one state-to-gate mapping in the codebase rather than two.
	return review.Render(ctx, review.Options{
		Job:           j,
		DataDir:       rv.cfg.Orchestrator.DataDir,
		RepoPath:      t.RepoPath,
		DefaultBranch: t.DefaultBranch,
		ShipCommand:   t.Ship.Command,
		FullDiff:      full,
		Events:        evs,
	})
}

// promptGate asks for the decision on an approval gate. It reports whether to
// carry on to the next job: quit and end-of-input both stop the walk, and
// neither writes anything.
func (rv *reviewer) promptGate(ctx context.Context, j *store.Job, t config.Target, gate string) bool {
	// A second row would race the first one the engine has not consumed yet,
	// and the engine acts on the oldest — so the answer typed here could be
	// silently discarded. Say what is already pending instead.
	if a, err := rv.st.PendingApproval(j.ID, gate); err == nil && a != nil {
		fmt.Fprintf(rv.out, "a %s decision is already pending for %s; the engine will act on its next tick\n", gate, j.ID)
		return true
	}
	for {
		fmt.Fprintf(rv.out, "[%s · %s] approve / reject / diff / skip / quit  [a/r/d/s/q]: ", j.ID, gate)
		ans, ok := rv.readLine()
		if !ok {
			return false
		}
		switch answerKey(ans) {
		case "":
			continue
		case "a":
			fmt.Fprint(rv.out, "note (optional): ")
			note, ok := rv.readLine()
			if !ok {
				return false
			}
			return rv.record(j, &store.Approval{JobID: j.ID, Gate: gate, Decision: "approve", Reason: note})
		case "r":
			// Interactive reject never cancels: --cancel deletes the branch and
			// the worktree, and one keystroke is the wrong affordance for that.
			reason, ok := rv.rejectReason()
			if !ok {
				return false
			}
			return rv.record(j, &store.Approval{JobID: j.ID, Gate: gate, Decision: "reject", Reason: reason, Cancel: false})
		case "d":
			rv.showFullDiff(ctx, j, t, nil)
		case "s":
			return true
		case "q":
			return false
		default:
			fmt.Fprintf(rv.out, "unrecognised answer %q\n", ans)
		}
	}
}

// promptHold asks the question a held job actually poses. A hold is cleared
// with resume or cancel, not approve/reject, and the letters deliberately do
// not overlap with promptGate's: muscle memory from one gate must not record a
// decision at the other.
func (rv *reviewer) promptHold(ctx context.Context, j *store.Job, t config.Target, evs []*store.Event) bool {
	for {
		fmt.Fprintf(rv.out, "[%s · hold] resume / cancel / diff / skip / quit  [r/c/d/s/q]: ", j.ID)
		ans, ok := rv.readLine()
		if !ok {
			return false
		}
		switch answerKey(ans) {
		case "":
			continue
		case "r":
			fmt.Fprint(rv.out, "note for the agent (optional): ")
			note, ok := rv.readLine()
			if !ok {
				return false
			}
			// Reason stays empty: handleControls reads it as the --to target,
			// and empty means "the state the job stopped in". The note rides
			// its own field to the agent that runs next.
			return rv.record(j, &store.Approval{JobID: j.ID, Gate: "resume", Decision: "resume", Reason: "", Note: note})
		case "c":
			fmt.Fprintf(rv.out, "cancel %s and delete its worktree? [y/N]: ", j.ID)
			confirm, ok := rv.readLine()
			if !ok {
				return false
			}
			if answerKey(confirm) != "y" {
				continue
			}
			return rv.record(j, &store.Approval{JobID: j.ID, Gate: "cancel", Decision: "cancel"})
		case "d":
			rv.showFullDiff(ctx, j, t, evs)
		case "s":
			return true
		case "q":
			return false
		default:
			fmt.Fprintf(rv.out, "unrecognised answer %q\n", ans)
		}
	}
}

// rejectReason keeps asking until there is one. The reason is handed to the
// next agent verbatim, so an empty rejection tells it only that somebody said
// no — which is the one thing it cannot act on.
func (rv *reviewer) rejectReason() (string, bool) {
	for {
		fmt.Fprint(rv.out, "reason (required): ")
		reason, ok := rv.readLine()
		if !ok {
			return "", false
		}
		if reason != "" {
			return reason, true
		}
		fmt.Fprintln(rv.out, "a reason is required — the agent is handed it verbatim")
	}
}

// record writes the row and confirms it. It always reports "carry on": the
// job has been decided, so the walk moves to the next one.
func (rv *reviewer) record(j *store.Job, a *store.Approval) bool {
	if err := recordDecision(rv.st, a); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return true
	}
	fmt.Fprintf(rv.out, decisionRecorded, j.ID, a.Decision, a.Gate)
	return true
}

// showFullDiff re-renders the same job with the patch inlined. A render that
// fails here must not end the walk: the operator still has the document they
// were shown and can still answer.
func (rv *reviewer) showFullDiff(ctx context.Context, j *store.Job, t config.Target, evs []*store.Event) {
	doc, err := rv.render(ctx, j, t, evs, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return
	}
	fmt.Fprint(rv.out, doc.Body)
}

// readLine returns the next answer, and false at end of input. Ctrl-D is a
// quit rather than an error: the operator who closed stdin has not decided
// anything, and the job must be left exactly as it was found.
func (rv *reviewer) readLine() (string, bool) {
	line, err := rv.in.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return strings.TrimSpace(line), true
}

// answerKey reduces an answer to the letter that decides it, so "approve",
// "A" and "a" are one answer. Empty stays empty and re-prompts.
func answerKey(ans string) string {
	ans = strings.ToLower(strings.TrimSpace(ans))
	if ans == "" {
		return ""
	}
	return string([]rune(ans)[0])
}

// interactive reports whether stdin is a terminal. os.Stdin.Stat() is the
// stdlib answer — a pipe or a redirected file reports a named pipe or a
// regular file, a terminal reports a character device. Deciding whether it may
// ask a question is not worth a third-party terminal package: the repo has no
// such dependency and must not gain one for this. Under `go test`, cron, CI or
// `sdlc review JOB-1 | less` the answer is false and the command prints and
// exits without touching the database.
func interactive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// /dev/null is a character device too, and it is exactly what a service
	// manager, a cron entry or a `< /dev/null` invocation hands a process with
	// no console. Reading the mode bits alone calls that a terminal, so the
	// fan-out would print one document, read EOF, treat it as "quit" and stop
	// — silently reviewing one job out of five.
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, null) {
		return false
	}
	return true
}

// fileExists is how the CLI decides whether to name a path. Printing the path
// of a document the engine never wrote would send the operator to open
// nothing, so a path is only ever offered once it is on disk.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
