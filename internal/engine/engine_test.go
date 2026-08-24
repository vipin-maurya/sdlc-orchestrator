package engine

// Integration tests (SPEC §15): the test binary impersonates BOTH the agent
// CLI (--fakeagent) and every build/test/ship command (-fake-exec), so the
// full pipeline runs against a real temp git repo with no network and no
// real models.

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
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

// designFindingText is the description the fake design reviewer emits under
// FAKE_DESIGN_FINDING; tests match on it to prove the finding travelled.
const designFindingText = "savedStateHandle written to the wrong nav graph entry"

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
	// First: a verifier's prompt names other roles in passing ("about to send
	// an implementation agent to rework working code"), so the substring
	// heuristics below would claim it.
	case strings.Contains(prompt, "verification agent"):
		return runFakeVerifier(prompt)
	case strings.Contains(prompt, "scoping agent"):
		clarity := os.Getenv("FAKE_SCOPE_CLARITY")
		if clarity == "" {
			clarity = "clear"
		}
		// FAKE_SCOPE_BLOCK_ONCE blocks the first attempt only, so a test can
		// drive block -> reject -> re-scope -> clear. It uses the marker-file
		// trick FAKE_DESIGN_BLOCK_ONCE already uses: the fake is a fresh
		// process each invocation and has nowhere else to remember.
		if os.Getenv("FAKE_SCOPE_BLOCK_ONCE") != "" {
			marker := filepath.Join(os.Getenv("FAKE_MARKER_DIR"), "scoped-once")
			if _, err := os.Stat(marker); err != nil {
				_ = os.WriteFile(marker, []byte("1"), 0o644)
				clarity = "blocked"
			} else {
				clarity = "clear"
			}
		}
		questions, assumptions := "[]", "[]"
		switch clarity {
		case "blocked":
			questions = `[{"id":"Q1","question":"which parser?","why_it_matters":"different work","blocking":true}]`
		case "assumed":
			assumptions = `[{"assumption":"only the SMS path","basis":"NotificationParser has its own regex"}]`
		}
		if os.Getenv("FAKE_SCOPE_EDIT") != "" {
			_ = os.WriteFile("src/app.txt", []byte("scoped\n"), 0o644)
		}
		writeOut("problem.json", fmt.Sprintf(`{"schema":"problem/1",
			"problem_statement":"the parser drops amounts with a non-breaking space",
			"in_scope":["the SMS amount parser"],"out_of_scope":["the notification parser"],
			"success_criteria":["U+00A0 before the amount parses like a plain space"],
			"assumptions":%s,"open_questions":%s,"clarity":%q}`, assumptions, questions, clarity))
	case strings.Contains(prompt, "planning agent"):
		// FAKE_PLAN_REQUIRE_CONTEXT: a re-plan driven by a human rejection must
		// be handed the spec and plan it is revising. Without them the planner
		// writes a new spec from scratch instead of answering the objection,
		// and the staging condition that guarantees this is easy to narrow by
		// accident — it keys off a counter a human "no" never increments.
		if os.Getenv("FAKE_PLAN_REQUIRE_CONTEXT") == "1" && strings.Contains(prompt, "A human rejected") {
			for _, n := range []string{"spec.json", "plan.json"} {
				if _, err := os.Stat(filepath.Join(".sdlc", "context", n)); err != nil {
					fmt.Fprintf(os.Stderr, "fakeagent: context/%s not staged for the re-plan: %v\n", n, err)
					return 1
				}
			}
		}
		writeOut("spec.json", `{"schema":"spec/1","issue_summary":"demo","approach":"edit app.txt",
			"affected_files":["src/app.txt"],"acceptance_criteria":["app.txt updated"],
			"error_paths":[],"out_of_scope":[],"compatibility_concerns":[]}`)
		// Two steps, one of them a verification step the orchestrator owns —
		// the shape real plans have, and the one that exposed steps being
		// dropped silently.
		writeOut("plan.json", `{"schema":"plan/1","steps":[
			{"id":"S1","description":"edit","files":["src/app.txt"],"verification":"app.txt contains the feature line"},
			{"id":"S2","description":"suite green","files":[],"verification":"unit suite passes"}],"risks":[]}`)
	case strings.Contains(prompt, "design reviewer"):
		// FAKE_DESIGN_FINDING: report one finding of the given severity every
		// round, so a test can exercise the configured blocking threshold.
		if sev := os.Getenv("FAKE_DESIGN_FINDING"); sev != "" {
			writeOut("review.json", `{"schema":"review/1","reviewed":"spec",
				"findings":[{"id":"F1","severity":"`+sev+`",
				"description":"`+designFindingText+`","recommendation":"use getBackStackEntry"}],
				"summary":"one finding"}`)
			return 0
		}
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
		// When a design review produced findings, the orchestrator must have
		// staged the artifact the prompt points at, not just inlined the JSON.
		if os.Getenv("FAKE_DESIGN_FINDING") != "" {
			if _, err := os.Stat(filepath.Join(".sdlc", "context", "design_review.json")); err != nil {
				fmt.Fprintln(os.Stderr, "fakeagent: design_review.json not staged:", err)
				return 1
			}
		}
		steps := `[{"id":"S1","status":"done"},
			{"id":"S2","status":"skipped","note":"orchestrator-verified"}]`
		claimed := `["src/app.txt"]`
		// FAKE_IMPL_SKIP_STEP: drop a plan step from the report entirely.
		if os.Getenv("FAKE_IMPL_SKIP_STEP") == "1" {
			steps = `[{"id":"S1","status":"done"}]`
		}
		// FAKE_IMPL_PHANTOM: claim a file that was never touched.
		if os.Getenv("FAKE_IMPL_PHANTOM") == "1" {
			claimed = `["src/app.txt","src/never_touched.txt"]`
		}
		// FAKE_IMPL_UNREPORTED: change a second file and leave it off the report.
		if os.Getenv("FAKE_IMPL_UNREPORTED") == "1" {
			appendFile("src/extra.txt", "substituted work")
		}
		impl := `{"schema":"implementation/1","summary":"implemented",
			"files_changed":` + claimed + `,"tests_added_or_changed":[],
			"steps_completed":` + steps + `,"test_change_requested":null}`
		// FAKE_IMPL_BAD_FIRST: fail the post-condition on the first attempt
		// only, and edit the source exactly once — so the retry runs against a
		// worktree that already holds the complete implementation.
		if os.Getenv("FAKE_IMPL_BAD_FIRST") == "1" {
			marker := filepath.Join(os.Getenv("FAKE_MARKER_DIR"), "impl_failed_once")
			if _, err := os.Stat(marker); err != nil {
				_ = os.WriteFile(marker, []byte("1"), 0o644)
				appendFile("src/app.txt", "implemented feature")
				writeOut("implementation.json", `{"schema":"implementation/1","summary":""}`)
				return 0
			}
			writeOut("implementation.json", impl)
			return 0
		}
		// FAKE_IMPL_STEPS: finish two steps a moment apart, reporting each to
		// .sdlc/progress.jsonl as a real implementer is asked to. The pauses
		// are what let the orchestrator's watcher commit them separately;
		// without separate commits the checkpointing is not doing anything.
		if os.Getenv("FAKE_IMPL_STEPS") == "1" {
			// Stream a couple of actions first, in the shape a streaming
			// backend emits them.
			fmt.Println(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"src/app.txt"}}]}}`)
			appendFile("src/app.txt", "step one work")
			appendFile(filepath.Join(".sdlc", "progress.jsonl"), `{"step":"S1","summary":"did step one"}`)
			time.Sleep(800 * time.Millisecond)
			appendFile("src/app.txt", "step two work")
			appendFile(filepath.Join(".sdlc", "progress.jsonl"), `{"step":"S2","summary":"did step two"}`)
			time.Sleep(800 * time.Millisecond)
			writeOut("implementation.json", impl)
			return 0
		}
		appendFile("src/app.txt", "implemented feature")
		if os.Getenv("FAKE_IMPL_BREAK") == "1" {
			_ = os.WriteFile("fail_unit", []byte("broken"), 0o644)
		}
		if os.Getenv("FAKE_IMPL_BOM") == "1" {
			impl = string(rune(0xFEFF)) + impl // UTF-8 BOM, as a Windows agent emits
		}
		writeOut("implementation.json", impl)
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

// verdictPathRe pulls the output path the orchestrator assigned this verifier
// out of the rendered prompt — the same way a real agent would read it.
var verdictPathRe = regexp.MustCompile(`\.sdlc/verdicts/([A-Za-z0-9_-]+)\.v(\d+)\.json`)

// runFakeVerifier impersonates one verifier in the verify pass.
//
// FAKE_VERDICTS is a comma-separated list read by vote index, e.g.
// "refuted,refuted,confirmed" — so a test can script an exact 1-of-3 outcome
// rather than hoping for one. "none" makes that verifier produce no file at
// all, which is how a dropped vote is exercised.
func runFakeVerifier(prompt string) int {
	m := verdictPathRe.FindStringSubmatch(prompt)
	if m == nil {
		fmt.Fprintln(os.Stderr, "fakeverifier: no verdict path in prompt")
		return 1
	}
	rel, findingID, vote := m[0], m[1], m[2]
	idx, _ := strconv.Atoi(vote)

	want := "refuted"
	if list := os.Getenv("FAKE_VERDICTS"); list != "" {
		parts := strings.Split(list, ",")
		if idx >= 1 && idx <= len(parts) {
			want = strings.TrimSpace(parts[idx-1])
		} else {
			want = strings.TrimSpace(parts[len(parts)-1])
		}
	}
	if want == "none" {
		return 1 // wrote nothing: the vote is dropped, not retried
	}
	// Each verifier must have been handed the finding itself, or it has
	// nothing to argue about.
	if !strings.Contains(prompt, designFindingText) && !strings.Contains(prompt, "missing edge case") {
		fmt.Fprintln(os.Stderr, "fakeverifier: prompt carries no finding")
		return 1
	}
	body := `{"schema":"verdict/1","finding_id":"` + findingID + `","verdict":"` + want + `",
		"evidence":"disassembled navigation-runtime-2.6.0.jar from the gradle cache",
		"reasoning":"getPreviousBackStackEntry skips NavGraph entries in this version"}`
	_ = os.MkdirAll(filepath.Dir(rel), 0o755)
	if err := os.WriteFile(rel, []byte(body), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fakeverifier:", err)
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
	eng := New(config.NewLive(e.cfg), e.st, log.New(os.Stderr, "[engine] ", 0))
	if err := eng.Run(ctx, true); err != nil {
		e.t.Fatalf("engine: %v", err)
	}
}

// syncBuf is a log destination the race detector tolerates. The engine logs
// from tick *and* from every step goroutine, so a bare bytes.Buffer behind
// log.Logger is not enough: log.Logger serialises its own writes, but the test
// then reads the buffer from a third goroutine while a step is still finishing.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// runEngineLogged is runEngine with the console captured. The gate
// announcements are the console, so a test that asserts on them has to read
// what an operator would have seen rather than what the database ended up
// holding.
func (e *env) runEngineLogged() string {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	buf := &syncBuf{}
	eng := New(config.NewLive(e.cfg), e.st, log.New(buf, "", 0))
	if err := eng.Run(ctx, true); err != nil {
		e.t.Fatalf("engine: %v", err)
	}
	return buf.String()
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
	// The blocker is real, so the verifiers uphold it and the loop runs.
	t.Setenv("FAKE_VERDICTS", "confirmed,confirmed,confirmed")
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
	if got.PrevState != SScoping {
		t.Errorf("prev_state=%s, want SCOPING", got.PrevState)
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

// promptsFor returns the rendered prompts for a state, in dispatch order.
func (e *env) promptsFor(id, state string) []string {
	e.t.Helper()
	dir := filepath.Join(e.cfg.Orchestrator.DataDir, "jobs", id, "prompts")
	matches, err := filepath.Glob(filepath.Join(dir, "*_"+state+".md"))
	if err != nil {
		e.t.Fatal(err)
	}
	sort.Strings(matches) // names are zero-padded sequence numbers
	var out []string
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			e.t.Fatal(err)
		}
		out = append(out, string(data))
	}
	return out
}

// verifierPrompts returns the prompts handed to verify-pass agents. They are
// named <seq>_VERIFYING_<finding>_v<n>.md, so promptsFor's exact-suffix glob
// does not see them.
func (e *env) verifierPrompts(id string) []string {
	e.t.Helper()
	dir := filepath.Join(e.cfg.Orchestrator.DataDir, "jobs", id, "prompts")
	matches, err := filepath.Glob(filepath.Join(dir, "*_VERIFYING_*.md"))
	if err != nil {
		e.t.Fatal(err)
	}
	sort.Strings(matches)
	var out []string
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			e.t.Fatal(err)
		}
		out = append(out, string(data))
	}
	return out
}

// A BOM-prefixed artifact must load on the first attempt. Before ReadJSON
// stripped it, a complete implementation was scored as a post-condition
// failure and the whole state re-run.
func TestBOMPrefixedArtifactNeedsNoRetry(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_IMPL_BOM", "1")
	j := e.submit("bom artifact")

	e.runEngine()
	if got := e.jobState(j.ID); got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q", got.State, got.HoldReason)
	}
	if n := len(e.promptsFor(j.ID, SImplementing)); n != 1 {
		t.Errorf("IMPLEMENTING dispatched %d time(s), want 1 — the BOM caused a retry", n)
	}
}

// eventDetails returns the Detail JSON of every event for a job, so a test can
// assert on the notes the orchestrator recorded.
func (e *env) eventDetails(id string) string {
	e.t.Helper()
	evs, err := e.st.ListEvents(id)
	if err != nil {
		e.t.Fatal(err)
	}
	var b strings.Builder
	for _, ev := range evs {
		b.WriteString(ev.Detail)
		b.WriteByte('\n')
	}
	return b.String()
}

// A plan step the implementer says nothing about fails the state. Silence is
// indistinguishable from forgetting, and JOB-1 lost both of its verification
// steps this way — steps_completed was not in the schema, so it was decoded
// away and nothing noticed.
func TestUnaccountedPlanStepFailsImplementing(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_IMPL_SKIP_STEP", "1")
	j := e.submit("dropped step")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SEscalated {
		t.Fatalf("state=%s, want %s", got.State, SEscalated)
	}
	if !strings.Contains(got.HoldReason, "does not account for plan step(s) S2") {
		t.Errorf("hold reason does not name the missing step: %q", got.HoldReason)
	}
}

// A step reported as skipped WITH a note is a legitimate answer — the default
// fake reports S2 that way — and it must be recorded where a human sees it.
func TestSkippedStepPassesAndIsRecorded(t *testing.T) {
	e := newEnv(t)
	j := e.submit("skipped step")

	e.runEngine()
	if got := e.jobState(j.ID); got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q", got.State, got.HoldReason)
	}
	details := e.eventDetails(j.ID)
	if !strings.Contains(details, "plan_steps_skipped") || !strings.Contains(details, "orchestrator-verified") {
		t.Errorf("skipped step not recorded in the event log:\n%s", details)
	}
}

// Claiming a file that was never modified fails the state: a self-report
// nobody checks is worth nothing.
func TestPhantomFileClaimFailsImplementing(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_IMPL_PHANTOM", "1")
	j := e.submit("phantom claim")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SEscalated {
		t.Fatalf("state=%s, want %s", got.State, SEscalated)
	}
	if !strings.Contains(got.HoldReason, "src/never_touched.txt") {
		t.Errorf("hold reason does not name the phantom file: %q", got.HoldReason)
	}
}

// The converse is not a failure — substituting a file is often the right call —
// but it must be visible rather than silent.
func TestUnreportedChangeIsRecordedNotFailed(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_IMPL_UNREPORTED", "1")
	j := e.submit("unreported change")

	e.runEngine()
	if got := e.jobState(j.ID); got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q, want %s", got.State, got.HoldReason, SAwaitMerge)
	}
	details := e.eventDetails(j.ID)
	if !strings.Contains(details, "changed_but_unreported") || !strings.Contains(details, "src/extra.txt") {
		t.Errorf("unreported change not recorded:\n%s", details)
	}
}

// With design_review_blocks_at: major, a major finding must stop the pipeline.
// JOB-1 is the case this pins: one major finding, a hardcoded blocker-only
// gate, and the implementer went on to write exactly the bug it described.
func TestDesignReviewBlocksAtConfiguredSeverity(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.DesignReviewBlocksAt = "major"
	t.Setenv("FAKE_DESIGN_FINDING", "major")
	// The finding is real: every verifier upholds it, so the gate must hold.
	t.Setenv("FAKE_VERDICTS", "confirmed,confirmed,confirmed")
	j := e.submit("major finding")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SEscalated {
		t.Fatalf("state=%s, want %s (the major finding should never clear the gate)", got.State, SEscalated)
	}
	if !strings.Contains(got.HoldReason, "at or above major") {
		t.Errorf("hold reason does not name the threshold: %q", got.HoldReason)
	}
	if got.Counters.DesignReviewRounds != e.cfg.Limits.MaxDesignReviewRounds {
		t.Errorf("design_review_rounds=%d, want %d", got.Counters.DesignReviewRounds, e.cfg.Limits.MaxDesignReviewRounds)
	}
	if !e.artifactExists(j.ID, "design_review.r2.json") {
		t.Error("second design review round did not run")
	}
}

// A finding below the threshold must not be discarded: it is handed to the
// implementer, who is the last agent that will ever see it.
func TestNonBlockingFindingsReachImplementer(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.DesignReviewBlocksAt = "blocker" // so "major" passes the gate
	t.Setenv("FAKE_DESIGN_FINDING", "major")
	j := e.submit("passing finding")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q, want %s", got.State, got.HoldReason, SAwaitMerge)
	}
	if got.Counters.LastDesignReview != "design_review.r1.json" {
		t.Errorf("last_design_review=%q, want design_review.r1.json", got.Counters.LastDesignReview)
	}
	prompts := e.promptsFor(j.ID, SImplementing)
	if len(prompts) != 1 {
		t.Fatalf("IMPLEMENTING dispatched %d time(s), want 1", len(prompts))
	}
	if !strings.Contains(prompts[0], designFindingText) {
		t.Errorf("implementing prompt does not carry the finding:\n%s", prompts[0])
	}
}

// The converse: a clean review must not decorate the prompt with an empty
// findings section, and must leave no pointer behind.
func TestCleanDesignReviewForwardsNothing(t *testing.T) {
	e := newEnv(t)
	j := e.submit("clean review")

	e.runEngine()
	if got := e.jobState(j.ID); got.Counters.LastDesignReview != "" {
		t.Errorf("last_design_review=%q, want empty", got.Counters.LastDesignReview)
	}
	prompts := e.promptsFor(j.ID, SImplementing)
	if len(prompts) != 1 {
		t.Fatalf("IMPLEMENTING dispatched %d time(s), want 1", len(prompts))
	}
	if strings.Contains(prompts[0], "Design review:") {
		t.Errorf("clean review still produced a findings section:\n%s", prompts[0])
	}
}

// A retry must tell the agent what the failed attempt already did, so it
// corrects the output file instead of re-applying its own edits.
func TestRetryPromptDescribesWorktree(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_IMPL_BAD_FIRST", "1")
	t.Setenv("FAKE_MARKER_DIR", t.TempDir())
	j := e.submit("retry context")

	e.runEngine()
	if got := e.jobState(j.ID); got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q", got.State, got.HoldReason)
	}
	prompts := e.promptsFor(j.ID, SImplementing)
	if len(prompts) != 2 {
		t.Fatalf("IMPLEMENTING dispatched %d time(s), want 2", len(prompts))
	}
	if strings.Contains(prompts[0], "# IMPORTANT") {
		t.Error("first-attempt prompt carries a retry block")
	}
	retry := prompts[1]
	for _, want := range []string{
		"retry 2 of",
		"summary (non-empty",                     // the actual post-condition failure, quoted
		"keys actually present: schema, summary", // and what the file did contain
		"already modified 1 file(s)",             // counted from the real worktree diff
		"- src/app.txt",                          // the file the failed attempt edited
		"`.sdlc/implementation.json`",            // the output it left behind
	} {
		if !strings.Contains(retry, want) {
			t.Errorf("retry prompt missing %q\n---\n%s", want, retry)
		}
	}
	// The source must have been edited exactly once: the retry was told not to
	// redo the work, and the fake agent obeys.
	wt := filepath.Join(e.repo, ".worktrees", j.ID)
	data, _ := os.ReadFile(filepath.Join(wt, "src", "app.txt"))
	if n := strings.Count(string(data), "implemented feature"); n != 1 {
		t.Errorf("implementation applied %d times, want 1:\n%s", n, data)
	}
}

// --- verify pass ----------------------------------------------------------

// JOB-1's F1 replayed: a major finding, articulate and wrong. Two of three
// verifiers refute it, so it must not gate — and the job must reach the merge
// gate instead of burning its design-review rounds reworking a plan that was
// already correct.
func TestMajorityRefutedFindingDoesNotGate(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.DesignReviewBlocksAt = "major"
	t.Setenv("FAKE_DESIGN_FINDING", "major")
	t.Setenv("FAKE_VERDICTS", "refuted,refuted,confirmed") // 1 of 3 confirms; min is 2
	j := e.submit("false positive")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q, want %s — a refuted finding must not block", got.State, got.HoldReason, SAwaitMerge)
	}
	if got.Counters.DesignReviewRounds != 0 {
		t.Errorf("design_review_rounds=%d, want 0: the gate should never have tripped", got.Counters.DesignReviewRounds)
	}
	if !e.artifactExists(j.ID, "design_review.r1.verified.json") {
		t.Fatal("verified artifact not written")
	}

	// The finding survives in the record, downgraded and annotated — never
	// deleted, so the next reader can see the question was already asked.
	rev, err := artifact.LoadReview(filepath.Join(e.cfg.Orchestrator.DataDir, "jobs", j.ID,
		"artifacts", "design_review.r1.verified.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rev.Findings) != 1 {
		t.Fatalf("verified review has %d finding(s), want 1 (findings are annotated, not removed)", len(rev.Findings))
	}
	f := rev.Findings[0]
	if f.Severity != artifact.DowngradedSeverity {
		t.Errorf("severity=%q, want %q", f.Severity, artifact.DowngradedSeverity)
	}
	if f.Verification == nil {
		t.Fatal("no verification record attached")
	}
	if f.Verification.OriginalSeverity != "major" {
		t.Errorf("original_severity=%q, want major", f.Verification.OriginalSeverity)
	}
	if f.Verification.Confirmed != 1 || f.Verification.Refuted != 2 || f.Verification.Survived {
		t.Errorf("verification = %+v, want 1 confirmed / 2 refuted / not survived", f.Verification)
	}
	if len(f.Verification.Verdicts) != 3 {
		t.Errorf("kept %d verdict(s), want 3 — the refutations are the record", len(f.Verification.Verdicts))
	}

	details := e.eventDetails(j.ID)
	if !strings.Contains(details, "verify_pass") {
		t.Errorf("no verify_pass event recorded:\n%s", details)
	}
	// The implementer must be handed the verified copy, not the raw review:
	// a dismissed finding without its refutation invites rework all over again.
	if got.Counters.LastDesignReview != "design_review.r1.verified.json" {
		t.Errorf("last_design_review=%q, want the verified artifact", got.Counters.LastDesignReview)
	}
}

// Exactly minConfirm confirmations is enough: 2 of 3 upholds the finding and
// the gate holds.
func TestMinorityRefutationDoesNotSaveAFinding(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.DesignReviewBlocksAt = "major"
	t.Setenv("FAKE_DESIGN_FINDING", "major")
	t.Setenv("FAKE_VERDICTS", "confirmed,refuted,confirmed") // 2 of 3
	j := e.submit("real finding")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SEscalated {
		t.Fatalf("state=%s, want %s — 2 of 3 confirmations must uphold the finding", got.State, SEscalated)
	}
	if !strings.Contains(got.HoldReason, "at or above major") {
		t.Errorf("hold reason does not name the threshold: %q", got.HoldReason)
	}
}

// A clean review must not invoke a single verifier. The pass is only worth
// having if it is free in the common case.
func TestVerifyPassSkippedWhenNothingGates(t *testing.T) {
	e := newEnv(t)
	j := e.submit("clean review")

	e.runEngine()
	if got := e.jobState(j.ID); got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q", got.State, got.HoldReason)
	}
	if n := len(e.verifierPrompts(j.ID)); n != 0 {
		t.Errorf("%d verifier(s) invoked on a clean review, want 0", n)
	}
	if strings.Contains(e.eventDetails(j.ID), "verify_pass") {
		t.Error("verify_pass event emitted with nothing to verify")
	}
}

// Below-threshold findings are never voted on: spending tokens on a finding
// that cannot change control flow is waste.
func TestNonGatingFindingIsNotVerified(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.DesignReviewBlocksAt = "blocker" // so the major finding passes
	t.Setenv("FAKE_DESIGN_FINDING", "major")
	j := e.submit("non-gating finding")

	e.runEngine()
	if got := e.jobState(j.ID); got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q", got.State, got.HoldReason)
	}
	if n := len(e.verifierPrompts(j.ID)); n != 0 {
		t.Errorf("%d verifier(s) invoked on a non-gating finding, want 0", n)
	}
}

// Turning the pass off must restore the previous behaviour exactly: the
// reviewer's word gates, unaudited.
func TestVerifyVotesZeroDisablesThePass(t *testing.T) {
	e := newEnv(t)
	e.cfg.Limits.VerifyVotes = 0
	e.cfg.Policies.DesignReviewBlocksAt = "major"
	t.Setenv("FAKE_DESIGN_FINDING", "major")
	t.Setenv("FAKE_VERDICTS", "refuted,refuted,refuted") // would be refuted if asked
	j := e.submit("verify off")

	e.runEngine()
	if got := e.jobState(j.ID); got.State != SEscalated {
		t.Fatalf("state=%s, want %s with the verify pass disabled", got.State, SEscalated)
	}
	if n := len(e.verifierPrompts(j.ID)); n != 0 {
		t.Errorf("%d verifier(s) invoked with verify_votes=0, want 0", n)
	}
}

// A finding no verifier could rule on stays gating. Declining to decide is not
// a refutation, and the conservative direction is to keep the block.
func TestInconclusiveVerificationLeavesTheFindingGating(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.DesignReviewBlocksAt = "major"
	t.Setenv("FAKE_DESIGN_FINDING", "major")
	t.Setenv("FAKE_VERDICTS", "none,none,none")
	j := e.submit("inconclusive")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SEscalated {
		t.Fatalf("state=%s, want %s", got.State, SEscalated)
	}
	if !strings.Contains(e.eventDetails(j.ID), "verify_inconclusive") {
		t.Error("inconclusive verification not recorded")
	}
}

// --- out-of-band commits --------------------------------------------------

// A human commits on the job branch while the job is held — the normal way to
// answer an escalation. The orchestrator must adopt those commits, not charge
// them to the next agent and not roll them back.
//
// This is JOB-1's worst outcome: two human commits containing a verified fix
// were attributed to the fix agent, tripped the test-file guard, and were
// erased along with the agent's own work.
func TestHumanCommitsOnJobBranchAreNotChargedToTheAgent(t *testing.T) {
	e := newEnv(t)
	// Drive the job to the JOB-1 escalation: unit tests fail, the failure is
	// classified code_bug, and the fix agent reaches for a test file. The
	// test-file guard discards that and escalates — which is correct, and is
	// the point at which a human steps in.
	t.Setenv("FAKE_IMPL_BREAK", "1")
	t.Setenv("FAKE_ANALYSIS", "code_bug")
	t.Setenv("FAKE_FIX_TOUCH_TEST", "1")
	j := e.submit("human intervention")

	e.runEngine()
	before := e.jobState(j.ID)
	if before.State != SEscalated {
		t.Fatalf("state=%s hold=%q, want %s", before.State, before.HoldReason, SEscalated)
	}

	// The human answers the escalation the way people actually do: by editing
	// the stale fixture and committing it on the job branch.
	wt := filepath.Join(e.repo, ".worktrees", j.ID)
	testFile := filepath.Join(wt, "tests", "app_test.txt")
	data, _ := os.ReadFile(testFile)
	if err := os.WriteFile(testFile, append(data, []byte("assert v2 (updated by hand)\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-m", "human: update the stale fixture")
	humanSHA := strings.TrimSpace(git(t, wt, "rev-parse", "HEAD"))
	if humanSHA == before.HeadSHA {
		t.Fatal("test setup: the human commit did not move the branch")
	}

	// Resume with a note — the channel that did not exist during JOB-1.
	e.st.AddApproval(&store.Approval{
		JobID: j.ID, Gate: "resume", Decision: "resume",
		Reason: SFixing, Note: "those fixtures are stale; updating them is correct",
	})
	e.runEngine()

	// Whatever the job did next, the human's commit must still be on the
	// branch and their edit must still be in the file. Under the old stale
	// baseline both were charged to the agent and reset away.
	logOut := git(t, wt, "log", "--oneline")
	if !strings.Contains(logOut, "human: update the stale fixture") {
		t.Fatalf("the human commit was destroyed:\n%s", logOut)
	}
	body, _ := os.ReadFile(testFile)
	if !strings.Contains(string(body), "updated by hand") {
		t.Errorf("the human's edit was rolled back:\n%s", body)
	}
	// And the branch move must be recorded, not silently absorbed.
	if details := e.eventDetails(j.ID); !strings.Contains(details, "head_sha_resynced") &&
		!strings.Contains(details, "branch_advanced_outside_orchestrator") {
		t.Errorf("branch movement was never recorded:\n%s", details)
	}
}

// The note must reach the agent, and it must be scoped to one attempt: the
// classification that made the previous prompt forbid test edits is cleared,
// so the prompt renders its permissive branch instead.
func TestResumeNoteReachesTheFixAgent(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_IMPL_BREAK", "1")
	t.Setenv("FAKE_ANALYSIS", "code_bug")
	t.Setenv("FAKE_FIX_TOUCH_TEST", "1")
	j := e.submit("resume with a note")
	e.runEngine()
	if got := e.jobState(j.ID); got.State != SEscalated {
		t.Fatalf("state=%s hold=%q, want %s", got.State, got.HoldReason, SEscalated)
	}
	beforeResume := len(e.promptsFor(j.ID, SFixing))

	const note = "those four fixtures are stale, update them"
	e.st.AddApproval(&store.Approval{
		JobID: j.ID, Gate: "resume", Decision: "resume", Reason: SFixing, Note: note,
	})
	e.runEngine()

	prompts := e.promptsFor(j.ID, SFixing)
	if len(prompts) <= beforeResume {
		t.Fatalf("FIXING did not run again after resume (%d prompts, was %d)", len(prompts), beforeResume)
	}
	resumed := prompts[beforeResume]
	if !strings.Contains(resumed, note) {
		t.Errorf("the fix prompt does not carry the operator's note:\n%s", resumed)
	}
	// Clearing LastAnalysis is what makes the grant real: with the code_bug
	// classification still set, the prompt would render the forbidden branch
	// again and the job would escalate identically.
	if strings.Contains(resumed, "The classification is `code_bug`") {
		t.Errorf("the note did not lift the code_bug restriction:\n%s", resumed)
	}
	if !strings.Contains(e.eventDetails(j.ID), note) {
		t.Error("the note was not recorded in the event log")
	}
}

// `resume --to` must refuse a state a job cannot actually run, rather than
// wedging the job in it.
func TestResumeRejectsANonRunnableState(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_IMPL_SKIP_STEP", "1") // escalates
	j := e.submit("bad resume target")
	e.runEngine()
	if got := e.jobState(j.ID); got.State != SEscalated {
		t.Fatalf("state=%s, want %s", got.State, SEscalated)
	}

	e.st.AddApproval(&store.Approval{JobID: j.ID, Gate: "resume", Decision: "resume", Reason: SCompleted})
	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SEscalated {
		t.Errorf("state=%s, want the job to stay %s", got.State, SEscalated)
	}
	if !strings.Contains(e.eventDetails(j.ID), "resume_rejected") {
		t.Error("the rejected resume target was not recorded")
	}
}

// An escalated state's output is what the operator has to read. It must be
// committed, not left in a worktree that the next reconcile deletes.
func TestEscalatedStateWorkIsCommitted(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_IMPL_SKIP_STEP", "1") // IMPLEMENTING edits src/app.txt, then fails
	j := e.submit("escalated work")

	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SEscalated {
		t.Fatalf("state=%s, want %s", got.State, SEscalated)
	}
	wt := filepath.Join(e.repo, ".worktrees", j.ID)
	if out := git(t, wt, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("worktree left dirty across a state boundary:\n%s", out)
	}
	logOut := git(t, wt, "log", "--oneline")
	if !strings.Contains(logOut, "IMPLEMENTING (incomplete)") {
		t.Errorf("the failed state's work was not preserved:\n%s", logOut)
	}
	if !strings.Contains(e.eventDetails(j.ID), "preserved_incomplete_work") {
		t.Error("preservation not recorded in the event log")
	}
}

// --- observability and checkpointing -------------------------------------

// A state that produces code must land in the branch step by step. One commit
// per state means a long implementation is all-or-nothing: JOB-1's 13-minute,
// 22-file IMPLEMENTING run was discarded whole over a malformed output file.
func TestPlanStepsAreCheckpointedAsTheyComplete(t *testing.T) {
	old := checkpointPollInterval
	checkpointPollInterval = 50 * time.Millisecond
	t.Cleanup(func() { checkpointPollInterval = old })

	e := newEnv(t)
	t.Setenv("FAKE_IMPL_STEPS", "1")
	j := e.submit("checkpointed implementation")

	e.runEngine()
	if got := e.jobState(j.ID); got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q", got.State, got.HoldReason)
	}
	wt := filepath.Join(e.repo, ".worktrees", j.ID)
	logOut := git(t, wt, "log", "--oneline")
	for _, want := range []string{"IMPLEMENTING checkpoint S1", "IMPLEMENTING checkpoint S2"} {
		if !strings.Contains(logOut, want) {
			t.Errorf("missing %q in:\n%s", want, logOut)
		}
	}
	// Two separate commits is the whole claim: if the watcher only drained at
	// the end, the second step would find a clean tree and commit nothing.
	if n := strings.Count(logOut, "IMPLEMENTING checkpoint"); n != 2 {
		t.Errorf("want 2 checkpoint commits, got %d:\n%s", n, logOut)
	}
	details := e.eventDetails(j.ID)
	if !strings.Contains(details, `"checkpoint":"S1"`) {
		t.Errorf("checkpoint not recorded as a progress event:\n%s", details)
	}
}

// The engine used to log only at transitions, so a state in flight and a hung
// one were indistinguishable. Every agent state must announce that it started
// and report what the agent is doing.
func TestAgentActionsBecomeProgressEvents(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_IMPL_STEPS", "1")
	j := e.submit("streamed progress")

	e.runEngine()
	details := e.eventDetails(j.ID)
	if !strings.Contains(details, `"started":true`) {
		t.Errorf("no state-start progress event:\n%s", details)
	}
	if !strings.Contains(details, "Edit src/app.txt") {
		t.Errorf("agent action not condensed into a progress event:\n%s", details)
	}
}

// The agent log has to be readable while the agent is still running: it is the
// only window `sdlc logs JOB --last` has into a state that has been going for
// ten minutes.
func TestAgentLogIsWrittenDuringTheRun(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_IMPL_STEPS", "1")
	j := e.submit("live log")

	e.runEngine()
	logsDir := filepath.Join(e.cfg.Orchestrator.DataDir, "jobs", j.ID, "logs")
	ents, err := os.ReadDir(logsDir)
	if err != nil {
		t.Fatal(err)
	}
	var implLog string
	for _, ent := range ents {
		if strings.Contains(ent.Name(), "IMPLEMENTING") {
			implLog = filepath.Join(logsDir, ent.Name())
		}
	}
	if implLog == "" {
		t.Fatalf("no IMPLEMENTING log in %s", logsDir)
	}
	data, err := os.ReadFile(implLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "tool_use") {
		t.Errorf("agent stdout did not reach the log file:\n%s", data)
	}
}

// Checkpoints survive a restart, so a re-run of IMPLEMENTING starts against a
// tree that already holds part of its own work. The agent must be told: a
// first-attempt prompt against a half-finished tree is what makes an agent
// redo completed work on top of itself.
func TestInterruptedStateTellsTheAgentWhatIsAlreadyDone(t *testing.T) {
	e := newEnv(t)
	j := e.submit("interrupted implementation")
	e.runEngine()
	if s := e.jobState(j.ID).State; s != SAwaitMerge {
		t.Fatalf("setup: state=%s", s)
	}
	wt := filepath.Join(e.repo, ".worktrees", j.ID)
	baseline := strings.TrimSpace(git(t, wt, "rev-parse", "HEAD~1"))

	// Simulate a restart part-way through IMPLEMENTING: the state is back in
	// flight and its earlier work is committed on the branch ahead of the
	// baseline it recorded on entry.
	got := e.jobState(j.ID)
	got.State = SImplementing
	got.Counters.StateEntryHead = baseline
	if err := e.st.UpdateJob(got); err != nil {
		t.Fatal(err)
	}

	e.runEngine()
	prompts := e.promptsFor(j.ID, SImplementing)
	last := prompts[len(prompts)-1]
	if !strings.Contains(last, "interrupted and is being re-run") {
		t.Errorf("re-run prompt does not say the state was interrupted:\n%s", last)
	}
	if !strings.Contains(last, "src/app.txt") {
		t.Errorf("re-run prompt does not list the work already on the branch:\n%s", last)
	}
	if strings.Contains(last, "post-condition check") {
		t.Error("an interrupted run is not a post-condition failure; the prompt says it is")
	}
}

// The baseline a state measures its work against is captured once on entry and
// kept. Re-reading it per run would make a state that already checkpointed
// look like it had done nothing at all.
func TestStateBaselineIsCapturedOnceAndClearedOnTransition(t *testing.T) {
	e := newEnv(t)
	j := e.submit("baseline lifecycle")
	e.runEngine()
	got := e.jobState(j.ID)
	if got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q", got.State, got.HoldReason)
	}
	if got.Counters.StateEntryHead != "" {
		t.Errorf("StateEntryHead=%q, want it cleared by the transition out of the state",
			got.Counters.StateEntryHead)
	}
}

// The default path: a clear problem statement goes straight to planning, and
// the artifact is harvested where every later state and the gate document
// expect to find it.
func TestScopingClearGoesToPlanning(t *testing.T) {
	e := newEnv(t)
	j := e.submit("non-breaking space in SMS amounts")

	e.runEngine()

	if got := e.jobState(j.ID); got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q", got.State, got.HoldReason)
	}
	if !e.artifactExists(j.ID, "problem.json") {
		t.Error("problem.json was not harvested")
	}
	if n := len(e.promptsFor(j.ID, SScoping)); n != 1 {
		t.Errorf("SCOPING dispatched %d time(s), want 1", n)
	}
}

// A blocking question stops the job at the gate, not in the ESCALATED hold: a
// vague ticket is an expected outcome and belongs in the approve/reject
// surface the operator already uses.
func TestScopingBlockedParksAtTheGate(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_SCOPE_CLARITY", "blocked")
	j := e.submit("fix the thing")

	e.runEngine()

	got := e.jobState(j.ID)
	if got.State != SAwaitScope {
		t.Fatalf("state=%s hold=%q, want %s", got.State, got.HoldReason, SAwaitScope)
	}
	if n := len(e.promptsFor(j.ID, SPlanning)); n != 0 {
		t.Errorf("PLANNING ran %d time(s) before the scope was settled", n)
	}
}

// The policy gate parks a perfectly clear problem statement too, for an
// operator who wants to check every one.
func TestScopingPolicyGateParksAClearProblem(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.HumanGates = []string{"scope"}
	j := e.submit("non-breaking space in SMS amounts")

	e.runEngine()

	if got := e.jobState(j.ID); got.State != SAwaitScope {
		t.Fatalf("state=%s, want %s", got.State, SAwaitScope)
	}
}

// policies.scoping: off must reproduce the pre-feature pipeline exactly.
func TestScopingOffSkipsTheState(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.Scoping = "off"
	j := e.submit("non-breaking space in SMS amounts")

	e.runEngine()

	if got := e.jobState(j.ID); got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q", got.State, got.HoldReason)
	}
	if n := len(e.promptsFor(j.ID, SScoping)); n != 0 {
		t.Errorf("SCOPING ran %d time(s) with scoping off", n)
	}
	if e.artifactExists(j.ID, "problem.json") {
		t.Error("problem.json exists with scoping off")
	}
}

// A loop between a human and an agent that neither side ends is worse than a
// stop, so the round cap escalates rather than dispatching the agent again.
func TestScopeRoundCapEscalates(t *testing.T) {
	e := newEnv(t)
	e.cfg.Limits.MaxScopeRounds = 1
	j := e.submit("fix the thing")
	j.Counters.ScopeRounds = 1
	j.State = SScoping
	if err := e.st.UpdateJob(j); err != nil {
		t.Fatal(err)
	}

	e.runEngine()

	got := e.jobState(j.ID)
	if got.State != SEscalated {
		t.Fatalf("state=%s, want %s", got.State, SEscalated)
	}
	if !strings.Contains(got.HoldReason, "scope") {
		t.Errorf("hold reason does not name the scope loop: %q", got.HoldReason)
	}
	if n := len(e.promptsFor(j.ID, SScoping)); n != 0 {
		t.Errorf("the agent ran %d time(s) with the budget already spent", n)
	}
}

// Like the planner and the reviewers, the scoping agent must leave the tree
// clean; a source edit is discarded rather than carried into planning.
func TestScopingDiscardsTreeEdits(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_SCOPE_EDIT", "1")
	j := e.submit("non-breaking space in SMS amounts")

	e.runEngine()

	wt := filepath.Join(e.repo, ".worktrees", j.ID)
	if out := git(t, wt, "status", "--porcelain"); strings.Contains(out, "src/app.txt") {
		t.Errorf("scoping left an edit in the tree: %q", out)
	}
}

// Approving with questions outstanding is a waiver, and a waiver that leaves
// no trace is the silent decision this pipeline exists to prevent.
func TestScopeApprovalWaivesOpenQuestions(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_SCOPE_CLARITY", "blocked")
	j := e.submit("fix the thing")
	e.runEngine()
	if got := e.jobState(j.ID); got.State != SAwaitScope {
		t.Fatalf("state=%s, want %s", got.State, SAwaitScope)
	}

	if err := e.st.AddApproval(&store.Approval{JobID: j.ID, Gate: "scope", Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	// The agent would block again on a second scoping run; approval must not
	// send it back there.
	t.Setenv("FAKE_SCOPE_CLARITY", "clear")
	e.runEngine()

	got := e.jobState(j.ID)
	if got.State == SAwaitScope || got.State == SScoping {
		t.Fatalf("approval did not move the job past scoping: state=%s", got.State)
	}
	if n := len(e.promptsFor(j.ID, SScoping)); n != 1 {
		t.Errorf("SCOPING ran %d time(s); approval must not re-scope", n)
	}
	if d := e.eventDetails(j.ID); !strings.Contains(d, "scope_questions_waived") || !strings.Contains(d, "Q1") {
		t.Error("the waived questions were not recorded in the event log")
	}
}

// Rejection carries the operator's answers back into a fresh scoping round.
func TestScopeRejectionRescopesWithTheAnswers(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_MARKER_DIR", t.TempDir())
	t.Setenv("FAKE_SCOPE_BLOCK_ONCE", "1")
	j := e.submit("fix the thing")
	e.runEngine()
	if got := e.jobState(j.ID); got.State != SAwaitScope {
		t.Fatalf("state=%s, want %s", got.State, SAwaitScope)
	}

	const answer = "Q1: only the SMS parser."
	if err := e.st.AddApproval(&store.Approval{
		JobID: j.ID, Gate: "scope", Decision: "reject", Reason: answer,
	}); err != nil {
		t.Fatal(err)
	}
	e.runEngine()

	got := e.jobState(j.ID)
	if got.Counters.ScopeRounds != 1 {
		t.Errorf("scope_rounds=%d, want 1", got.Counters.ScopeRounds)
	}
	if got.Counters.HumanRejectReason != "" {
		t.Error("the rejection reason was not cleared after the round consumed it")
	}
	prompts := e.promptsFor(j.ID, SScoping)
	if len(prompts) != 2 {
		t.Fatalf("SCOPING ran %d time(s), want 2", len(prompts))
	}
	if !strings.Contains(prompts[1], answer) {
		t.Errorf("the second scoping prompt does not carry the answers:\n%s", prompts[1])
	}
	if got.State == SAwaitScope || got.State == SScoping {
		t.Errorf("the re-scoped job did not move on: state=%s", got.State)
	}
}

// --cancel at the scope gate ends the job, as it does at the spec gate.
func TestScopeRejectionWithCancelEndsTheJob(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_SCOPE_CLARITY", "blocked")
	j := e.submit("fix the thing")
	e.runEngine()

	if err := e.st.AddApproval(&store.Approval{
		JobID: j.ID, Gate: "scope", Decision: "reject", Reason: "not worth doing", Cancel: true,
	}); err != nil {
		t.Fatal(err)
	}
	e.runEngine()

	if got := e.jobState(j.ID); got.State != SCancelled {
		t.Fatalf("state=%s, want %s", got.State, SCancelled)
	}
}

// Planning and design review receive problem.json in .sdlc/context, but only
// when scoping ran: with scoping off there is nothing to stage and the context
// directory must not carry a stale problem from a prior run.
func TestProblemContextIsStagedForPlanningAndDesignReview(t *testing.T) {
	e := newEnv(t)
	j := e.submit("fix SMS parser")

	e.runEngine()

	// The engine ran through to the merge gate, so planning and design review
	// both executed. Assert the artifact exists in the job's artifact store:
	if !e.artifactExists(j.ID, "problem.json") {
		t.Fatal("problem.json not in artifacts")
	}

	// Verify the planning prompt inlined the scoped problem:
	prompts := e.promptsFor(j.ID, SPlanning)
	if len(prompts) == 0 {
		t.Fatal("no planning prompts found")
	}
	if !strings.Contains(prompts[0], "# Scoped problem") || !strings.Contains(prompts[0], "problem/1") {
		t.Errorf("planning prompt does not inline problem.json:\n%s", prompts[0])
	}
}

func TestProblemContextOmittedWhenScopingDisabled(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.Scoping = "off"
	j := e.submit("fix SMS parser")

	e.runEngine()

	if e.artifactExists(j.ID, "problem.json") {
		t.Fatal("problem.json must not exist in artifacts when scoping is off")
	}

	prompts := e.promptsFor(j.ID, SPlanning)
	if len(prompts) == 0 {
		t.Fatal("no planning prompts found")
	}
	if strings.Contains(prompts[0], "# Scoped problem") {
		t.Errorf("planning prompt must not contain '# Scoped problem' when scoping is off:\n%s", prompts[0])
	}
}
