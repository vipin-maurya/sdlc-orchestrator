package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/agent"
	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/execx"
	"github.com/vipinm/sdlc-orchestrator/internal/gitx"
	"github.com/vipinm/sdlc-orchestrator/internal/prompt"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// jobCtx bundles everything a state handler needs for one job step.
type jobCtx struct {
	e      *Engine
	job    *store.Job
	target config.Target
	repo   gitx.Repo
	// mu guards c.job while the verify pass fans out: verifiers run
	// concurrently and each one bumps the invocation counter and writes an
	// event. Every other handler is single-goroutine and never takes it.
	mu sync.Mutex
}

func jsonStr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func (c *jobCtx) dataDir() string    { return c.e.cfg.Orchestrator.DataDir }
func (c *jobCtx) artDir() string     { return artifact.ArtifactsDir(c.dataDir(), c.job.ID) }
func (c *jobCtx) logsDir() string    { return artifact.LogsDir(c.dataDir(), c.job.ID) }
func (c *jobCtx) promptsDir() string { return artifact.PromptsDir(c.dataDir(), c.job.ID) }
func (c *jobCtx) sdlcDir() string    { return filepath.Join(c.job.WorktreePath, ".sdlc") }
func (c *jobCtx) ctxDir() string     { return filepath.Join(c.sdlcDir(), "context") }

// seq returns a monotonically increasing number for log/prompt file names.
func (c *jobCtx) seq() int {
	n, err := c.e.st.CountEvents(c.job.ID)
	if err != nil {
		return int(time.Now().Unix() % 100000)
	}
	return n + 1
}

// resetExchange recreates the .sdlc exchange directory with a fresh context/.
func (c *jobCtx) resetExchange() error {
	if err := os.RemoveAll(c.sdlcDir()); err != nil {
		return err
	}
	return os.MkdirAll(c.ctxDir(), 0o755)
}

// stageArtifact copies artifacts/<name> into .sdlc/context/<name>; missing
// source is ignored (states stage whatever exists).
func (c *jobCtx) stageArtifact(name string) {
	src := filepath.Join(c.artDir(), name)
	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(c.ctxDir(), name), data, 0o644)
}

// stageArtifactAs copies artifacts/<name> into .sdlc/context/<as>.
func (c *jobCtx) stageArtifactAs(name, as string) {
	data, err := os.ReadFile(filepath.Join(c.artDir(), name))
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(c.ctxDir(), as), data, 0o644)
}

// harvest moves .sdlc/<src> into artifacts/<dest> and returns the dest path.
func (c *jobCtx) harvest(src, dest string) (string, error) {
	data, err := os.ReadFile(filepath.Join(c.sdlcDir(), src))
	if err != nil {
		return "", fmt.Errorf("expected agent output .sdlc/%s missing: %w", src, err)
	}
	destPath := filepath.Join(c.artDir(), dest)
	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		return "", err
	}
	return destPath, nil
}

// stageDiffPatch writes the full diff vs the job base into
// .sdlc/context/diff.patch for CODE_REVIEW / FINAL_REVIEW (SPEC §5.0).
func (c *jobCtx) stageDiffPatch(ctx context.Context) error {
	// The truncation flag is dropped here on purpose: the reviewing agent's
	// context window binds long before 4 MiB, and changing what the engine
	// stages for a giant patch is not this change. The gate document, which a
	// human reads and acts on, does say so.
	patch, _, err := c.repo.DiffPatchSince(ctx, c.job.WorktreePath, c.job.Counters.BaseSHA)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.ctxDir(), "diff.patch"), []byte(patch), 0o644)
}

