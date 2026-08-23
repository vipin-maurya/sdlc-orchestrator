package review

// The gate document is the thing a human reads before they type "approve".
// These tests hold its bytes still.
//
// Render is time-, path- and timezone-dependent, and each of those is pinned
// here rather than normalized away afterwards: a fixture that renders the same
// text in every zone is a golden that means something, while a comparison that
// scrubs the output until it matches is a golden that passes no matter what
// the renderer did.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// -update-golden rewrites the expected documents. Reach for it only when a
// deliberate change to the document's wording is what you are landing. If it
// is the block-model refactor that made the goldens differ, the difference IS
// the bug: the whole point of the refactor is that the terminal's words do not
// move, and a regenerated golden hides exactly the drift this file is here to
// catch.
var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden/expected")

// The sentence the failure message repeats, so the next reader meets it before
// they meet the flag.
const regeneratingHidesTheDrift = "If it is the block-model refactor that made the goldens differ, the difference IS the bug: " +
	"the whole point of the refactor is that the terminal's words do not move, and a regenerated golden " +
	"hides exactly the drift this file is here to catch."

const (
	// goldenDataDir is a literal relative path, not t.TempDir(), because the
	// artifacts line prints it: a temp directory would put a different string
	// in the document on every run.
	goldenDataDir = "testdata/golden"
	// goldenWorktree must not exist. renderDiff's "worktree is gone" branch is
	// the only one that reaches no git subprocess, so this is what makes the
	// diff section deterministic without pinning the machine's git as well.
	goldenWorktree = "testdata/golden/worktree-gone"
	// goldenRepo is never opened — the worktree check fails first — but it is
	// named so nothing here can silently start depending on the real repo.
	goldenRepo = "testdata/golden/repo-gone"

	goldenBaseSHA = "9f1c0d3a7b5e2148c6a0d9f3b71e4c825a6d0f39"
)

// TestMarkdownGoldenUnchanged renders all five gates and compares Doc.Body
// byte for byte against the committed documents.
func TestMarkdownGoldenUnchanged(t *testing.T) {
	for _, gate := range []string{GateSpec, GateCode, GateMerge, GateRelease, GateHold} {
		t.Run(gate, func(t *testing.T) {
			doc, err := Render(context.Background(), goldenOptions(gate))
			if err != nil {
				t.Fatalf("Render(%s): %v", gate, err)
			}
			// The worktree is gone, so no patch was produced. If this ever
			// fails the fixture has started shelling out to git and the
			// document is no longer reproducible.
			if doc.Diff != "" {
				t.Fatalf("the fixture produced a patch (%d bytes); %s must not exist", len(doc.Diff), goldenWorktree)
			}

			path := filepath.Join("testdata", "golden", "expected", gate+".md")
			got := toSlash(doc.Body)
			if *updateGolden {
				// The normalized form is what gets committed, so a golden
				// written on Windows and one written on Linux are the same
				// file rather than a separator-only diff.
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("rewrote %s", path)
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading the golden document: %v", err)
			}
			if got != toSlash(string(want)) {
				t.Errorf("the %s gate document changed.\n\n%s\n\n%s\n\nRun with -update-golden only if you meant to change the wording.",
					gate, firstDifference(got, toSlash(string(want))), regeneratingHidesTheDrift)
			}
		})
	}
}

// toSlash applies the one substitution this comparison is permitted, to both
// sides. The artifacts line is built with filepath.Join, which separates with
// '\' on Windows, so the same correct document differs by separator alone
// between platforms. Nothing else is normalized — no trimming, no whitespace
// collapsing, no regexps — because every other byte of difference is the drift
// this test exists to catch.
func toSlash(s string) string { return strings.ReplaceAll(s, "\\", "/") }

// firstDifference names where the two documents part company. A diff of two
// 200-line markdown files is unreadable in test output; the first line that
// moved is almost always the whole story.
func firstDifference(got, want string) string {
	g := strings.Split(got, "\n")
	w := strings.Split(want, "\n")
	for i := 0; i < len(g) && i < len(w); i++ {
		if g[i] != w[i] {
			return fmt.Sprintf("first difference at line %d:\n  golden:   %q\n  rendered: %q", i+1, w[i], g[i])
		}
	}
	return fmt.Sprintf("the first %d lines agree; the golden has %d lines and the render has %d",
		min(len(g), len(w)), len(w), len(g))
}

// --- the fixture ----------------------------------------------------------

