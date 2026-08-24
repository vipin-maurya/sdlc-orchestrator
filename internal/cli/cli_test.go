package cli

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vipinm/sdlc-orchestrator/internal/review"
)

// Go's flag package stops parsing at the first non-flag token. Every command
// here takes its job id positionally, so `sdlc resume JOB-1 --to BUILDING`
// left --to unparsed and silently ignored: the engine fell back to the
// previous state and resumed the job straight back into the state that had
// just escalated. Both orderings must produce the same result, and anything
// left over must be an error rather than something quietly dropped.
func TestParseArgsAcceptsFlagsInEitherPosition(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"flags after the positional", []string{"JOB-1", "--to", "BUILDING", "--note", "do it"}},
		{"flags before the positional", []string{"--to", "BUILDING", "--note", "do it", "JOB-1"}},
		{"flags on both sides", []string{"--to", "BUILDING", "JOB-1", "--note", "do it"}},
		{"single-dash form", []string{"JOB-1", "-to", "BUILDING", "-note", "do it"}},
		{"equals form", []string{"JOB-1", "--to=BUILDING", "--note=do it"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("resume", flag.ContinueOnError)
			fs.SetOutput(io_Discard{})
			to := fs.String("to", "", "")
			note := fs.String("note", "", "")
			pos, err := parseArgs(fs, tc.args, 1, "usage")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(pos) != 1 || pos[0] != "JOB-1" {
				t.Errorf("positional = %v, want [JOB-1]", pos)
			}
			if *to != "BUILDING" {
				t.Errorf("--to = %q, want BUILDING", *to)
			}
			if *note != "do it" {
				t.Errorf("--note = %q, want %q", *note, "do it")
			}
		})
	}
}

// A stray argument is a typo, not a thing to ignore. Ignoring it is how the
// original bug stayed invisible for a whole run.
func TestParseArgsRejectsExtraPositionals(t *testing.T) {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(io_Discard{})
	fs.String("to", "", "")
	_, err := parseArgs(fs, []string{"JOB-1", "JOB-2", "--to", "FIXING"}, 1, "sdlc resume <JOB-ID>")
	if err == nil {
		t.Fatal("extra positional accepted")
	}
	if !strings.Contains(err.Error(), "JOB-2") {
		t.Errorf("error does not name the stray argument: %v", err)
	}
}

func TestParseArgsRequiresThePositional(t *testing.T) {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(io_Discard{})
	fs.String("to", "", "")
	if _, err := parseArgs(fs, []string{"--to", "FIXING"}, 1, "sdlc resume <JOB-ID>"); err == nil {
		t.Fatal("missing positional accepted")
	}
}

// An unknown flag must fail rather than being swallowed as a positional.
func TestParseArgsRejectsUnknownFlags(t *testing.T) {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(io_Discard{})
	fs.String("to", "", "")
	if _, err := parseArgs(fs, []string{"JOB-1", "--nope", "x"}, 1, "usage"); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

type io_Discard struct{}

func (io_Discard) Write(p []byte) (int, error) { return len(p), nil }

// `sdlc review` is the first command whose positional argument is optional:
// with an id it decides one job, without one it walks everything waiting.
// parseArgsRange is what parseArgs became so that could work, so the bound it
// applies at each end is what needs holding — the four tests above prove the
// exact-count case still behaves, and this one proves the range case does.
func TestParseArgsRangeAcceptsZeroOrOne(t *testing.T) {
	const usage = "sdlc review [JOB-ID] [--diff] [--no-prompt]"
	cases := []struct {
		name    string
		args    []string
		wantPos []string
		wantErr string
	}{
		{"no positional at all", []string{}, nil, ""},
		{"flags only", []string{"--diff"}, nil, ""},
		{"one positional", []string{"JOB-1"}, []string{"JOB-1"}, ""},
		{"positional then flag", []string{"JOB-1", "--diff"}, []string{"JOB-1"}, ""},
		{"flag then positional", []string{"--diff", "JOB-1"}, []string{"JOB-1"}, ""},
		{"two positionals", []string{"JOB-1", "JOB-2"}, nil, "JOB-2"},
		{"unknown flag", []string{"JOB-1", "--nope"}, nil, "nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("review", flag.ContinueOnError)
			fs.SetOutput(io_Discard{})
			diff := fs.Bool("diff", false, "")
			pos, err := parseArgsRange(fs, tc.args, 0, 1, usage)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseArgsRange(%v) = %v, want an error naming %q", tc.args, pos, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %v does not name %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgsRange(%v): %v", tc.args, err)
			}
			if strings.Join(pos, ",") != strings.Join(tc.wantPos, ",") {
				t.Errorf("positional = %v, want %v", pos, tc.wantPos)
			}
			if wantDiff := contains(tc.args, "--diff"); *diff != wantDiff {
				t.Errorf("--diff = %v, want %v", *diff, wantDiff)
			}
		})
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// --- submit ---------------------------------------------------------------