// findingsJSON extracts the findings array from a review artifact as inline
// JSON for prompt embedding ("" when unavailable).
func (c *jobCtx) findingsJSON(name string) string {
	r, err := artifact.LoadReview(filepath.Join(c.artDir(), name))
	if err != nil {
		return ""
	}
	b, err := json.MarshalIndent(r.Findings, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// baseCtx builds the prompt context shared by all states.
func (c *jobCtx) baseCtx() prompt.Ctx {
	return prompt.Ctx{
		JobID:      c.job.ID,
		Branch:     c.job.Branch,
		Target:     c.job.Target,
		IssueTitle: c.job.IssueTitle,
		IssueBody:  c.job.IssueBody,
		BaseSHA:    c.job.Counters.BaseSHA,
	}
}

// excerpt extracts a failure excerpt from a log per limits.* config.
func (c *jobCtx) excerpt(logPath string) string {
	if logPath == "" {
		return "(no log recorded)"
	}
	var pats []*regexp.Regexp
	for _, p := range c.e.cfg.Limits.LogErrorPatterns {
		if re, err := regexp.Compile(p); err == nil {
			pats = append(pats, re)
		}
	}
	return execx.Excerpt(logPath, c.e.cfg.Limits.LogExcerptLines, pats)
}

// normPath canonicalises a path an agent reported so it can be compared with
// git's output: forward slashes, no wrapping quotes, no leading "./" or "/".
func normPath(p string) string {
	p = strings.Trim(strings.TrimSpace(p), `"`)
	p = strings.ReplaceAll(p, `\`, "/")
	p = strings.TrimPrefix(strings.TrimSpace(p), "./")
	return strings.TrimPrefix(p, "/")
}

// reconcileClaims compares the files an agent said it touched against what the
// worktree actually shows. phantom is claimed-but-unmodified (the report is
// wrong, and a self-report nobody checks is worth nothing); unreported is
// modified-but-unclaimed (often a legitimate substitution, but it should be
// visible rather than silent).
func reconcileClaims(claimed, actual []string) (phantom, unreported []string) {
	act := make(map[string]bool, len(actual))
	for _, f := range actual {
		act[normPath(f)] = true
	}
	seen := make(map[string]bool, len(claimed))
	for _, f := range claimed {
		n := normPath(f)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		if !act[n] {
			phantom = append(phantom, n)
		}
	}
	for _, f := range actual {
		if n := normPath(f); n != "" && !seen[n] {
			unreported = append(unreported, n)
		}
	}
	sort.Strings(phantom)
	sort.Strings(unreported)
	return phantom, unreported
}

// exchangeOutputs describes the agent output files already sitting in .sdlc/,
// each annotated with whether it currently parses. A retry needs this: the
// common post-condition failure is a malformed output file next to a complete
// set of correct code changes.
func (c *jobCtx) exchangeOutputs() []string {
	ents, err := os.ReadDir(c.sdlcDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		status := "parses as valid JSON"
		var v any
		if err := artifact.ReadJSON(filepath.Join(c.sdlcDir(), e.Name()), &v); err != nil {
			status = "does NOT parse: " + err.Error()
		}
		out = append(out, fmt.Sprintf("`.sdlc/%s` — %s", e.Name(), status))
	}
	return out
}

// retryNote builds the block appended to a re-rendered prompt when this state
// has already run once in this worktree.
//
// The prompt is regenerated from the template on every attempt, so without
// this the agent receives a first-attempt prompt against a worktree that is
// already half-edited, with no signal that anything ran before — and may redo
// completed work on top of itself. The note states what the previous run
// changed, what it left in .sdlc/, and what is in scope now.
//
// lastErr is nil for the other way a state re-runs against its own output: an
// interrupted run whose checkpoint commits are still on the branch. There is
// no failure to quote there, but the worktree needs describing just the same.
func (c *jobCtx) retryNote(ctx context.Context, attempt, maxRetries int, lastErr error, stateHead string) string {
	var b strings.Builder
	if lastErr != nil {
		fmt.Fprintf(&b, "\n\n# IMPORTANT — retry %d of %d\n\n", attempt+1, maxRetries+1)
		b.WriteString("A previous attempt at this state already ran in this worktree and failed its\npost-condition check:\n\n")
		fmt.Fprintf(&b, "    %v\n\n", lastErr)
	} else {
		b.WriteString("\n\n# IMPORTANT — this state was interrupted and is being re-run\n\n")
		b.WriteString("An earlier run of this state was cut short (the orchestrator restarted, or the\nrun was cancelled). Its completed work was committed and is still on the branch.\n\n")
	}

	b.WriteString("## Worktree state\n\n")
	changed, err := c.repo.DiffNamesSince(ctx, c.job.WorktreePath, stateHead)
	switch {
	case err != nil:
		b.WriteString("The worktree could not be inspected; verify the current contents of any file\nbefore editing it.\n")
	case len(changed) == 0:
		b.WriteString("The previous run left no file changes. The tree is as it was when this\nstate began.\n")
	default:
		fmt.Fprintf(&b, "The previous run already modified %d file(s) since this state began:\n\n", len(changed))
		for _, f := range changed {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\nThose changes are still in place and are NOT reverted. Do not redo them and do\nnot undo them. Read any file before editing it — it may already contain the\nchange you were about to make.\n")
	}

	if outs := c.exchangeOutputs(); len(outs) > 0 {
		b.WriteString("\n## Files already written to `.sdlc/`\n\n")
		for _, o := range outs {
			fmt.Fprintf(&b, "- %s\n", o)
		}
	}

	b.WriteString("\n## What to do now\n\n")
	if lastErr != nil {
		b.WriteString("Fix only the failure quoted above, then write your output file again. If the\n" +
			"failure was in that file's encoding, format, or content, then the code changes\n" +
			"listed above are already done and need no further work — correct the file and\n" +
			"stop. Write it as UTF-8 with no byte-order mark and no surrounding prose.\n")
	} else {
		b.WriteString("Continue from what is already there: finish the work that is not done yet, and\n" +
			"produce this state's output file covering the whole state, including the parts\n" +
			"the earlier run completed. Write it as UTF-8 with no byte-order mark and no\n" +
			"surrounding prose.\n")
	}
	return b.String()
}

// priorWorkNote returns the interrupted-run block when this state's first
// attempt is starting against a tree that already holds its own earlier work,
// and "" in the normal case. Checkpoint commits (SPEC §6.1) make this
// reachable: work that used to be discarded on restart now survives it, and an
// agent handed a first-attempt prompt against a half-finished tree is the
// failure mode the retry note exists to prevent.
func (c *jobCtx) priorWorkNote(ctx context.Context, maxRetries int, stateHead string) string {
	if stateHead == "" {
		return ""
	}
	changed, err := c.repo.DiffNamesSince(ctx, c.job.WorktreePath, stateHead)
	if err != nil || len(changed) == 0 {
		return ""
	}
	return c.retryNote(ctx, 0, maxRetries, nil, stateHead)
}

// runAgent renders the state's prompt, invokes the configured agent with
// post-condition verification and retries, and records events. validate is
// the state's post-condition; it runs after every attempt. Returns quotaErr
// for rate limits and escalate() when retries are exhausted.
func (c *jobCtx) runAgent(ctx context.Context, state string, pctx prompt.Ctx, validate func() error) error {
	agentName, ag, backend, stCfg, err := c.e.cfg.AgentFor(state)
	if err != nil {
		return err
	}
	cfgDir := filepath.Dir(c.e.cfg.Path)
	maxRetries := c.e.cfg.Limits.MaxAgentRetries
	// Head at state entry: the baseline a retry's "what did the last run
	// already do" summary is computed against. Persisted with the job, so it
	// survives a restart that leaves this state's checkpoints on the branch.
	stateHead, err := c.stateBaseline(ctx)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if c.job.Counters.AgentInvocations >= c.e.cfg.Limits.MaxAgentInvocationsPerJob {
			return escalate("agent budget exhausted (%d invocations)", c.job.Counters.AgentInvocations)
		}
		text, hash, err := prompt.Render(state, stCfg.Prompt, cfgDir, pctx)
		if err != nil {
			return err
		}
		if attempt > 0 {
			text += c.retryNote(ctx, attempt, maxRetries, lastErr, stateHead)
		} else {
			text += c.priorWorkNote(ctx, maxRetries, stateHead)
		}
		seq := c.seq()
		promptPath := filepath.Join(c.promptsDir(), fmt.Sprintf("%03d_%s.md", seq, state))
		_ = os.WriteFile(promptPath, []byte(text), 0o644)
		logPath := filepath.Join(c.logsDir(), fmt.Sprintf("%03d_%s_%s.log", seq, state, agentName))

		headBefore, _ := c.repo.HeadSHA(ctx, c.job.WorktreePath)
		start := time.Now()
		// The state is about to be opaque for however long the agent takes.
		// The watcher is what keeps it legible: a heartbeat, the agent's
		// actions as they happen, and — for the states that write code — a
		// commit per plan step the agent reports finishing.
		w := c.newWatcher(state)
		c.e.event(c.job, "progress", map[string]any{
			"state": state, "agent": agentName, "model": ag.Model,
			"attempt": attempt + 1, "of": maxRetries + 1, "started": true,
		})
		c.e.logger.Printf("%s: %s started (agent %s, attempt %d/%d, timeout %s)",
			c.job.ID, state, agentName, attempt+1, maxRetries+1, stCfg.Timeout)
		w.run(ctx, producesCode(state))
		res, runErr := c.e.runner.Run(ctx, agent.Spec{
			BackendName: ag.Backend,
			Backend:     backend,
			Model:       ag.Model,
			Effort:      ag.Effort,
			Prompt:      text,
			Cwd:         c.job.WorktreePath,
			Timeout:     stCfg.Timeout.D(),
			Allowed:     stCfg.AllowedTools,
			Disallowed:  stCfg.DisallowedTools,
			LogPath:     logPath,
			OnProgress:  w.onProgress,
		})
		w.stopWait()
		headAfter, _ := c.repo.HeadSHA(ctx, c.job.WorktreePath)

		c.job.Counters.AgentInvocations++
		c.job.Counters.AgentRetries = attempt
		_ = c.e.st.UpdateJob(c.job)
		ev := &store.Event{
			JobID: c.job.ID, State: state, Kind: "agent_run",
			Agent: agentName, Backend: ag.Backend, Model: ag.Model, Effort: ag.Effort,
			BinaryVersion: c.e.runner.BinaryVersion(backend.Binary),
			PromptHash:    hash,
			InputRef:      promptPath, OutputRef: logPath,
			DurationMS: time.Since(start).Milliseconds(),
			HeadBefore: headBefore, HeadAfter: headAfter,
			TokensIn: res.TokensIn, TokensOut: res.TokensOut,
		}
		ev.ExitCode.Valid = true
		ev.ExitCode.Int64 = int64(res.ExitCode)
		if res.Model != "" {
			ev.Detail = jsonStr(map[string]any{"reported_model": res.Model, "quota_hit": res.QuotaHit, "timed_out": res.TimedOut})
		}
		_ = c.e.st.AddEvent(ev)

		if runErr != nil {
			lastErr = fmt.Errorf("spawn %s: %w", backend.Binary, runErr)
			continue
		}
		if res.QuotaHit {
			return quotaErr{backend: ag.Backend, backoff: backend.QuotaBackoff.D()}
		}
		if res.TimedOut {
			lastErr = fmt.Errorf("agent timed out after %s", stCfg.Timeout)
			continue
		}
		if res.ModelMismatch {
			lastErr = fmt.Errorf("backend ran model %q instead of requested %q (backends.%s.assert_model)", res.Model, ag.Model, ag.Backend)
			continue
		}
		// Exit code is a hint; the post-condition decides (SPEC §6.1).
		if err := validate(); err != nil {
			lastErr = fmt.Errorf("post-condition: %w (agent exit %d)", err, res.ExitCode)
			c.e.logger.Printf("%s: %s attempt %d post-condition failed: %v", c.job.ID, state, attempt+1, err)
			continue
		}
		// Backends exit nonzero for reasons of their own — a harness error
		// after the work is done, a warning promoted to a failure. The
		// artifact is the evidence, so the state proceeds; but the precedence
		// is recorded rather than assumed, so a backend that always exits 1
		// is visible in `sdlc events` instead of invisible.
		if res.ExitCode != 0 {
			c.e.event(c.job, "note", map[string]any{
				"post_condition_overrode_exit": res.ExitCode, "state": state, "agent": agentName,
			})
			c.e.logger.Printf("%s: %s post-condition passed despite agent exit %d; proceeding", c.job.ID, state, res.ExitCode)
		}
		return nil
	}
	return escalate("%s failed after %d attempts: %v", state, maxRetries+1, lastErr)
}

// requireCleanTree enforces reviewer immutability (SPEC §13.2): review and
// analyze states must not modify the tree. Any dirt is discarded and reported.
func (c *jobCtx) requireCleanTree(ctx context.Context) error {
	if !c.e.cfg.Policies.ReviewerDiffMustBeEmpty {
		return nil
	}
	dirty, err := c.repo.IsDirty(ctx, c.job.WorktreePath)
	if err != nil {
		return err
	}
	if dirty {
		files, _ := c.repo.DirtyFiles(ctx, c.job.WorktreePath)
		_ = c.repo.ResetHardClean(ctx, c.job.WorktreePath, "")
		return fmt.Errorf("reviewer modified the tree (%v); changes discarded", files)
	}
	return nil
}

// discardTreeChanges silently resets any stray edits (used after PLANNING).
func (c *jobCtx) discardTreeChanges(ctx context.Context) {
	dirty, err := c.repo.IsDirty(ctx, c.job.WorktreePath)
	if err == nil && dirty {
		files, _ := c.repo.DirtyFiles(ctx, c.job.WorktreePath)
		c.e.event(c.job, "note", map[string]any{"discarded_stray_edits": files})
		_ = c.repo.ResetHardClean(ctx, c.job.WorktreePath, "")
	}
}

// runPhase executes one build/test phase command under the gradle semaphore,
// recording an exec_run event. Returns the exit code and log path.
func (c *jobCtx) runPhase(ctx context.Context, phase string, argv []string, timeout time.Duration, extraEnv map[string]string) (int, string, error) {
	release, err := c.e.res.AcquireGradle(ctx)
	if err != nil {
		return -1, "", err
	}
	defer release()
	seq := c.seq()
	logPath := filepath.Join(c.logsDir(), fmt.Sprintf("%03d_%s_%s.log", seq, c.job.State, phase))
	// A deterministic phase is silent for as long as it takes; without this a
	// running suite and a hung one look identical from outside.
	c.e.event(c.job, "progress", map[string]any{"phase": phase, "argv": argv, "started": true})
	c.e.logger.Printf("%s: %s: %s started (%s)", c.job.ID, c.job.State, phase, strings.Join(argv, " "))
	env := map[string]string{}
	if c.target.Gradle.IsolatedUserHome {
		env["GRADLE_USER_HOME"] = filepath.Join(c.dataDir(), "gradle-homes", c.job.ID)
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	start := time.Now()
	res, err := execx.Run(ctx, execx.Cmd{
		Argv:    argv,
		Dir:     c.job.WorktreePath,
		Env:     env,
		Timeout: timeout,
		LogPath: logPath,
	})
	ev := &store.Event{
		JobID: c.job.ID, State: c.job.State, Kind: "exec_run",
		OutputRef: logPath, DurationMS: time.Since(start).Milliseconds(),
		Detail: jsonStr(map[string]any{"phase": phase, "argv": argv, "timed_out": res.TimedOut}),
	}
	ev.ExitCode.Valid = true
	ev.ExitCode.Int64 = int64(res.ExitCode)
	_ = c.e.st.AddEvent(ev)
	if err != nil {
		return -1, logPath, err
	}
	return res.ExitCode, logPath, nil
}

// stateBaseline returns the commit the current state's work is measured
// against — its diff, its post-conditions, and the test-file guard that can
// roll the branch back to it.
//
// It is read from git, not from job.HeadSHA: that field only ever advances in
// the orchestrator's own commit(), so a human who commits on the job branch to
// answer an escalation is invisible to it. Diffing against a stale counter
// charges their files to the agent and then discards them, which is exactly
// what destroyed a verified fix in JOB-1.
//
// It is then persisted for the life of the state rather than re-read per run.
// A state can be re-entered — after a crash, or after a post-condition retry —
// with its own checkpoint commits already on the branch, and a fresh read at
// that point would fold the state's own work into its own baseline, making
// completed work look like it never happened.
func (c *jobCtx) stateBaseline(ctx context.Context) (string, error) {
	if h := c.job.Counters.StateEntryHead; h != "" {
		return h, nil
	}
	sha, err := c.repo.HeadSHA(ctx, c.job.WorktreePath)
	if err != nil {
		return "", fmt.Errorf("read branch head for %s: %w", c.job.State, err)
	}
	c.job.Counters.StateEntryHead = sha
	c.job.HeadSHA = sha
	_ = c.e.st.UpdateJob(c.job)
	return sha, nil
}

// syncHead adopts a branch that moved forward without the orchestrator, so the
// job record and the branch agree before any handler diffs against it.
// Divergence is not adopted: it is reported and left for reconcile to judge.
func (c *jobCtx) syncHead(ctx context.Context) error {
	if _, err := os.Stat(c.job.WorktreePath); err != nil {
		return nil // not provisioned yet; CREATED will make it
	}
	actual, err := c.repo.HeadSHA(ctx, c.job.WorktreePath)
	if err != nil || actual == "" || actual == c.job.HeadSHA {
		return err
	}
	recorded := c.job.HeadSHA
	c.job.HeadSHA = actual
	if err := c.e.st.UpdateJob(c.job); err != nil {
		return err
	}
	ahead := false
	if recorded != "" {
		ahead, _ = c.repo.IsAncestor(ctx, c.job.WorktreePath, recorded, actual)
	}
	c.e.event(c.job, "note", map[string]any{
		"head_sha_resynced": map[string]any{"from": recorded, "to": actual, "branch_ahead": ahead},
	})
	c.e.logger.Printf("%s: branch %s moved outside the orchestrator (%s -> %s); adopting it",
		c.job.ID, c.job.Branch, short(recorded), short(actual))
	return nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}

// preserveWork commits whatever a failing state left in the worktree. An
// escalated state's output is precisely what the operator has to read to
// decide what to do, and an uncommitted worktree is one reconcile away from
// being erased. Committing it also means the next `sdlc resume` starts from a
// clean tree rather than half-applied edits of unknown provenance.
func (c *jobCtx) preserveWork(ctx context.Context, cause error) {
	if _, err := os.Stat(c.job.WorktreePath); err != nil {
		return
	}
	dirty, err := c.repo.IsDirty(ctx, c.job.WorktreePath)
	if err != nil || !dirty {
		return
	}
	files, _ := c.repo.DirtyFiles(ctx, c.job.WorktreePath)
	msg := fmt.Sprintf("[sdlc %s] %s (incomplete): %s", c.job.ID, c.job.State, truncateOneLine(cause.Error(), 120))
	sha, cerr := c.repo.CommitAll(ctx, c.job.WorktreePath, msg)
	if cerr != nil {
		c.e.logger.Printf("%s: could not preserve %s work: %v", c.job.ID, c.job.State, cerr)
		return
	}
	c.job.HeadSHA = sha
	_ = c.e.st.UpdateJob(c.job)
	c.e.event(c.job, "note", map[string]any{
		"preserved_incomplete_work": map[string]any{"state": c.job.State, "commit": sha, "files": files},
	})
	c.e.logger.Printf("%s: committed %d incomplete file(s) from %s as %s before holding",
		c.job.ID, len(files), c.job.State, short(sha))
}

func truncateOneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "\u2026"
}

// commit commits all worktree changes on the agent's behalf.
func (c *jobCtx) commit(ctx context.Context, state, summary string) error {
	msg := fmt.Sprintf("[sdlc %s] %s: %s", c.job.ID, state, summary)
	sha, err := c.repo.CommitAll(ctx, c.job.WorktreePath, msg)
	if err != nil {
		return err
	}
	c.job.HeadSHA = sha
	return c.e.st.UpdateJob(c.job)
}

// reconcile implements crash-resume for restartable states (SPEC §7):
// partial, uncommitted work from an interrupted state is discarded so the
// state re-runs from a known point.
//
// What it must not do is discard commits. The recorded HeadSHA is only ever
// advanced by the orchestrator's own commit(), so a human who commits on the
// job branch — the normal way to answer an escalation — leaves the record
// behind the branch. A blind `reset --hard <recorded>` then deletes their work
// silently, which is exactly what happened in JOB-1. So the branch is
// classified first, and only the uncommitted layer is ever thrown away.
func (c *jobCtx) reconcile(ctx context.Context) error {
	if _, err := os.Stat(c.job.WorktreePath); err != nil {
		return c.repo.EnsureWorktree(ctx, c.job.WorktreePath, c.job.Branch, c.target.DefaultBranch)
	}
	recorded := c.job.HeadSHA
	actual, err := c.repo.HeadSHA(ctx, c.job.WorktreePath)
	if err != nil {
		return err
	}
	dirty, err := c.repo.IsDirty(ctx, c.job.WorktreePath)
	if err != nil {
		return err
	}

	target := recorded
	switch {
	case recorded == "" || recorded == actual:
		target = actual
	default:
		ahead, aerr := c.repo.IsAncestor(ctx, c.job.WorktreePath, recorded, actual)
		if aerr != nil {
			return fmt.Errorf("classify branch %s against recorded head %s: %w", c.job.Branch, short(recorded), aerr)
		}
		if ahead {
			// Commits landed on the branch that the orchestrator did not make.
			// Adopt them: they are the newest state of the work, and the only
			// alternative is to delete someone's commits without asking.
			files, _ := c.repo.DiffNamesSince(ctx, c.job.WorktreePath, recorded)
			c.e.event(c.job, "note", map[string]any{
				"branch_advanced_outside_orchestrator": map[string]any{
					"from": recorded, "to": actual, "files": files, "adopted": true,
				},
			})
			c.e.logger.Printf("%s: branch %s is %s ahead of the recorded head (%d file(s)); adopting instead of resetting",
				c.job.ID, c.job.Branch, short(actual), len(files))
			c.job.HeadSHA = actual
			if err := c.e.st.UpdateJob(c.job); err != nil {
				return err
			}
			target = actual
		} else {
			behind, _ := c.repo.IsAncestor(ctx, c.job.WorktreePath, actual, recorded)
			if !behind {
				// Neither is an ancestor of the other: history was rewritten
				// under the job. There is no safe automatic answer, and
				// picking one would destroy the other.
				c.e.event(c.job, "note", map[string]any{
					"branch_diverged": map[string]any{"recorded": recorded, "actual": actual},
				})
				return fmt.Errorf("branch %s diverged from the recorded head (recorded %s, branch %s); "+
					"resolve it by hand, then `sdlc resume %s`", c.job.Branch, short(recorded), short(actual), c.job.ID)
			}
			// Branch is behind the record — the recorded commit is gone from
			// this branch. Reset to what is actually there rather than to a
			// sha the branch no longer contains.
			c.e.event(c.job, "note", map[string]any{
				"branch_behind_recorded": map[string]any{"recorded": recorded, "actual": actual},
			})
			c.job.HeadSHA = actual
			if err := c.e.st.UpdateJob(c.job); err != nil {
				return err
			}
			target = actual
		}
	}

	if !dirty && target == actual {
		c.e.logger.Printf("%s: worktree already clean at %s; nothing to reconcile", c.job.ID, short(actual))
		return nil
	}
	files, _ := c.repo.DirtyFiles(ctx, c.job.WorktreePath)
	c.e.logger.Printf("%s: discarding %d uncommitted file(s) and resetting worktree to %s after restart",
		c.job.ID, len(files), short(target))
	c.e.event(c.job, "note", map[string]any{
		"reconcile_to": target, "discarded_uncommitted": files,
	})
	return c.repo.ResetHardClean(ctx, c.job.WorktreePath, target)
}
