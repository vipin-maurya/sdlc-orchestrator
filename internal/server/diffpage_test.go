package server

// Tests for the diff viewer. Owner: W2-K.
//
// Every patch here is a literal string rather than something git produced. A
// test that shells out to git tests git; what is under test is what this page
// does with a patch, and the patches that matter — a truncated capture, a
// binary file, a rename with no hunk, a line with no newline at the end — are
// each one string and each a nuisance to arrange for real.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// fakePatch is the patch source the tests install in place of git.
type fakePatch struct {
	patch     string
	truncated bool
	err       error
}

func (f fakePatch) DiffPatchSince(ctx context.Context, dir, sha string) (string, bool, error) {
	return f.patch, f.truncated, f.err
}

// withPatch points the page at a canned patch and gives the job a base sha and
// a worktree that exists, which are the two preconditions patchFor checks
// before it asks for a diff at all.
func withPatch(t *testing.T, e *env, j *store.Job, p fakePatch) {
	t.Helper()
	j.Counters.BaseSHA = "0123456789abcdef0123456789abcdef01234567"
	j.WorktreePath = e.tmp // any directory that exists
	if err := e.st.UpdateJob(j); err != nil {
		t.Fatal(err)
	}
	prev := openPatchSource
	openPatchSource = func(string) patchSource { return p }
	t.Cleanup(func() { openPatchSource = prev })
}

const smallPatch = `diff --git a/hello.go b/hello.go
index 1111111..2222222 100644
--- a/hello.go
+++ b/hello.go
@@ -1,4 +1,4 @@
 package main
 
-func hello() string { return "hi" }
+func hello() string { return "hello" }
`

// TestDiffPageRendersBothViews is A3: both views ship, both are rendered by the
// server from one parse, and each shows the change.
func TestDiffPageRendersBothViews(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "both views")
	withPatch(t, e, j, fakePatch{patch: smallPatch})

	for _, tc := range []struct {
		view string
		want []string
	}{
		{"unified", []string{`class="diff-view on" href="?view=unified"`, `return &#34;hi&#34;`, `return &#34;hello&#34;`}},
		{"split", []string{`class="diff-view on" href="?view=split"`, `<span class="mark">i</span>`, `<span class="mark">ello</span>`}},
	} {
		res := e.get("/jobs/" + j.ID + "/diff?view=" + tc.view)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("view=%s: status %d", tc.view, res.StatusCode)
		}
		body := e.body(res)
		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("view=%s: page does not contain %q", tc.view, want)
			}
		}
	}
}

// TestSplitViewMarksOnlyTheChangedSpan pins that the intra-line mark is made of
// template structure around escaped text, not of markup this package built. The
// two lines differ only in the word inside the quotes, so that is the only
// thing that may be marked.
func TestSplitViewMarksOnlyTheChangedSpan(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "marks")
	withPatch(t, e, j, fakePatch{patch: smallPatch})
	body := e.body(e.get("/jobs/" + j.ID + "/diff?view=split"))
	// The common prefix runs to the h the two words share, so the marked span
	// is "i" against "ello" — the smallest true statement about the change,
	// which is the point of computing it rather than lighting up the line.
	for _, want := range []string{
		`func hello() string { return &#34;h<span class="mark">i</span>&#34; }`,
		`func hello() string { return &#34;h<span class="mark">ello</span>&#34; }`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("split view does not contain %q", want)
		}
	}
}

// TestViewDefaultsFromCookieThenUnified is spec §5.1's precedence, in order:
// the query string beats the cookie, the cookie beats the default, and the
// default is unified.
func TestViewDefaultsFromCookieThenUnified(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "cookie")
	withPatch(t, e, j, fakePatch{patch: smallPatch})
	path := "/jobs/" + j.ID + "/diff"

	if got := viewOf(t, e, e.get(path)); got != viewUnified {
		t.Errorf("with no cookie and no query the view is %q, want unified", got)
	}
	if got := viewOf(t, e, getCookie(t, e, path, diffViewCookie, viewSplit)); got != viewSplit {
		t.Errorf("with a split cookie the view is %q, want split", got)
	}
	if got := viewOf(t, e, getCookie(t, e, path+"?view=unified", diffViewCookie, viewSplit)); got != viewUnified {
		t.Errorf("?view=unified did not beat the split cookie: got %q", got)
	}
	// A cookie somebody edited by hand is not a reason to refuse to show a
	// diff.
	if got := viewOf(t, e, getCookie(t, e, path, diffViewCookie, "sideways")); got != viewUnified {
		t.Errorf("a nonsense cookie gave view %q, want unified", got)
	}
}

