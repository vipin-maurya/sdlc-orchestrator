package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// fakeDiffSource stands in for a repository so renderDiff's branches can be
// pinned without git on the machine. The golden fixture is deliberately
// git-free, which left every branch past the worktree check unpinned — and a
// truncation-labelling defect landed in exactly that gap.
type fakeDiffSource struct {
	stat      string
	statErr   error
	patch     string
	truncated bool
	patchErr  error
}

func (f fakeDiffSource) DiffStatSince(context.Context, string, string) (string, error) {
	return f.stat, f.statErr
}

func (f fakeDiffSource) DiffPatchSince(context.Context, string, string) (string, bool, error) {
	return f.patch, f.truncated, f.patchErr
}

// withDiffSource swaps the seam for one test. Not parallel-safe by
// construction, which is why no test here calls t.Parallel.
func withDiffSource(t *testing.T, f diffSource) {
	t.Helper()
	prev := openDiffSource
	openDiffSource = func(string) diffSource { return f }
	t.Cleanup(func() { openDiffSource = prev })
}

// diffOptions builds a job whose worktree exists, so renderDiff gets past the
// worktree check and reaches the source.
func diffOptions(t *testing.T, full bool) Options {
	t.Helper()
	dir := t.TempDir()
	wt := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	job := &store.Job{ID: "JOB-1", IssueTitle: "Something", Branch: "sdlc/JOB-1", WorktreePath: wt}
	job.Counters.BaseSHA = "abcdef0123456789abcdef"
	return Options{
		Job: job, Gate: GateMerge, DataDir: filepath.Join(dir, "data"),
		RepoPath: dir, DefaultBranch: "main", FullDiff: full,
	}
}

// TestTruncatedPatchIsLabelledInEveryPlaceItSurfaces pins the three surfaces a
// clipped patch reaches. Before this, Doc.DiffTruncated was written and read by
// nothing, and the document called a file clipped mid-hunk "Full patch".
func TestTruncatedPatchIsLabelledInEveryPlaceItSurfaces(t *testing.T) {
	for _, full := range []bool{false, true} {
		name := "summary"
		if full {
			name = "inline"
		}
		t.Run(name, func(t *testing.T) {
			withDiffSource(t, fakeDiffSource{stat: " a.go | 2 +-\n", patch: "diff --git a/a.go b/a.go\n+x\n", truncated: true})
			o := diffOptions(t, full)
			doc, path, err := Write(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if !doc.DiffTruncated {
				t.Fatal("DiffTruncated is false for a truncated patch")
			}
			// The document must not promise a head it never prints.
			if !full && strings.Contains(doc.Body, "what follows is its head") {
				t.Error("document promises a head that --diff off never prints")
			}
			if !strings.Contains(doc.Body, "only its head was captured") {
				t.Errorf("no truncation notice in the document:\n%s", doc.Body)
			}
			if !strings.Contains(doc.Body, "(truncated at the 4 MiB capture limit)") {
				t.Error(`the "Full patch:" line does not say the patch is short`)
			}
			// The artifact has to say what happened to it in its own bytes: a
			// patch clipped mid-hunk looks complete to whatever opens it next.
			b, err := os.ReadFile(DiffPath(o.DataDir, o.Job.ID, doc.Gate))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), "[sdlc] This patch stops here") {
				t.Errorf("the .diff artifact carries no truncation trailer:\n%s", b)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestDiffSectionRendersWithoutTruncation is the negative half: nothing may
// claim truncation when the capture was complete. A label that cries wolf
// trains a reviewer to ignore the one signal that has to be trustworthy.
func TestDiffSectionRendersWithoutTruncation(t *testing.T) {
	withDiffSource(t, fakeDiffSource{stat: " a.go | 2 +-\n", patch: "diff --git a/a.go b/a.go\n+x\n"})
	o := diffOptions(t, true)
	doc, _, err := Write(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if doc.DiffTruncated {
		t.Fatal("DiffTruncated is true for a complete patch")
	}
	for _, bad := range []string{"capture limit", "truncated", "stops here"} {
		if strings.Contains(doc.Body, bad) {
			t.Errorf("complete patch is described with %q:\n%s", bad, doc.Body)
		}
	}
	if !strings.Contains(doc.Body, "## Diff vs base") {
		t.Error("no diff section")
	}
	// The ```diff fence is the only use of BlockCode.Lang anywhere.
	if !strings.Contains(doc.Body, "```diff\n") {
		t.Errorf("--diff did not render a diff-tagged fence:\n%s", doc.Body)
	}
	b, err := os.ReadFile(DiffPath(o.DataDir, o.Job.ID, doc.Gate))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "[sdlc]") {
		t.Error("a complete .diff artifact carries a truncation trailer")
	}
}

// TestDiffSectionReportsAnUnavailableStat pins the branch where the repository
// answers with an error rather than a diff.
func TestDiffSectionReportsAnUnavailableStat(t *testing.T) {
	withDiffSource(t, fakeDiffSource{statErr: os.ErrPermission})
	doc, _, err := Write(context.Background(), diffOptions(t, false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Body, "unavailable") {
		t.Errorf("a failed diff stat is not reported:\n%s", doc.Body)
	}
	if doc.Diff != "" {
		t.Error("Diff is set although the stat failed")
	}
}

// TestWriteKeepsBodyAFunctionOfBlocks is what lets Write append one block's
// markdown instead of re-rendering the document. renderMarkdown is a
// concatenation of per-block strings, so the two are the same bytes — until
// somebody makes it stateful, at which point the browser and the terminal
// would start disagreeing about whether the patch is on disk. This is the test
// that would catch that.
func TestWriteKeepsBodyAFunctionOfBlocks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		truncated bool
		full      bool
	}{
		{"complete summary", false, false},
		{"complete inline", false, true},
		{"truncated summary", true, false},
		{"truncated inline", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withDiffSource(t, fakeDiffSource{stat: " a.go | 2 +-\n", patch: "diff --git a/a.go b/a.go\n+x\n", truncated: tc.truncated})
			doc, _, err := Write(context.Background(), diffOptions(t, tc.full))
			if err != nil {
				t.Fatal(err)
			}
			if want := renderMarkdown(doc.Blocks); doc.Body != want {
				t.Errorf("Body is not renderMarkdown(Blocks)\n got: %q\nwant: %q", doc.Body, want)
			}
		})
	}
}