// `sdlc submit` delegates to jobs.Submit so the CLI and the web form cannot
// disagree about what a valid submission is. The delegation changes which code
// produces each refusal, so the lines an operator (and any script reading an
// exit code) depends on are pinned here. There was no such test before, which
// is exactly how the two copies drifted.

// An omitted --target is a usage error, not a failure: jobs.Submit reports it
// as an unknown target, which would be exit 1.
func TestSubmitEmptyTargetIsAUsageError(t *testing.T) {
	e := newReviewEnv(t)
	var code int
	stderr := captureStderr(t, func() { code = cmdSubmit(e.cfg, []string{"--title", "anything"}) })
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if got := strings.TrimRight(stderr, "\n"); got != "error: --target is required" {
		t.Errorf("stderr = %q, want %q", got, "error: --target is required")
	}
}

// The missing-title line names the two flags that supply one, and exits 2.
// jobs.ErrNoTitle says "a title is required" — right for the web form, which
// has no flags — and this is where that sentinel becomes this command's words.
func TestSubmitMissingTitleMessageAndExitCode(t *testing.T) {
	e := newReviewEnv(t)
	const want = "error: --title (or --file with a heading) is required"
	empty := filepath.Join(t.TempDir(), "issue.md")
	if err := os.WriteFile(empty, []byte("   \n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
	}{
		{"no title at all", []string{"--target", "demo"}},
		{"body but no title", []string{"--target", "demo", "--body", "it is broken"}},
		{"whitespace title", []string{"--target", "demo", "--title", "   "}},
		{"file with no heading", []string{"--target", "demo", "--file", empty}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			stderr := captureStderr(t, func() { code = cmdSubmit(e.cfg, tc.args) })
			if code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if got := strings.TrimRight(stderr, "\n"); got != want {
				t.Errorf("stderr = %q, want %q", got, want)
			}
		})
	}
}

// The size cap and the control-character rule arrived with jobs.Submit and now
// apply to the CLI too. Each must surface as one sentence naming what was
// wrong — not as a panic, and not as a job row with an escape sequence in the
// title that an approver's terminal will interpret.
func TestSubmitRefusesAnUnprintableOrOversizedTitle(t *testing.T) {
	e := newReviewEnv(t)
	big := strings.Repeat("x", 501)
	cases := []struct {
		name, title, body, want string
	}{
		{"escape sequence in the title", "fix \x1b[31mthis\x1b[0m", "", "control characters"},
		{"newline in the title", "line one\nline two", "", "control characters"},
		{"title over the cap", big, "", "at most 500 bytes"},
		{"NUL in the body", "fine", "before\x00after", "NUL byte"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			stderr := captureStderr(t, func() {
				code = cmdSubmit(e.cfg, []string{"--target", "demo", "--title", tc.title, "--body", tc.body})
			})
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tc.want)
			}
			if !strings.HasPrefix(stderr, "error: ") || strings.Contains(stderr, "goroutine") {
				t.Errorf("not a clean one-line error: %q", stderr)
			}
		})
	}
	if jobs, err := e.st.ListJobs(); err != nil {
		t.Fatal(err)
	} else if len(jobs) != 0 {
		t.Errorf("%d job(s) created by refused submissions", len(jobs))
	}
}