// viewOf reads which toggle link the page marked current.
func viewOf(t *testing.T, e *env, res *http.Response) string {
	t.Helper()
	body := e.body(res)
	switch {
	case strings.Contains(body, `class="diff-view on" href="?view=split"`):
		return viewSplit
	case strings.Contains(body, `class="diff-view on" href="?view=unified"`):
		return viewUnified
	}
	t.Fatalf("no toggle link is marked current:\n%s", body)
	return ""
}

// TestRowBudgetDegradesToStubs is spec §5.4's 20 000 rows. Three files of
// 9 000 rows each: two fit and the third cannot, so it becomes a stub linking
// to its own page rather than 9 000 more rows on this one.
func TestRowBudgetDegradesToStubs(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "budget")
	var b strings.Builder
	for i := 0; i < 3; i++ {
		b.WriteString(bigFilePatch(t, i, 9000))
	}
	withPatch(t, e, j, fakePatch{patch: b.String()})

	body := e.body(e.get("/jobs/" + j.ID + "/diff"))
	if !strings.Contains(body, "line0-of-big0") || !strings.Contains(body, "line0-of-big1") {
		t.Error("the files inside the budget were not rendered")
	}
	if strings.Contains(body, "line0-of-big2") {
		t.Error("the file past the budget was rendered anyway")
	}
	if !strings.Contains(body, `href="/jobs/`+j.ID+`/diff/2"`) {
		t.Error("the stub does not link to the file's own page")
	}

	// The escape hatch has to actually work, or the stub is a dead end.
	res := e.get("/jobs/" + j.ID + "/diff/2")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("per-file page status %d", res.StatusCode)
	}
	if !strings.Contains(e.body(res), "line0-of-big2") {
		t.Error("the per-file page does not render the file the stub sent the reader to")
	}
}

// TestBigFileDefaultsCollapsed is the 500-line threshold. Collapsed means the
// <details> has no open attribute — the content is still on the page, which is
// what makes the collapse work with JavaScript off.
func TestBigFileDefaultsCollapsed(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "collapse")
	withPatch(t, e, j, fakePatch{patch: bigFilePatch(t, 0, 501) + smallPatch})
	body := e.body(e.get("/jobs/" + j.ID + "/diff"))

	big := detailsTag(t, body, "file-0")
	if strings.Contains(big, "open") {
		t.Errorf("a 501-line file is open by default: %s", big)
	}
	small := detailsTag(t, body, "file-1")
	if !strings.Contains(small, "open") {
		t.Errorf("a small file is not open by default: %s", small)
	}
	// ?file= is the reader saying "this one", and it wins over the threshold.
	body = e.body(e.get("/jobs/" + j.ID + "/diff?file=0"))
	if tag := detailsTag(t, body, "file-0"); !strings.Contains(tag, "open") {
		t.Errorf("?file=0 did not expand the file it named: %s", tag)
	}
}

