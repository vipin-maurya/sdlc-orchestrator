package engine

// Integration tests (SPEC §15): the test binary impersonates BOTH the agent
// CLI (--fakeagent) and every build/test/ship command (-fake-exec), so the
// full pipeline runs against a real temp git repo with no network and no
// real models.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "--fakeagent" {
		os.Exit(runFakeAgent(os.Args[2]))
	}
	if len(os.Args) > 2 && os.Args[1] == "-fake-exec" {
		os.Exit(runFakeExec(os.Args[2]))
	}
	os.Exit(m.Run())
}

// --- fake agent ----------------------------------------------------------

func runFakeAgent(promptFile string) int {
	data, err := os.ReadFile(promptFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeagent: read prompt:", err)
		return 1
	}
	prompt := string(data)
	if os.Getenv("FAKE_QUOTA") == "1" {
		fmt.Println("FAKEQUOTA: you have hit your rate limit")
		return 1
	}
	writeOut := func(name, content string) {
		_ = os.MkdirAll(".sdlc", 0o755)
		_ = os.WriteFile(filepath.Join(".sdlc", name), []byte(content), 0o644)
	}
	emptyReview := func(kind string) string {
		return `{"schema":"review/1","reviewed":"` + kind + `","findings":[],"summary":"looks good"}`
	}
	appendFile := func(path, line string) {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, line)
		f.Close()
	}
	switch {
	case strings.Contains(prompt, "planning agent"):
		writeOut("spec.json", `{"schema":"spec/1","issue_summary":"demo","approach":"edit app.txt",
			"affected_files":["src/app.txt"],"acceptance_criteria":["app.txt updated"],
			"error_paths":[],"out_of_scope":[],"compatibility_concerns":[]}`)
		writeOut("plan.json", `{"schema":"plan/1","steps":[{"id":"S1","description":"edit","files":["src/app.txt"],"verification":"unit"}],"risks":[]}`)
	case strings.Contains(prompt, "design reviewer"):
		marker := filepath.Join(os.Getenv("FAKE_MARKER_DIR"), "design_blocked")
		if os.Getenv("FAKE_DESIGN_BLOCK_ONCE") == "1" {
			if _, err := os.Stat(marker); err != nil {
				_ = os.WriteFile(marker, []byte("1"), 0o644)
				writeOut("review.json", `{"schema":"review/1","reviewed":"spec",
					"findings":[{"id":"F1","severity":"blocker","description":"missing edge case","recommendation":"add it"}],
					"summary":"needs work"}`)
				return 0
			}
		}
		writeOut("review.json", emptyReview("spec"))
	case strings.Contains(prompt, "implementation agent"):
		appendFile("src/app.txt", "implemented feature")
		if os.Getenv("FAKE_IMPL_BREAK") == "1" {
			_ = os.WriteFile("fail_unit", []byte("broken"), 0o644)
		}
		writeOut("implementation.json", `{"schema":"implementation/1","summary":"implemented",
			"files_changed":["src/app.txt"],"tests_added_or_changed":[],"test_change_requested":null}`)
	case strings.Contains(prompt, "code reviewer"):
		writeOut("review.json", emptyReview("diff"))
	case strings.Contains(prompt, "failure-analysis agent"):
		cls := os.Getenv("FAKE_ANALYSIS")
		if cls == "" {
			cls = "code_bug"
		}
		writeOut("analysis.json", `{"schema":"analysis/1","classification":"`+cls+`",
			"reasoning":"fail_unit marker present","failing_targets":["unit"],"fix_hint":"remove marker"}`)
	case strings.Contains(prompt, "fix agent"):
		if os.Getenv("FAKE_FIX_TOUCH_TEST") == "1" {
			appendFile("tests/app_test.txt", "weakened assertion")
		} else {
			_ = os.Remove("fail_unit")
			appendFile("src/app.txt", "fixed root cause")
		}
		writeOut("fix.json", `{"schema":"implementation/1","summary":"fixed",
			"files_changed":["src/app.txt"],"tests_added_or_changed":[],"test_change_requested":null}`)
	case strings.Contains(prompt, "final reviewer"):
		writeOut("review.json", emptyReview("final"))
	default:
		fmt.Fprintln(os.Stderr, "fakeagent: unrecognized role in prompt")
		return 1
	}
	fmt.Println(`{"model":"fake-model","usage":{"input_tokens":10,"output_tokens":5}}`)
	return 0
}