// An unknown target keeps the failure exit and jobs.Submit's sentence, which
// lists the targets that do exist.
func TestSubmitUnknownTargetIsAFailureExit(t *testing.T) {
	e := newReviewEnv(t)
	var code int
	stderr := captureStderr(t, func() {
		code = cmdSubmit(e.cfg, []string{"--target", "nope", "--title", "x"})
	})
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "nope") {
		t.Errorf("stderr = %q, want it to name the target", stderr)
	}
}

// The happy path goes through jobs.Submit: the row it writes carries the
// trimmed title, the branch derived from the target's prefix, and a body that
// falls back to the title.
func TestSubmitCreatesTheJobThroughJobsSubmit(t *testing.T) {
	e := newReviewEnv(t)
	if code := cmdSubmit(e.cfg, []string{"--target", "demo", "--title", "  Fix the thing  "}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	all, err := e.st.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("%d jobs, want 1", len(all))
	}
	j := all[0]
	if j.IssueTitle != "Fix the thing" {
		t.Errorf("title = %q, want %q", j.IssueTitle, "Fix the thing")
	}
	if j.IssueBody != "Fix the thing" {
		t.Errorf("body = %q, want it to fall back to the title", j.IssueBody)
	}
	if j.Branch != "sdlc/"+j.ID || j.State != "CREATED" {
		t.Errorf("branch/state = %q/%q", j.Branch, j.State)
	}
}