func detailsTag(t *testing.T, body, id string) string {
	t.Helper()
	i := strings.Index(body, `<details class="dfile" id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no <details> for %s", id)
	}
	return body[i : i+strings.Index(body[i:], ">")+1]
}

// TestTruncatedPatchShowsTheBanner is the one that matters most on this page.
// A reviewer approving a merge from a silently clipped patch is deciding about
// a change they have not seen, and the flag comes from the capture rather than
// from the patch's length.
func TestTruncatedPatchShowsTheBanner(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "truncated")
	withPatch(t, e, j, fakePatch{patch: smallPatch, truncated: true})
	body := e.body(e.get("/jobs/" + j.ID + "/diff"))
	if !strings.Contains(body, "cut off at the 4 MiB capture limit") {
		t.Errorf("a truncated patch renders with no truncation banner:\n%s", body)
	}

	// And the same patch, not flagged, must not claim to be truncated: a
	// banner that is always there is a banner nobody reads.
	e2 := newEnv(t)
	j2 := e2.job("AWAITING_MERGE_APPROVAL", "whole")
	withPatch(t, e2, j2, fakePatch{patch: smallPatch})
	if body := e2.body(e2.get("/jobs/" + j2.ID + "/diff")); strings.Contains(body, "cut off at the 4 MiB") {
		t.Error("an untruncated patch was labelled truncated")
	}
}

// TestMalformedHunkShowsTheBanner: the header promises four old lines and the
// body has five. The parser keeps every line and flags the file; the page must
// pass the flag on rather than render the half it understood as a whole.
func TestMalformedHunkShowsTheBanner(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "malformed")
	const p = `diff --git a/x.go b/x.go
--- a/x.go
+++ b/x.go
@@ -1,2 +1,2 @@
 one
 two
 three
 four
`
	withPatch(t, e, j, fakePatch{patch: p})
	body := e.body(e.get("/jobs/" + j.ID + "/diff"))
	if !strings.Contains(body, "only partly understood") || !strings.Contains(body, `badge bad">malformed`) {
		t.Errorf("a malformed hunk renders with no warning:\n%s", body)
	}
}

// TestHTMLInADiffLineIsEscaped is the rule the whole file is written around. A
// patch is arbitrary text written by somebody else's code; a page that executed
// it would be executing whatever the agent under review decided to write.
func TestHTMLInADiffLineIsEscaped(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "escaping")
	const p = `diff --git a/index.html b/index.html
--- a/index.html
+++ b/index.html
@@ -1,2 +1,2 @@
 <p onclick="steal()">context</p>
-<script>alert(1)</script>
+<script>alert(2)</script>
`
	withPatch(t, e, j, fakePatch{patch: p})
	for _, view := range []string{viewUnified, viewSplit} {
		body := e.body(e.get("/jobs/" + j.ID + "/diff?view=" + view))
		for _, bad := range []string{"<script>alert(1)</script>", "<script>alert(2)</script>", `<p onclick="steal()">`} {
			if strings.Contains(body, bad) {
				t.Errorf("view=%s: %q reached the page unescaped", view, bad)
			}
		}
		// The escaped form is on the page, as text. In the split view the
		// intra-line mark cuts the line at the digit that changed, so the tag
		// is asserted rather than the whole line.
		for _, want := range []string{"&lt;script&gt;alert(", "&lt;/script&gt;", "&lt;p onclick=&#34;steal()&#34;&gt;"} {
			if !strings.Contains(body, want) {
				t.Errorf("view=%s: the escaped text %q is not on the page", view, want)
			}
		}
	}
	// The path is attacker-influenced too, and it is printed in an href.
	const pathPatch = `diff --git "a/x\"><script>evil()</script>.go" "b/x\"><script>evil()</script>.go"
--- "a/x\"><script>evil()</script>.go"
+++ "b/x\"><script>evil()</script>.go"
@@ -1 +1 @@
-a
+b
`
	e2 := newEnv(t)
	j2 := e2.job("AWAITING_MERGE_APPROVAL", "path escaping")
	withPatch(t, e2, j2, fakePatch{patch: pathPatch})
	if body := e2.body(e2.get("/jobs/" + j2.ID + "/diff")); strings.Contains(body, "<script>evil()</script>") {
		t.Error("a path containing markup reached the page unescaped")
	}
}