func goldenOptions(gate string) Options {
	job := &store.Job{
		ID:           "JOB-1",
		Target:       "demo",
		IssueTitle:   "Redact tokens before they reach the job log",
		IssueBody:    goldenIssueBody(),
		Branch:       "sdlc/JOB-1",
		WorktreePath: goldenWorktree,
		State:        goldenState(gate),
		// humanSince reads time.Since, so no fixed instant can be used here.
		// 90 minutes renders "1h" anywhere between 60 and 119 minutes: an hour
		// of slack either side of the boundary, which is more than any test
		// run needs.
		StateEnteredAt: time.Now().Add(-90 * time.Minute),
		HoldReason: "Three fix attempts did not make the unit tests pass. " +
			"The last analysis classified the failure as `environment`, which this loop cannot fix on its own.",
		// A base sha is required or renderDiff returns before it can say the
		// worktree is gone, and the Diff section would vanish from the golden.
		Counters: store.Counters{BaseSHA: goldenBaseSHA, CodeReviewRounds: 2, FixAttempts: 3},
	}
	o := Options{
		Job:           job,
		Gate:          gate,
		DataDir:       goldenDataDir,
		RepoPath:      goldenRepo,
		DefaultBranch: "main",
		ShipCommand:   []string{"scripts/ship.sh", "--tag", "v1.4.0"},
	}
	if gate == GateHold {
		o.Events = goldenEvents()
	}
	return o
}

// goldenState keeps the state in the facts list agreeable with the gate being
// rendered. Render would accept any state once Gate is set explicitly, and a
// document headed "approve the spec" that says the job is ESCALATED would be
// a fixture teaching the reader something untrue.
func goldenState(gate string) string {
	switch gate {
	case GateSpec:
		return "AWAITING_SPEC_APPROVAL"
	case GateCode:
		return "AWAITING_CODE_APPROVAL"
	case GateMerge:
		return "AWAITING_MERGE_APPROVAL"
	case GateRelease:
		return "AWAITING_RELEASE_APPROVAL"
	default:
		return "ESCALATED"
	}
}

// goldenIssueBody deliberately runs past quote()'s 40-line limit: the pasted
// log tail is what real issue bodies look like, and it is the only way the
// golden gets to pin the truncation marker. A renderer that quietly stopped
// truncating would paste an entire log into the terminal at the first gate.
func goldenIssueBody() string {
	var b strings.Builder
	b.WriteString("A token pasted into an issue body is written to the job log verbatim.\n")
	b.WriteString("From the run on 2026-01-02, every one of these lines reached logs/agent.log:\n")
	b.WriteString("\n")
	for i := 1; i <= 38; i++ {
		fmt.Fprintf(&b, "  line %02d — captured agent output, copied through unchanged\n", i)
	}
	return b.String()
}

// goldenEvents gives the hold document more events than it shows, so the
// golden pins the 12-event tail as well as the formatting of each line.
func goldenEvents() []*store.Event {
	// Every event carries the same instant, built in time.Local. renderHold
	// formats e.CreatedAt.Local(), so a time built in any other location would
	// render a different wall clock under TZ=UTC than under TZ=Asia/Kolkata;
	// built this way it is 03:04:05 in every zone.
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	mk := func(state, kind, detail string) *store.Event {
		return &store.Event{JobID: "JOB-1", State: state, Kind: kind, Detail: detail, CreatedAt: at}
	}
	return []*store.Event{
		mk("PLANNING", "enter", "DROPPED: only the last 12 events are shown"),
		mk("PLANNING", "agent_run", "DROPPED: only the last 12 events are shown"),
		mk("AWAITING_SPEC_APPROVAL", "enter", "parked for the spec gate"),
		mk("AWAITING_SPEC_APPROVAL", "approval", "approve — start with the pattern table"),
		mk("IMPLEMENTING", "enter", ""),
		mk("IMPLEMENTING", "agent_run", "implementer wrote implementation.json"),
		mk("BUILDING", "exec_run", "go build ./... (exit 0)"),
		// Longer than oneLine's 90 runes, so the golden pins the ellipsis too.
		mk("TESTING", "exec_run", "go test ./... (exit 1): --- FAIL: TestRedactsAToken (0.00s) redact_test.go:41: got \"ghp_realtokenvalue\", want \"[redacted]\""),
		mk("ANALYZING", "enter", "classified as environment"),
		// A detail with an embedded newline: renderHold puts events in a fence,
		// so a second line would misalign every column after it.
		mk("FIXING", "agent_run", "fixer ran with the analysis as its instruction\nand changed nothing"),
		mk("BUILDING", "exec_run", "go test ./... (exit 1)"),
		mk("ESCALATED", "error", "fix attempts exhausted (3 of 3)"),
		mk("ESCALATED", "enter", "waiting for a human"),
		mk("ESCALATED", "note", "the sandbox has no network; the failing test fetches a module"),
	}
}