// --file with no --title takes the first line as the title, stripped of its
// heading hashes however many there are — the CLI's old copy stripped exactly
// one, so "## Fix it" became a job titled "# Fix it".
func TestSubmitFileHeadingBecomesTheTitle(t *testing.T) {
	e := newReviewEnv(t)
	p := filepath.Join(t.TempDir(), "issue.md")
	if err := os.WriteFile(p, []byte("## Fix it\n\nthe details\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := cmdSubmit(e.cfg, []string{"--target", "demo", "--file", p}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	all, err := e.st.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].IssueTitle != "Fix it" {
		t.Fatalf("title = %q, want %q", all[0].IssueTitle, "Fix it")
	}
	if all[0].IssueBody != "the details" {
		t.Errorf("body = %q, want %q", all[0].IssueBody, "the details")
	}
}

// jobs.Submit is only the shared path if it is the only path. A second caller
// of store.CreateJob is a second set of rules about titles, branches and
// bodies, which is the state this delegation was undoing.
func TestCreateJobHasOneNonTestCaller(t *testing.T) {
	var callers []string
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, ln := range strings.Split(string(src), "\n") {
			if strings.Contains(ln, ".CreateJob(") {
				callers = append(callers, path+": "+strings.TrimSpace(ln))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 1 || !strings.Contains(callers[0], filepath.Join("internal", "jobs", "submit.go")) {
		t.Errorf("store.CreateJob callers = %v, want exactly internal/jobs/submit.go", callers)
	}
}

// One pending decision per gate, at the terminal as well as in the browser.
//
// `sdlc reject` typed a second time — because the first seemed not to take, or
// because the operator changed their mind — used to write a second row, and
// PendingApproval hands the engine the oldest. Every gate here is re-enterable
// (a rejected spec re-parks at the spec gate), so the spare was consumed on the
// job's next visit as a decision nobody typed, against a job that had moved on.
func TestSecondDecisionAtTheSameGateIsRefused(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("AWAITING_SPEC_APPROVAL", "spec is rejected")

	var code int
	captureStderr(t, func() {
		code = cmdDecision(e.cfg, []string{j.ID, "--reason", "use the existing FooCache"}, "reject")
	})
	if code != 0 {
		t.Fatalf("the first reject exited %d, want 0", code)
	}

	stderr := captureStderr(t, func() {
		code = cmdDecision(e.cfg, []string{j.ID, "--reason", "on second thoughts, cancel it"}, "reject")
	})
	if code == 0 {
		t.Error("the second reject was accepted; the operator now has two answers queued")
	}
	// The operator has to be told which decision is in the way, and that the
	// engine has simply not got to it yet — otherwise the obvious next move is
	// to type it a third time.
	for _, want := range []string{"already pending", "next tick", "sdlc review " + j.ID} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
		}
	}
	// The first answer is the one that survives, unedited.
	a, err := e.st.PendingApproval(j.ID, "spec")
	if err != nil || a == nil {
		t.Fatalf("pending decision: %v %v", a, err)
	}
	if a.Reason != "use the existing FooCache" {
		t.Errorf("pending reason = %q, want the first decision's", a.Reason)
	}
}

// The same rule for the control rows, which are written by a different
// function and so would otherwise need the guard remembered twice.
func TestSecondControlRequestIsRefused(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("HELD", "held for a human")

	var code int
	captureStderr(t, func() { code = cmdControl(e.cfg, []string{j.ID}, "cancel") })
	if code != 0 {
		t.Fatalf("the first cancel exited %d, want 0", code)
	}
	stderr := captureStderr(t, func() { code = cmdControl(e.cfg, []string{j.ID}, "cancel") })
	if code == 0 {
		t.Error("the second cancel was accepted")
	}
	if !strings.Contains(stderr, "already") {
		t.Errorf("stderr does not say a cancel is already queued:\n%s", stderr)
	}
}

// A2: reject already requires a reason on every gate, and the new one inherits
// that for free through review.GateFor. The scope gate's reason IS the
// operator's answers, so an empty one would hand the next scoping round
// nothing to act on. Pinned here because the rule is inherited rather than
// written, and inherited behaviour is what silently stops applying.
func TestRejectAtScopeGateRequiresAReason(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("AWAITING_SCOPE_APPROVAL", "fix the thing")

	var code int
	stderr := captureStderr(t, func() {
		code = cmdDecision(e.cfg, []string{j.ID}, "reject")
	})
	if code == 0 {
		t.Error("reject with no --reason was accepted at the scope gate")
	}
	if !strings.Contains(stderr, "--reason") {
		t.Errorf("stderr does not say what is missing:\n%s", stderr)
	}

	captureStderr(t, func() {
		code = cmdDecision(e.cfg, []string{j.ID, "--reason", "Q1: only the SMS parser"}, "reject")
	})
	if code != 0 {
		t.Fatalf("reject with a reason exited %d at the scope gate, want 0", code)
	}
	a, err := e.st.PendingApproval(j.ID, review.GateScope)
	if err != nil || a == nil {
		t.Fatalf("no pending row at the scope gate: %v %v", a, err)
	}
	if a.Reason != "Q1: only the SMS parser" {
		t.Errorf("reason=%q, want the operator's answers verbatim", a.Reason)
	}
}

func TestApproveAtScopeGateWritesTheScopeRow(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("AWAITING_SCOPE_APPROVAL", "fix the thing")

	var code int
	captureStderr(t, func() { code = cmdDecision(e.cfg, []string{j.ID}, "approve") })
	if code != 0 {
		t.Fatalf("approve exited %d at the scope gate, want 0", code)
	}
	if a, err := e.st.PendingApproval(j.ID, review.GateScope); err != nil || a == nil {
		t.Fatalf("approve did not write a row under the scope gate: %v %v", a, err)
	}
}

func TestStatusRendersTheScopeGateAndClarification(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("AWAITING_SCOPE_APPROVAL", "fix the thing")

	stdout := captureStdout(t, func() { cmdStatus(e.cfg, []string{j.ID}) })
	for _, want := range []string{"scope_rounds=0", "approve the scoped problem before a spec is written", "sdlc review " + j.ID} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status output is missing %q:\n%s", want, stdout)
		}
	}
}
