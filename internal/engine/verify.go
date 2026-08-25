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

	"github.com/vipinm/sdlc-orchestrator/internal/agent"
	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
	"github.com/vipinm/sdlc-orchestrator/internal/prompt"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// The verify pass sits between loading a review artifact and gating on it.
//
// A single reviewer stage has one failure mode it cannot detect in itself: a
// finding that is articulate, names real code, supplies a plausible failure
// scenario, and is wrong. Nothing downstream can tell that apart from a real
// finding — the gate sees a severity, not an argument. JOB-1 produced exactly
// one such finding, reasoned from a remembered version of a library API rather
// than the version the build resolved, and it would have sent a correct
// implementation into a rework loop over a bug that did not exist.
//
// So gating findings are put to independent verifiers who are asked to refute
// them. Only findings that could actually stop the pipeline are voted on:
// verifying a nit spends tokens on something that cannot change control flow.

// verifyFindings runs the verify pass over rev in place and returns the
// artifact path holding the verified copy ("" when no pass ran).
//
// rev is mutated: surviving findings keep their severity, refuted ones are
// downgraded and annotated. Callers must gate on rev *after* this returns.
func (c *jobCtx) verifyFindings(ctx context.Context, rev *artifact.Review, reviewName, threshold string) (string, error) {
	votes := c.e.cfg.Limits.VerifyVotes
	if votes <= 0 {
		return "", nil
	}
	// Verdicts are matched back to findings by id, so the ids have to be
	// unique first. Nothing requires an agent to set them, and two findings
	// sharing an id (or both leaving it blank) would share one bucket of
	// verdicts — one finding decided by the other's evidence.
	normaliseFindingIDs(rev)
	gating := rev.Blocking(threshold)
	if len(gating) == 0 {
		// The common case, and it must stay free: no gating finding means
		// nothing to argue about and no agent invoked.
		return "", nil
	}
	minConfirm := c.e.cfg.Limits.VerifyMinConfirm
	if minConfirm < 1 {
		minConfirm = 1
	}

	c.e.event(c.job, "progress", map[string]any{
		"verify_pass": "start", "gating": len(gating), "votes": votes,
		"min_confirm": minConfirm, "review": reviewName,
	})
	c.e.logger.Printf("%s: %s produced %d gating finding(s); putting each to %d verifier(s)",
		c.job.ID, c.job.State, len(gating), votes)

	byFinding := make(map[string][]artifact.Verdict, len(gating))
	for _, f := range gating {
		vs, err := c.runVerifiers(ctx, f, votes)
		if err != nil {
			return "", err
		}
		if len(vs) == 0 {
			// Every verifier failed to produce a usable verdict. Refusing to
			// decide is not the same as refuting: leave the finding gating,
			// which is the conservative direction, and say why.
			c.e.event(c.job, "note", map[string]any{
				"verify_inconclusive": f.ID, "reason": "no verifier produced a valid verdict; finding left gating",
			})
			continue
		}
		byFinding[f.ID] = vs
	}

	surviving, downgraded := artifact.ApplyVerdicts(rev, byFinding, minConfirm)

	verifiedName := strings.TrimSuffix(reviewName, ".json") + ".verified.json"
	verifiedPath := filepath.Join(c.artDir(), verifiedName)
	blob, err := json.MarshalIndent(rev, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(verifiedPath, blob, 0o644); err != nil {
		return "", err
	}

	c.e.event(c.job, "verify_pass", map[string]any{
		"review": reviewName, "verified_artifact": verifiedName,
		"gating": len(gating), "confirmed": len(surviving), "refuted": len(downgraded),
		"votes": votes, "min_confirm": minConfirm,
		"downgraded": findingIDs(downgraded),
	})
	c.e.logger.Printf("%s: verify pass on %s: %d confirmed, %d refuted (of %d gating)",
		c.job.ID, reviewName, len(surviving), len(downgraded), len(gating))
	return verifiedPath, nil
}

// normaliseFindingIDs gives every finding a unique id, minting F<n> for ones
// that are blank or collide. Ids the reviewer chose are kept: they appear in
// hold reasons and in the artifact a human reads.
func normaliseFindingIDs(rev *artifact.Review) {
	seen := make(map[string]bool, len(rev.Findings))
	for i := range rev.Findings {
		id := strings.TrimSpace(rev.Findings[i].ID)
		if id == "" || seen[id] {
			n := i + 1
			for {
				candidate := fmt.Sprintf("F%d", n)
				if !seen[candidate] {
					id = candidate
					break
				}
				n++
			}
		}
		seen[id] = true
		rev.Findings[i].ID = id
	}
}

func findingIDs(fs []artifact.Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

// runVerifiers puts one finding to n independent agents concurrently and
// returns the verdicts that came back valid.
//
// Concurrency is what makes the pass affordable: three sequential ten-minute
// verifiers would cost more wall-clock than the review they are checking. They
// share the worktree, which is safe because each writes to its own path under
// .sdlc/verdicts/ (git-excluded) and none may edit the tree — the caller's
// clean-tree check still runs afterwards and catches one that tries.
//
// Verifiers must not see each other's output. That is the entire value of the
// vote: three agents that can read each other's verdicts produce one verdict
// with extra steps.
func (c *jobCtx) runVerifiers(ctx context.Context, f artifact.Finding, n int) ([]artifact.Verdict, error) {
	agentName, ag, backend, stCfg, err := c.e.cfg.AgentFor(SVerifying)
	if err != nil {
		return nil, fmt.Errorf("verify pass: %w", err)
	}
	// Reserve the whole fan-out against the job budget up front. Spending it
	// one invocation at a time would let a fan-out start and be cut off
	// halfway, which produces a vote count nobody configured.
	if !c.reserveInvocations(n) {
		c.e.event(c.job, "note", map[string]any{
			"verify_skipped": f.ID,
			"reason":         fmt.Sprintf("agent budget would be exceeded by %d verifier(s)", n),
		})
		return nil, nil
	}

	findingJSON, err := json.MarshalIndent([]artifact.Finding{f}, "", "  ")
	if err != nil {
		return nil, err
	}
	verdictDir := filepath.Join(c.sdlcDir(), "verdicts")
	if err := os.MkdirAll(verdictDir, 0o755); err != nil {
		return nil, err
	}
	cfgDir := filepath.Dir(c.e.cfg.Path)
	fid := safeID(f.ID)

	type slot struct {
		v   *artifact.Verdict
		err error
	}
	results := make([]slot, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rel := filepath.ToSlash(filepath.Join(".sdlc", "verdicts", fmt.Sprintf("%s.v%d.json", fid, i+1)))
			outPath := filepath.Join(verdictDir, fmt.Sprintf("%s.v%d.json", fid, i+1))
			_ = os.Remove(outPath)

			pctx := c.baseCtx()
			pctx.Round = i + 1
			pctx.MaxRounds = n
			pctx.PrevFindings = string(findingJSON)
			pctx.FindingID = f.ID
			pctx.OutputPath = rel
			// Reviewed=="diff" states stage a patch; tell the prompt so it
			// mentions the file only when it is actually there.
			if _, statErr := os.Stat(filepath.Join(c.ctxDir(), "diff.patch")); statErr == nil {
				pctx.Classification = "diff"
			}

			text, hash, rerr := prompt.Render(SVerifying, stCfg.Prompt, cfgDir, pctx)
			if rerr != nil {
				results[i] = slot{err: rerr}
				return
			}
			seq := c.seq()
			label := fmt.Sprintf("%03d_VERIFYING_%s_v%d", seq, fid, i+1)
			promptPath := filepath.Join(c.promptsDir(), label+".md")
			_ = os.WriteFile(promptPath, []byte(text), 0o644)
			logPath := filepath.Join(c.logsDir(), label+"_"+agentName+".log")

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

			ev := &store.Event{
				JobID: c.job.ID, State: c.job.State, Kind: "agent_run",
				Agent: agentName, Backend: ag.Backend, Model: ag.Model, Effort: ag.Effort,
				BinaryVersion: c.e.runner.BinaryVersion(backend.Binary),
				PromptHash:    hash,
				InputRef:      promptPath, OutputRef: logPath,
				DurationMS: time.Since(start).Milliseconds(),
				TokensIn:   res.TokensIn, TokensOut: res.TokensOut,
				Detail: jsonStr(map[string]any{
					"verify": true, "finding_id": f.ID, "vote": i + 1, "of": n,
					"timed_out": res.TimedOut, "quota_hit": res.QuotaHit,
				}),
			}
			ev.ExitCode.Valid = true
			ev.ExitCode.Int64 = int64(res.ExitCode)
			c.addEventLocked(ev)

			if runErr != nil {
				results[i] = slot{err: fmt.Errorf("spawn %s: %w", backend.Binary, runErr)}
				return
			}
			if res.Unreachable {
				results[i] = slot{err: unreachableErr{
					backend: ag.Backend,
					detail:  agent.UnreachableDetail(res.Stdout),
					backoff: backend.TransportBackoff.D(),
				}}
				return
			}
			if res.QuotaHit {
				results[i] = slot{err: quotaErr{backend: ag.Backend, backoff: backend.QuotaBackoff.D()}}
				return
			}
			// The verdict file is the post-condition. A verifier that timed
			// out or exited nonzero but still wrote a valid verdict is counted;
			// one that did not is dropped, not retried — the other votes are
			// the redundancy, and retrying inflates the fan-out silently.
			v, lerr := artifact.LoadVerdict(outPath)
			if lerr != nil {
				results[i] = slot{err: lerr}
				return
			}
			if v.FindingID != "" && v.FindingID != f.ID {
				results[i] = slot{err: fmt.Errorf("verdict names finding %q, expected %q", v.FindingID, f.ID)}
				return
			}
			v.FindingID = f.ID
			results[i] = slot{v: v}
		}(i)
	}
	wg.Wait()

	var out []artifact.Verdict
	var dropped []string
	for i, r := range results {
		switch {
		case r.err != nil:
			// A suspension must reach the engine: the job parks and retries the
			// whole fan-out rather than deciding the finding on a partial vote.
			if s, ok := suspendErr(r.err); ok {
				return nil, s
			}
			dropped = append(dropped, fmt.Sprintf("v%d: %v", i+1, r.err))
		case r.v != nil:
			out = append(out, *r.v)
		}
	}
	if len(dropped) > 0 {
		c.e.event(c.job, "note", map[string]any{
			"verify_votes_dropped": map[string]any{
				"finding_id": f.ID, "counted": len(out), "of": n, "reasons": dropped,
			},
		})
		c.e.logger.Printf("%s: finding %s: %d/%d verdicts usable (%s)",
			c.job.ID, f.ID, len(out), n, strings.Join(dropped, "; "))
	}
	return out, nil
}

// reserveInvocations claims n slots from the job's agent budget at once,
// reporting whether the whole fan-out fits. All-or-nothing on purpose: half a
// vote is not a vote.
func (c *jobCtx) reserveInvocations(n int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job.Counters.AgentInvocations+n > c.e.cfg.Limits.MaxAgentInvocationsPerJob {
		return false
	}
	c.job.Counters.AgentInvocations += n
	_ = c.e.st.UpdateJob(c.job)
	return true
}

// addEventLocked records an event from a verifier goroutine. The store
// serializes writes itself, but the job row is shared mutable state.
func (c *jobCtx) addEventLocked(ev *store.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.e.st.AddEvent(ev)
}

// safeID makes a finding id usable as a filename component.
func safeID(id string) string {
	if id == "" {
		return "finding"
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// preferVerified returns the verified copy of a review artifact when the
// verify pass wrote one, and the raw name otherwise. Downstream agents should
// always read the verified copy: it carries the refutations, and a finding
// handed on without the reason it was dismissed invites the next agent to act
// on it again.
func (c *jobCtx) preferVerified(name string) string {
	verified := strings.TrimSuffix(name, ".json") + ".verified.json"
	if _, err := os.Stat(filepath.Join(c.artDir(), verified)); err == nil {
		return verified
	}
	return name
}