// --- the patch line -------------------------------------------------------

// TestFullPatchLineIsExactBytes pins the bytes Write appends once it has put
// the patch on disk. W1-E rebuilds that line out of typed spans, and
// "\nFull patch: `<path>`\n" is the target: a dropped newline or a lost pair of
// backticks reads as nothing at all in a rendered view and as a broken
// document in the terminal.
//
// Stated plainly, because the plan says this test needs no git and half of it
// does: Doc.Diff is only ever set from gitx.DiffPatchSince, and Write builds
// its own Doc by calling Render, so there is no seam through which a test can
// hand Write a Doc that already carries a patch. Asserting the format string
// against a locally rebuilt copy of the same format string would pin nothing
// at all. The "no patch" half below therefore runs everywhere and holds Write
// to leaving Body alone; the "with a patch" half builds a real repository in a
// temp dir and skips where git is not installed, rather than reporting a check
// it did not make.
func TestFullPatchLineIsExactBytes(t *testing.T) {
	ctx := context.Background()

	t.Run("no patch", func(t *testing.T) {
		data := t.TempDir()
		o := patchLineOptions(t, filepath.Join(data, "worktree-gone"), "", data)
		rendered, err := Render(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		doc, docPath, err := Write(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		if doc.Body != rendered.Body {
			t.Errorf("Write changed Body with no patch to name:\n%s", firstDifference(doc.Body, rendered.Body))
		}
		onDisk, err := os.ReadFile(docPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(onDisk) != rendered.Body {
			t.Errorf("the written document differs from the rendered one:\n%s", firstDifference(string(onDisk), rendered.Body))
		}
	})

	t.Run("with a patch", func(t *testing.T) {
		git, err := exec.LookPath("git")
		if err != nil {
			t.Skip("git is not installed, so no patch can be produced and the appended line cannot be observed")
		}
		worktree, base := goldenPatchRepo(t, git)
		data := t.TempDir()
		o := patchLineOptions(t, worktree, base, data)

		// Render is called separately to get the document without the line;
		// Write renders the same Options again. Both agree because the fixture
		// pins the only two moving parts — the waiting time (90 minutes, so
		// "1h") and the patch (a repository nothing else is writing to).
		rendered, err := Render(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		if rendered.Diff == "" {
			t.Fatalf("the fixture repository produced no patch, so there is nothing to pin")
		}
		doc, docPath, err := Write(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		want := rendered.Body + "\nFull patch: `" + DiffPath(data, o.Job.ID, GateMerge) + "`\n"
		if doc.Body != want {
			t.Errorf("the appended patch line is not the expected bytes.\ngot  %q\nwant %q",
				strings.TrimPrefix(doc.Body, rendered.Body), strings.TrimPrefix(want, rendered.Body))
		}
		onDisk, err := os.ReadFile(docPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(onDisk) != want {
			t.Errorf("the written document does not carry the appended line:\n%s", firstDifference(string(onDisk), want))
		}
	})
}

func patchLineOptions(t *testing.T, worktree, base, dataDir string) Options {
	t.Helper()
	return Options{
		Job: &store.Job{
			ID:             "JOB-2",
			Target:         "demo",
			IssueTitle:     "Pin the patch line",
			IssueBody:      "the body is not what this test is about",
			Branch:         "sdlc/JOB-2",
			WorktreePath:   worktree,
			State:          "AWAITING_MERGE_APPROVAL",
			StateEnteredAt: time.Now().Add(-90 * time.Minute),
			Counters:       store.Counters{BaseSHA: base},
		},
		Gate:          GateMerge,
		DataDir:       dataDir,
		RepoPath:      worktree,
		DefaultBranch: "main",
	}
}

// goldenPatchRepo builds a repository with one commit and one uncommitted
// edit, which is exactly what `git diff <base>` needs to produce a patch.
func goldenPatchRepo(t *testing.T, git string) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	// An empty hooks directory, outside the worktree so it cannot turn up in
	// the patch. A global commit.gpgsign or a template pre-commit hook would
	// otherwise decide whether this test can run, and it is a real empty
	// directory rather than /dev/null so the flag also works on Windows, where
	// CI runs too.
	hooks := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(git, append([]string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + hooks}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	run("config", "user.email", "golden@example.invalid")
	run("config", "user.name", "golden")
	write("redact.go", "package execx\n\nfunc Line(s string) string { return s }\n")
	run("add", "redact.go")
	run("commit", "-q", "-m", "base")
	base = strings.TrimSpace(run("rev-parse", "HEAD"))
	write("redact.go", "package execx\n\nfunc Line(s string) string { return tokens.ReplaceAllString(s, \"[redacted]\") }\n")
	return dir, base
}