// --- fake build/test/ship executable -------------------------------------

func runFakeExec(phase string) int {
	if phase == "ship" {
		if os.Getenv("FAKE_SHIP_FAIL") == "1" {
			fmt.Println("ship failed")
			return 1
		}
		fmt.Println("shipped")
		return 0
	}
	if _, err := os.Stat("fail_" + phase); err == nil {
		fmt.Printf("%s FAILED: marker present\n", phase)
		return 1
	}
	fmt.Printf("%s ok\n", phase)
	return 0
}

// --- test environment -----------------------------------------------------

type env struct {
	t    *testing.T
	tmp  string
	repo string
	cfg  *config.Config
	st   *store.Store
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, tmp, "init", "-b", "main", "repo")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "test")
	os.WriteFile(filepath.Join(repo, "src", "app.txt"), []byte("v1\n"), 0o644)
	os.MkdirAll(filepath.Join(repo, "tests"), 0o755)
	os.WriteFile(filepath.Join(repo, "tests", "app_test.txt"), []byte("assert v1\n"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")

	cfg := config.Default()
	cfg.Path = filepath.Join(tmp, "sdlc.yaml")
	cfg.Orchestrator.DataDir = filepath.Join(tmp, "data")
	cfg.Orchestrator.LockFile = filepath.Join(tmp, "data", "engine.lock")
	cfg.Orchestrator.PollInterval = config.Duration(50 * time.Millisecond)
	cfg.Database.Path = filepath.Join(tmp, "data", "sdlc.db")
	cfg.Limits.ReleaseRetryBackoff = config.Duration(100 * time.Millisecond)
	cfg.Backends = map[string]config.Backend{
		"fake": {
			Kind: "exec", Binary: exe,
			ArgvTemplate:       []string{"--fakeagent", "{prompt_file}"},
			DefaultTimeout:     config.Duration(2 * time.Minute),
			QuotaErrorPatterns: []string{"FAKEQUOTA"},
			QuotaBackoff:       config.Duration(200 * time.Millisecond),
		},
	}
	cfg.Agents = map[string]config.Agent{"fake": {Backend: "fake", Model: "fake-model"}}
	states := map[string]config.State{}
	for _, s := range config.AgentStates {
		states[s] = config.State{Agent: "fake", Timeout: config.Duration(2 * time.Minute)}
	}
	cfg.States = states
	cfg.Policies.TestFileGlobs = []string{"tests/**"}
	cfg.Git.CleanupWorktrees = "never"
	fx := func(phase string) []string { return []string{exe, "-fake-exec", phase} }
	cfg.Targets = map[string]config.Target{
		"demo": {
			RepoPath:      repo,
			DefaultBranch: "main",
			BranchPrefix:  "sdlc/",
			WorktreesDir:  filepath.Join(repo, ".worktrees"),
			Build:         config.BuildCfg{Commands: [][]string{fx("build")}, Timeout: config.Duration(time.Minute)},
			UnitTest:      config.CmdCfg{Command: fx("unit"), Timeout: config.Duration(time.Minute)},
			Lint:          config.LintCfg{RunIn: "off", Timeout: config.Duration(time.Minute)},
			UITest:        config.UITestCfg{Enabled: false, OnNoDevice: "skip", Timeout: config.Duration(time.Minute)},
			Merge:         config.MergeCfg{RebaseBeforeMerge: true, VerifyAfterRebase: "none", Push: "never"},
			Ship:          config.ShipCfg{Command: fx("ship"), Timeout: config.Duration(time.Minute)},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Database.Path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &env{t: t, tmp: tmp, repo: repo, cfg: cfg, st: st}
}

func (e *env) submit(title string) *store.Job {
	e.t.Helper()
	id, err := e.st.NextJobID("JOB")
	if err != nil {
		e.t.Fatal(err)
	}
	j := &store.Job{
		ID: id, Target: "demo", IssueTitle: title, IssueBody: "do the thing",
		Branch: "sdlc/" + id, WorktreePath: filepath.Join(e.repo, ".worktrees", id),
		State: SCreated,
	}
	if err := e.st.CreateJob(j); err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) runEngine() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	eng := New(e.cfg, e.st, log.New(os.Stderr, "[engine] ", 0))
	if err := eng.Run(ctx, true); err != nil {
		e.t.Fatalf("engine: %v", err)
	}
}

func (e *env) jobState(id string) *store.Job {
	e.t.Helper()
	j, err := e.st.GetJob(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) artifactExists(id, name string) bool {
	_, err := os.Stat(filepath.Join(e.cfg.Orchestrator.DataDir, "jobs", id, "artifacts", name))
	return err == nil
}

// --- scenarios ------------------------------------------------------------

func TestHappyPathToCompletion(t *testing.T) {
	e := newEnv(t)
	j := e.submit("happy path")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SAwaitMerge {
		t.Fatalf("after run: state=%s hold=%q", got.State, got.HoldReason)
	}
	for _, a := range []string{"spec.json", "plan.json", "implementation.json", "code_review.r1.json", "final_review.json"} {
		if !e.artifactExists(j.ID, a) {
			t.Errorf("artifact %s missing", a)
		}
	}
	// The orchestrator, not the agent, must have committed the implementation.
	logOut := git(t, filepath.Join(e.repo, ".worktrees", j.ID), "log", "--oneline")
	if !strings.Contains(logOut, "[sdlc "+j.ID+"] IMPLEMENTING") {
		t.Errorf("no orchestrator commit found:\n%s", logOut)
	}

	// Approve merge.
	e.st.AddApproval(&store.Approval{JobID: j.ID, Gate: "merge", Decision: "approve"})
	e.runEngine()
	got = e.jobState(j.ID)
	if got.State != SAwaitRelease {
		t.Fatalf("after merge approval: state=%s hold=%q", got.State, got.HoldReason)
	}
	mainLog := git(t, e.repo, "log", "--oneline", "main")
	if !strings.Contains(mainLog, "[sdlc "+j.ID+"] merge:") {
		t.Errorf("merge commit missing on main:\n%s", mainLog)
	}

	// Approve release.
	e.st.AddApproval(&store.Approval{JobID: j.ID, Gate: "release", Decision: "approve"})
	e.runEngine()
	got = e.jobState(j.ID)
	if got.State != SCompleted {
		t.Fatalf("after release approval: state=%s hold=%q", got.State, got.HoldReason)
	}
}

func TestFixLoopThroughAnalysis(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_IMPL_BREAK", "1")
	t.Setenv("FAKE_ANALYSIS", "code_bug")
	j := e.submit("failing implementation")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q", got.State, got.HoldReason)
	}
	if got.Counters.FixAttempts != 1 {
		t.Errorf("fix_attempts=%d, want 1", got.Counters.FixAttempts)
	}
	if got.Counters.AnalysisRound != 1 {
		t.Errorf("analysis_round=%d, want 1", got.Counters.AnalysisRound)
	}
	if !e.artifactExists(j.ID, "analysis.a1.json") || !e.artifactExists(j.ID, "fix.f1.json") {
		t.Error("analysis/fix artifacts missing")
	}
	// The flake check must have run the failing phase deterministically.
	evs, _ := e.st.ListEvents(j.ID)
	sawFlake := false
	for _, ev := range evs {
		if strings.Contains(ev.Detail, "flake-rerun") {
			sawFlake = true
		}
	}
	if !sawFlake {
		t.Error("no flake-rerun exec events recorded")
	}
}

func TestFixTouchingTestsEscalates(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_IMPL_BREAK", "1")
	t.Setenv("FAKE_ANALYSIS", "code_bug")
	t.Setenv("FAKE_FIX_TOUCH_TEST", "1")
	j := e.submit("fix violates test policy")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SEscalated {
		t.Fatalf("state=%s, want ESCALATED; hold=%q", got.State, got.HoldReason)
	}
	if !strings.Contains(got.HoldReason, "touched test files") {
		t.Errorf("hold reason = %q", got.HoldReason)
	}
	// The violating change must have been discarded.
	testFile := filepath.Join(e.repo, ".worktrees", j.ID, "tests", "app_test.txt")
	data, _ := os.ReadFile(testFile)
	if strings.Contains(string(data), "weakened") {
		t.Error("violating test edit was not rolled back")
	}
}

func TestDesignReviewRejectLoop(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_DESIGN_BLOCK_ONCE", "1")
	t.Setenv("FAKE_MARKER_DIR", t.TempDir())
	j := e.submit("design gets rejected once")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q", got.State, got.HoldReason)
	}
	if got.Counters.DesignReviewRounds != 1 {
		t.Errorf("design_review_rounds=%d, want 1 rejection", got.Counters.DesignReviewRounds)
	}
	if !e.artifactExists(j.ID, "design_review.r1.json") || !e.artifactExists(j.ID, "design_review.r2.json") {
		t.Error("expected two design review round artifacts")
	}
}

func TestEnvironmentClassificationEscalates(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_IMPL_BREAK", "1")
	t.Setenv("FAKE_ANALYSIS", "environment")
	j := e.submit("environment failure")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SEscalated || !strings.Contains(got.HoldReason, "environment") {
		t.Fatalf("state=%s hold=%q", got.State, got.HoldReason)
	}
}

func TestQuotaSuspendsAndResumes(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_QUOTA", "1")
	j := e.submit("quota hit")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SBlockedQuota {
		t.Fatalf("state=%s, want BLOCKED_ON_QUOTA; hold=%q", got.State, got.HoldReason)
	}
	if got.PrevState != SPlanning {
		t.Errorf("prev_state=%s, want PLANNING", got.PrevState)
	}
	if got.Counters.FixAttempts != 0 || got.Counters.AgentRetries != 0 {
		t.Error("quota hit must not consume retry budgets")
	}
	// Clear the quota condition; backoff is 200ms, so a fresh engine resumes it.
	t.Setenv("FAKE_QUOTA", "")
	time.Sleep(300 * time.Millisecond)
	e.runEngine()
	got = e.jobState(j.ID)
	if got.State != SAwaitMerge {
		t.Fatalf("after resume: state=%s hold=%q", got.State, got.HoldReason)
	}
}

func TestCancelViaControlRow(t *testing.T) {
	e := newEnv(t)
	j := e.submit("to be cancelled")
	e.st.AddApproval(&store.Approval{JobID: j.ID, Gate: "cancel", Decision: "cancel", Reason: "changed my mind"})
	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SCancelled {
		t.Fatalf("state=%s, want CANCELLED", got.State)
	}
}

func TestMergeRejectRoutesToFixing(t *testing.T) {
	e := newEnv(t)
	j := e.submit("human rejects merge")
	e.runEngine()
	if s := e.jobState(j.ID).State; s != SAwaitMerge {
		t.Fatalf("setup: state=%s", s)
	}
	e.st.AddApproval(&store.Approval{JobID: j.ID, Gate: "merge", Decision: "reject", Reason: "wrong copy in dialog"})
	e.runEngine()
	got := e.jobState(j.ID)
	// The fix agent runs, the pipeline re-verifies, and it parks again.
	if got.State != SAwaitMerge {
		t.Fatalf("after reject+refix: state=%s hold=%q", got.State, got.HoldReason)
	}
	if got.Counters.FixAttempts != 1 {
		t.Errorf("fix_attempts=%d, want 1", got.Counters.FixAttempts)
	}
}

func TestCrashResumeReconcilesWorktree(t *testing.T) {
	e := newEnv(t)
	j := e.submit("crash resume")
	e.runEngine()
	if s := e.jobState(j.ID).State; s != SAwaitMerge {
		t.Fatalf("setup: state=%s", s)
	}
	// Simulate a crash mid-FIXING: force the state back and dirty the tree.
	got := e.jobState(j.ID)
	wt := filepath.Join(e.repo, ".worktrees", j.ID)
	os.WriteFile(filepath.Join(wt, "src", "app.txt"), []byte("half-finished garbage"), 0o644)
	got.State = SFinalReview // a restartable agent state
	if err := e.st.UpdateJob(got); err != nil {
		t.Fatal(err)
	}
	e.runEngine() // fresh engine ⇒ reconcile discards the partial edit, re-runs FINAL_REVIEW
	got = e.jobState(j.ID)
	if got.State != SAwaitMerge {
		t.Fatalf("after resume: state=%s hold=%q", got.State, got.HoldReason)
	}
	data, _ := os.ReadFile(filepath.Join(wt, "src", "app.txt"))
	if strings.Contains(string(data), "garbage") {
		t.Error("partial work not discarded on resume")
	}
}
