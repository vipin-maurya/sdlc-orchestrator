package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Everything here exists to answer one question an unattended run could not
// answer: is the state that has been running for eleven minutes working, or
// hung? The engine used to log only at transitions, and an agent's log file
// only materialised once the agent exited, so a long state and a dead one
// looked exactly alike — and the operator's only recourse was to wait out the
// timeout.
//
// Three things fix that, in increasing order of how much the backend has to
// cooperate:
//
//   - the agent log is written as output arrives (internal/execx), so
//     `sdlc logs JOB --last` tails a state that is still running;
//   - a heartbeat says "still here, N minutes in, last action X" on a timer,
//     which works even for a backend that prints nothing until it exits;
//   - condensed per-action lines, when the backend streams its actions.
//
// The same watcher also turns the agent's own step reports into commits, so a
// long implementation is no longer all-or-nothing.

// progressEventInterval throttles how often live agent chatter becomes a
// stored progress event. The console gets every line; the event log gets a
// checkpoint often enough for `sdlc status` to be current, and rarely enough
// that a chatty agent does not write thousands of rows.
const progressEventInterval = 15 * time.Second

// checkpointPollInterval is how often the worktree is checked for a newly
// completed plan step. Steps take minutes; this only bounds how long a
// finished one stays uncommitted. A var so tests can shrink it.
var checkpointPollInterval = 5 * time.Second

// stepReport is one line of .sdlc/progress.jsonl, appended by the agent as it
// finishes each plan step. Unknown fields are ignored — the file is a
// convenience for the orchestrator, never a contract the state fails on.
type stepReport struct {
	Step    string `json:"step"`
	Summary string `json:"summary"`
}

// watcher observes one in-flight agent run.
type watcher struct {
	c     *jobCtx
	state string
	start time.Time

	mu        sync.Mutex
	last      string // most recent condensed action
	actions   int
	lastEvent time.Time

	stop chan struct{}
	done sync.WaitGroup
}

func (c *jobCtx) newWatcher(state string) *watcher {
	return &watcher{c: c, state: state, start: time.Now(), stop: make(chan struct{})}
}

// onProgress records one condensed action line from the backend.
func (w *watcher) onProgress(line string) {
	w.mu.Lock()
	w.last = line
	w.actions++
	n := w.actions
	emit := time.Since(w.lastEvent) >= progressEventInterval
	if emit {
		w.lastEvent = time.Now()
	}
	w.mu.Unlock()

	if w.c.e.cfg.Get().Orchestrator.StreamOutput {
		w.c.e.logger.Printf("%s: %s [%s] %s", w.c.job.ID, w.state, elapsed(w.start), line)
	}
	if emit {
		w.event(map[string]any{
			"state": w.state, "elapsed_s": int(time.Since(w.start).Seconds()),
			"actions": n, "last": line,
		})
	}
}

// run starts the heartbeat and, when the agent reports completed plan steps,
// the checkpoint committer. Call stopWait when the agent returns.
func (w *watcher) run(ctx context.Context, checkpoint bool) {
	if hb := w.c.e.cfg.Get().Orchestrator.HeartbeatInterval.D(); hb > 0 {
		w.done.Add(1)
		go w.heartbeat(ctx, hb)
	}
	if checkpoint {
		w.done.Add(1)
		go w.checkpointLoop(ctx)
	}
}

func (w *watcher) stopWait() {
	close(w.stop)
	w.done.Wait()
}

func (w *watcher) heartbeat(ctx context.Context, every time.Duration) {
	defer w.done.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			w.mu.Lock()
			last, n := w.last, w.actions
			w.lastEvent = time.Now()
			w.mu.Unlock()
			detail := map[string]any{
				"state": w.state, "elapsed_s": int(time.Since(w.start).Seconds()), "actions": n,
			}
			shown := last
			if shown == "" {
				// A backend that batches its output tells us nothing about what
				// it is doing. Saying so is still worth more than silence: it
				// distinguishes a process that is alive from one that is gone.
				shown = "(no output from this backend until it exits)"
			} else {
				detail["last"] = last
			}
			w.event(detail)
			w.c.e.logger.Printf("%s: %s still running (%s, %d action(s)) %s",
				w.c.job.ID, w.state, elapsed(w.start), n, shown)
		}
	}
}

