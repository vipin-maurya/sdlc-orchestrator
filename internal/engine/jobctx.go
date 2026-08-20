package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	patch, err := c.repo.DiffPatchSince(ctx, c.job.WorktreePath, c.job.Counters.BaseSHA)
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
			text += fmt.Sprintf("\n\n# IMPORTANT — retry %d of %d\n\nThe previous attempt failed its post-condition check: %v.\nRe-read the output requirements above and comply exactly.\n",
				attempt+1, maxRetries+1, lastErr)
		}
		seq := c.seq()
		promptPath := filepath.Join(c.promptsDir(), fmt.Sprintf("%03d_%s.md", seq, state))
		_ = os.WriteFile(promptPath, []byte(text), 0o644)
		logPath := filepath.Join(c.logsDir(), fmt.Sprintf("%03d_%s_%s.log", seq, state, agentName))

		headBefore, _ := c.repo.HeadSHA(ctx, c.job.WorktreePath)
		start := time.Now()
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
		})
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

// reconcile implements crash-resume for restartable states (SPEC §7): the
// worktree is reset to the sha recorded at state entry and cleaned.
func (c *jobCtx) reconcile(ctx context.Context) error {
	if _, err := os.Stat(c.job.WorktreePath); err != nil {
		return c.repo.EnsureWorktree(ctx, c.job.WorktreePath, c.job.Branch, c.target.DefaultBranch)
	}
	sha := c.job.HeadSHA
	c.e.logger.Printf("%s: reconciling worktree to %s after restart", c.job.ID, sha)
	c.e.event(c.job, "note", map[string]any{"reconcile_to": sha})
	return c.repo.ResetHardClean(ctx, c.job.WorktreePath, sha)
}