// TestOddFilesRenderSanely: a binary file, a pure rename with no hunk at all,
// and a file whose last line has no newline. Each is a shape that makes a
// hunk-driven renderer either drop the file or invent a line.
func TestOddFilesRenderSanely(t *testing.T) {
	const p = `diff --git a/logo.png b/logo.png
index 1111111..2222222 100644
Binary files a/logo.png and b/logo.png differ
diff --git a/old/name.go b/new/name.go
similarity index 100%
rename from old/name.go
rename to new/name.go
diff --git a/eof.txt b/eof.txt
--- a/eof.txt
+++ b/eof.txt
@@ -1 +1 @@
-before
\ No newline at end of file
+after
\ No newline at end of file
`
	for _, view := range []string{viewUnified, viewSplit} {
		e := newEnv(t)
		j := e.job("AWAITING_MERGE_APPROVAL", "odd files")
		withPatch(t, e, j, fakePatch{patch: p})
		body := e.body(e.get("/jobs/" + j.ID + "/diff?view=" + view))

		if !strings.Contains(body, "logo.png") || !strings.Contains(body, "Binary file changed") {
			t.Errorf("view=%s: the binary file is missing or was rendered as bytes", view)
		}
		if !strings.Contains(body, "old/name.go") || !strings.Contains(body, "new/name.go") {
			t.Errorf("view=%s: the pure rename was dropped — it has no hunk to be driven by", view)
		}
		if !strings.Contains(body, "renamed") {
			t.Errorf("view=%s: the rename has no rename badge", view)
		}
		if !strings.Contains(body, "no newline at end of file") {
			t.Errorf("view=%s: the missing-newline marker is not shown", view)
		}
		if !strings.Contains(body, "before") || !strings.Contains(body, "after") {
			t.Errorf("view=%s: the no-newline file's lines are missing", view)
		}
	}
}

// TestTestFileBadge is spec §8.5's badge: the orchestrator already cares which
// changes touch tests, and so does the reviewer.
func TestTestFileBadge(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.TestFileGlobs = []string{"**/*_test.go"}
	j := e.job("AWAITING_MERGE_APPROVAL", "badges")
	withPatch(t, e, j, fakePatch{patch: smallPatch + strings.ReplaceAll(smallPatch, "hello.go", "hello_test.go")})
	body := e.body(e.get("/jobs/" + j.ID + "/diff"))
	if !strings.Contains(body, `<span class="badge test">test</span>`) {
		t.Error("a test file carries no test badge")
	}
	if strings.Count(body, `class="badge test"`) != 1 {
		t.Error("the test badge is on a file that is not a test")
	}
}

// TestRawPatchIsPlainText: this is what a reviewer pipes into their own tools,
// so it is the patch and nothing else.
func TestRawPatchIsPlainText(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "raw")
	withPatch(t, e, j, fakePatch{patch: smallPatch, truncated: true})
	res := e.get("/jobs/" + j.ID + "/diff.patch")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := res.Header.Get("Content-Disposition"); got != "inline" {
		t.Errorf("Content-Disposition = %q, want inline", got)
	}
	if got := e.body(res); got != smallPatch {
		t.Errorf("the raw patch is not byte-for-byte what the source produced:\n%q", got)
	}
}

// TestNoPatchIsNotAnEmptyPatch. A worktree that is gone and a branch with no
// changes are different facts, and a tool handed the first as the second
// reports "no changes" about a change it never saw.
func TestNoPatchIsNotAnEmptyPatch(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "no worktree")
	j.Counters.BaseSHA = "abc"
	j.WorktreePath = e.tmp + "/gone"
	if err := e.st.UpdateJob(j); err != nil {
		t.Fatal(err)
	}
	res := e.get("/jobs/" + j.ID + "/diff.patch")
	if res.StatusCode != http.StatusConflict {
		t.Errorf("GET diff.patch with no worktree = %d, want 409", res.StatusCode)
	}
	if body := e.body(res); !strings.Contains(body, "is gone") {
		t.Errorf("the refusal does not say why: %q", body)
	}
	body := e.body(e.get("/jobs/" + j.ID + "/diff"))
	if !strings.Contains(body, "is gone") {
		t.Errorf("the diff page does not say the worktree is gone:\n%s", body)
	}
}