// checkpointLoop commits each plan step the agent reports finishing.
//
// Without it a state is one commit: a 13-minute run over 22 files that fails
// its post-condition, or is interrupted, is re-done from nothing. With it the
// unit of loss is one step. The agent opts in simply by appending to
// .sdlc/progress.jsonl — a backend that never writes the file loses nothing
// and gains nothing, which is why this needs no configuration.
func (w *watcher) checkpointLoop(ctx context.Context) {
	defer w.done.Done()
	path := filepath.Join(w.c.sdlcDir(), "progress.jsonl")
	// A retry re-runs the agent against the same exchange directory, so lines
	// the previous attempt wrote are already committed. Start after them:
	// re-reading them would label new work with an old step's name.
	seen := countCompleteLines(path)
	t := time.NewTicker(checkpointPollInterval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			// One last pass: the final step is usually reported moments before
			// the agent exits, and it should be its own commit like the rest.
			w.drainSteps(ctx, path, &seen)
			return
		case <-ctx.Done():
			return
		case <-t.C:
			w.drainSteps(ctx, path, &seen)
		}
	}
}

// countCompleteLines returns how many newline-terminated lines path holds
// (0 when it does not exist).
func countCompleteLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

func (w *watcher) drainSteps(ctx context.Context, path string, seen *int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	// Only newline-terminated lines are complete. The trailing fragment is a
	// line the agent is still writing, and counting it as consumed would skip
	// it once the newline arrives.
	lines := strings.Split(string(data), "\n")
	complete := lines[:len(lines)-1]
	for i := *seen; i < len(complete); i++ {
		*seen = i + 1
		ln := strings.TrimSpace(complete[i])
		if ln == "" {
			continue
		}
		var r stepReport
		if err := json.Unmarshal([]byte(ln), &r); err != nil || strings.TrimSpace(r.Step) == "" {
			continue
		}
		w.checkpoint(ctx, r)
	}
}

func (w *watcher) checkpoint(ctx context.Context, r stepReport) {
	label := r.Step
	if r.Summary != "" {
		label += ": " + truncateOneLine(r.Summary, 100)
	}
	// The job row is shared with the goroutine blocked in the agent run, and
	// with the verify fan-out; commit() writes HeadSHA.
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	dirty, err := w.c.repo.IsDirty(ctx, w.c.job.WorktreePath)
	if err != nil || !dirty {
		return
	}
	msg := fmt.Sprintf("[sdlc %s] %s checkpoint %s", w.c.job.ID, w.state, label)
	sha, err := w.c.repo.CommitAll(ctx, w.c.job.WorktreePath, msg)
	if err != nil {
		// Most likely an index.lock held by a git command the agent itself is
		// running. The next step's checkpoint picks the work up.
		w.c.e.logger.Printf("%s: checkpoint of step %s deferred: %v", w.c.job.ID, r.Step, err)
		return
	}
	w.c.job.HeadSHA = sha
	_ = w.c.e.st.UpdateJob(w.c.job)
	w.c.e.event(w.c.job, "progress", map[string]any{
		"checkpoint": r.Step, "state": w.state, "commit": sha, "summary": r.Summary,
	})
	w.c.e.logger.Printf("%s: %s checkpointed step %s as %s", w.c.job.ID, w.state, r.Step, short(sha))
}

// event records a progress row. Verifiers run this concurrently, so the job
// row is taken under the same lock every other shared write uses.
func (w *watcher) event(detail map[string]any) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	w.c.e.event(w.c.job, "progress", detail)
}

func elapsed(start time.Time) string {
	return time.Since(start).Round(time.Second).String()
}