// TestEmptyDiffSaysSo covers the gate whose diff is genuinely empty: the page
// states it rather than rendering a blank area that reads as a broken page.
func TestEmptyDiffSaysSo(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "empty")
	withPatch(t, e, j, fakePatch{patch: ""})
	if body := e.body(e.get("/jobs/" + j.ID + "/diff")); !strings.Contains(body, "diff against its base is empty") {
		t.Errorf("an empty diff renders no explanation:\n%s", body)
	}
}

// TestPerFilePageBounds: an index that is not a file is a 404, not a panic and
// not a blank page.
func TestPerFilePageBounds(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "bounds")
	withPatch(t, e, j, fakePatch{patch: smallPatch})
	for _, idx := range []string{"1", "-1", "99", "notanumber"} {
		if got := e.get("/jobs/" + j.ID + "/diff/" + idx).StatusCode; got != http.StatusNotFound {
			t.Errorf("GET diff/%s = %d, want 404", idx, got)
		}
	}
	if got := e.get("/jobs/" + j.ID + "/diff/0").StatusCode; got != http.StatusOK {
		t.Errorf("GET diff/0 = %d, want 200", got)
	}
}

// TestInvalidUTF8DoesNotBreakThePage. The parser reproduces whatever bytes the
// patch held, and a patch of a latin-1 file holds bytes that are not UTF-8 at
// all. They must not reach the page as bytes the browser has to guess at, and
// slicing them for an intra-line mark must not split anything.
func TestInvalidUTF8DoesNotBreakThePage(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "latin-1")
	p := "diff --git a/l.txt b/l.txt\n--- a/l.txt\n+++ b/l.txt\n@@ -1 +1 @@\n" +
		"-caf\xe9 one\n+caf\xe9 two\n"
	withPatch(t, e, j, fakePatch{patch: p})
	for _, view := range []string{viewUnified, viewSplit} {
		res := e.get("/jobs/" + j.ID + "/diff?view=" + view)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("view=%s: status %d", view, res.StatusCode)
		}
		body := e.body(res)
		if strings.Contains(body, "\xe9") {
			t.Errorf("view=%s: a raw non-UTF-8 byte reached the page", view)
		}
		if !strings.Contains(body, "one") || !strings.Contains(body, "two") {
			t.Errorf("view=%s: the line was dropped rather than repaired", view)
		}
	}
}

// TestDiffCacheKeyIncludesHeadSHA. The cache is server.go's, and this is the
// property the diff page depends on: a job whose head moved must not be shown
// the diff of the commit before it.
func TestDiffCacheKeyIncludesHeadSHA(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "cache")
	first, err := e.srv.diffFor(j, smallPatch)
	if err != nil {
		t.Fatal(err)
	}
	same, err := e.srv.diffFor(j, smallPatch)
	if err != nil {
		t.Fatal(err)
	}
	if first[0] != same[0] {
		t.Error("the same job, head and patch re-parsed instead of hitting the cache")
	}
	j.HeadSHA = "1111111111111111111111111111111111111111"
	moved, err := e.srv.diffFor(j, smallPatch)
	if err != nil {
		t.Fatal(err)
	}
	if moved[0] == first[0] {
		t.Error("a moved head was served the previous commit's parse")
	}
}

// bigFilePatch builds one file of n changed lines, each carrying its file's
// number so a test can tell which files reached the page.
func bigFilePatch(t *testing.T, file, n int) string {
	t.Helper()
	var b strings.Builder
	name := "big" + strconv.Itoa(file) + ".txt"
	b.WriteString("diff --git a/" + name + " b/" + name + "\n")
	b.WriteString("--- a/" + name + "\n+++ b/" + name + "\n")
	b.WriteString("@@ -1," + strconv.Itoa(n) + " +1," + strconv.Itoa(n) + " @@\n")
	for i := 0; i < n; i++ {
		b.WriteString("+line" + strconv.Itoa(i) + "-of-big" + strconv.Itoa(file) + "\n")
	}
	return b.String()
}

// getCookie sends one request carrying one cookie, which is what a browser
// that has already been told a view preference looks like.
func getCookie(t *testing.T, e *env, path, name, value string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: name, Value: value})
	return e.do(req)
}
