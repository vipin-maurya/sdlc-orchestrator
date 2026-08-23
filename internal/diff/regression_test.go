package diff

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestRenameAndCopyKeepTheirDirectory pins the paths of a rename and a copy
// whose files live under a top-level directory literally named `a` or `b`.
// Those four header lines carry the filename directly, with no a/ b/ prefix, so
// stripping one took a real directory off the front — and a pure rename has no
// later `--- a/...` line to overwrite the damage, so the wrong path was final
// and nothing said so. The merge screen then offers a file that is not in the
// repository.
func TestRenameAndCopyKeepTheirDirectory(t *testing.T) {
	for _, tc := range []struct{ fixture, status, old, new string }{
		{"rename_in_dir", "renamed", "a/foo.txt", "a/renamed.txt"},
		{"copy_pure", "copied", "b/orig.txt", "b/dup.txt"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			files := parseFixture(t, tc.fixture)
			if len(files) != 1 {
				t.Fatalf("got %d files, want 1", len(files))
			}
			f := files[0]
			if got := string(f.Status); got != tc.status {
				t.Errorf("Status = %q, want %q", got, tc.status)
			}
			if f.OldPath != tc.old {
				t.Errorf("OldPath = %q, want %q", f.OldPath, tc.old)
			}
			if f.NewPath != tc.new {
				t.Errorf("NewPath = %q, want %q", f.NewPath, tc.new)
			}
			if f.Path() != tc.new {
				t.Errorf("Path() = %q, want %q", f.Path(), tc.new)
			}
		})
	}
}

// TestTrailingSpaceInAFilenameSurvives holds a filename that ends in a space.
// It is legal on POSIX and git does not quote it, so trimming the header line
// silently renamed the file. git's own --numstat for this fixture ends in that
// space; an editor or hook that strips trailing whitespace from testdata will
// break this test, which is the correct outcome rather than a silent one.
func TestTrailingSpaceInAFilenameSurvives(t *testing.T) {
	files := parseFixture(t, "trailing_space")
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if got := files[0].Path(); got != "trail " {
		t.Errorf("Path() = %q, want %q", got, "trail ")
	}
}

// TestSplitTerminatesOnAnUnknownKind is a liveness test, so it asserts nothing
// about the rows. A Kind outside the three consumed no lines, so the loop never
// advanced and Split spun forever. Parse cannot produce such a line, but Split
// is exported and takes an exported struct with an exported integer field, and
// this code is bound for a request handler where a hang is a denial of service.
func TestSplitTerminatesOnAnUnknownKind(t *testing.T) {
	h := Hunk{Lines: []Line{
		{Kind: KindContext, Text: "before"},
		{Kind: Kind(0), Text: "zero value"},
		{Kind: Kind(99), Text: "out of range"},
		{Kind: KindDel, Text: "gone"},
		{Kind: KindAdd, Text: "new"},
		{Kind: KindContext, Text: "after"},
	}}
	done := make(chan []Row, 1)
	go func() { done <- Split(h) }()
	select {
	case rows := <-done:
		// The known lines must still pair correctly around the junk.
		var ctx int
		for _, r := range rows {
			if r.HasLeft && r.HasRight && r.Left.Text == r.Right.Text {
				ctx++
			}
		}
		if ctx < 2 {
			t.Errorf("context rows lost around an unknown kind: %d of 2", ctx)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Split did not return: an unknown Kind advances no index")
	}
}

// TestIntraLineGuardBoundary pins the 70% itself. The two fixtures that covered
// this sat at ratios 1.00 and 0.0625, which constrained the threshold only to
// somewhere between them: moving it to 0.99, flipping the comparison, or
// swapping the denominator all left the suite green. These two cases sit either
// side of the boundary, so the number is now a decision rather than a variable.
func TestIntraLineGuardBoundary(t *testing.T) {
	// Ten bytes on each side, differing in a run of exactly n in the middle.
	// span/longer is then n/10, and the guard drops the marks when it exceeds
	// 0.7 — so 7 is marked and 8 is not.
	for _, tc := range []struct {
		name       string
		a, b       string
		wantMarked bool
	}{
		// 7 of 10 changed: exactly at 0.7, which is not "past it".
		{"at the threshold", "aa1234567x", "aa7654321x", true},
		// 8 of 10 changed: past it.
		{"past the threshold", "a12345678x", "a87654321x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.a) != 10 || len(tc.b) != 10 {
				t.Fatalf("fixture is not 10 bytes: %d/%d", len(tc.a), len(tc.b))
			}
			r := Row{Left: Line{Text: tc.a}, Right: Line{Text: tc.b}, HasLeft: true, HasRight: true}
			markIntraLine(&r)
			span := r.LeftEnd - r.LeftStart
			if e := r.RightEnd - r.RightStart; e > span {
				span = e
			}
			marked := r.LeftEnd != r.LeftStart || r.RightEnd != r.RightStart
			if marked != tc.wantMarked {
				t.Errorf("marked = %v (span %d of 10), want %v", marked, span, tc.wantMarked)
			}
		})
	}
}

// TestMalformedTextStaysBounded holds the cap on File.Malformed.
//
// The dedup only suppresses a message that repeats verbatim, and two callers
// build their message from the line or the numbers they read — so a patch of
// distinct junk headers produced a distinct message every time, and the
// Contains scan then walked everything already recorded. 268 KB of this took
// 13 seconds and produced 768 KB of Malformed; the capture cap is 4 MiB. Real
// git output cannot reach it, but the fuzzer in this package can.
func TestMalformedTextStaysBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/x b/x\n")
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&b, "@@ junk %d\n", i)
	}
	done := make(chan []*File, 1)
	go func() {
		files, _ := Parse(b.String())
		done <- files
	}()
	var files []*File
	select {
	case files = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Parse did not finish: the flag path is quadratic again")
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	// The cap is 1 KiB; the seal adds one short sentence after it.
	if n := len(files[0].Malformed); n > maxMalformed+128 {
		t.Errorf("Malformed grew to %d bytes, past the %d cap", n, maxMalformed)
	}
	// Bounded is not the same as silent: the file must still say it is bad,
	// and say that more was dropped.
	if !strings.Contains(files[0].Malformed, "unparsable hunk header") {
		t.Error("the bounded Malformed no longer says what went wrong")
	}
	if !strings.Contains(files[0].Malformed, "not recorded") {
		t.Errorf("the cap fired but the file does not say anything was dropped: %q", files[0].Malformed)
	}
}

// TestUnusableHunkNumberIsFlagged covers a header whose number does not fit an
// int. It used to fold to 0 in silence, which left a deleted line carrying
// OldNo == 0 — the value Line's own doc reserves for "does not exist on that
// side". A patch the parser cannot honour has to say so.
func TestUnusableHunkNumberIsFlagged(t *testing.T) {
	files, err := Parse("diff --git a/x b/x\n@@ -99999999999999999999,1 +1,1 @@\n-a\n+b\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if files[0].Malformed == "" {
		t.Fatal("an out-of-range hunk count parsed silently")
	}
	if !strings.Contains(files[0].Malformed, "not a usable number") {
		t.Errorf("Malformed does not name the problem: %q", files[0].Malformed)
	}
}

// TestTextFileSurvivesAPrecedingBinaryPayload is the case TestParseBinary's
// comment describes and its fixture cannot reach: in binary_literal.patch the
// text file comes first, so nothing follows the base85 payload and the branch
// that ends it has no coverage at all. Here the payload comes first.
func TestTextFileSurvivesAPrecedingBinaryPayload(t *testing.T) {
	files := parseFixture(t, "binary_first")
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2 — the payload swallowed what followed it: %v", len(files), paths(files))
	}
	if !files[0].Binary {
		t.Errorf("%s is not marked binary", files[0].Path())
	}
	text := files[1]
	if text.Path() != "z_keep.txt" {
		t.Fatalf("second file is %q, want z_keep.txt", text.Path())
	}
	if text.Binary {
		t.Error("the text file after a binary payload is marked binary")
	}
	if len(text.Hunks) == 0 {
		t.Fatal("the text file after a binary payload has no hunks")
	}
	if text.Adds != 1 || text.Dels != 0 {
		t.Errorf("counts are +%d -%d, want +1 -0", text.Adds, text.Dels)
	}
}

// TestShortHunkMidPatchFlagsAndRecovers covers stepHunk's default branch: a
// hunk whose body ends before its header said it would, with another file
// after it. Every other short-hunk fixture runs out at EOF, where closeHunk
// catches it instead, so this branch had no coverage.
//
// Two things are asserted separately because they are separate claims, and one
// of them is weaker than it looks. The branch's own message must be present —
// closeHunk flags the same file with a different sentence, so asserting merely
// that Malformed is non-empty passes with this branch deleted. And the next
// file must survive; note that it survives even with Parse's hand-back removed,
// because the header state recovers on its own from the following lines. The
// hand-back is load-bearing for the binary payload, not here — see
// TestTextFileSurvivesAPrecedingBinaryPayload, which is what fails when it goes.
func TestShortHunkMidPatchFlagsAndRecovers(t *testing.T) {
	files := parseFixture(t, "short_midpatch")
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2: %v", len(files), paths(files))
	}
	if !strings.Contains(files[0].Malformed, "shorter than its header declares") {
		t.Errorf("the mid-patch shortfall is not flagged by stepHunk: %q", files[0].Malformed)
	}
	second := files[1]
	if second.Path() != "second.txt" {
		t.Fatalf("second file is %q, want second.txt", second.Path())
	}
	if second.Malformed != "" {
		t.Errorf("the file after the short hunk is flagged: %q", second.Malformed)
	}
	if second.Adds != 1 || second.Dels != 1 {
		t.Errorf("second.txt counts are +%d -%d, want +1 -1", second.Adds, second.Dels)
	}
}

// TestModeChangeWithAnEditIsNotBadgedModeOnly holds the difference between a
// chmod and a chmod that also changed the file. Both carry `old mode`/`new
// mode`; only the first has no hunks, and that is the whole of the test in
// closeFile. Dropping the hunk check badges this file StatusModeOnly, which
// the diff page renders as a badge saying the contents did not change — about
// a file whose contents did.
func TestModeChangeWithAnEditIsNotBadgedModeOnly(t *testing.T) {
	f := onlyFile(t, "mode_and_edit")
	if f.Status == StatusModeOnly {
		t.Errorf("a file that changed mode AND content is badged %q", f.Status)
	}
	if f.OldMode != "100644" || f.NewMode != "100755" {
		t.Errorf("modes are %q -> %q, want 100644 -> 100755", f.OldMode, f.NewMode)
	}
	if f.Adds != 1 || f.Dels != 1 {
		t.Errorf("counts are +%d -%d, want +1 -1", f.Adds, f.Dels)
	}
}

// TestQuotedPathWithAnEscapedQuote is a mode-only change whose filename
// contains a double quote. There are no ---/+++ lines on a mode-only change,
// so the `diff --git` line is the only source of the path and closingQuote has
// to find the real end of the quoted name — the escaped quote in the middle is
// not it. Every other quoted fixture has ---/+++ lines to recover from, which
// is why the escape skip could be deleted with the suite still green.
func TestQuotedPathWithAnEscapedQuote(t *testing.T) {
	f := onlyFile(t, "quoted_escape")
	if want := `qu"ote.txt`; f.Path() != want {
		t.Errorf("Path() = %q, want %q", f.Path(), want)
	}
	if f.Status != StatusModeOnly {
		t.Errorf("Status = %q, want mode-only", f.Status)
	}
}

// TestBlankContextLineWithItsSpaceStripped covers a patch that has been through
// an editor or a mail client, which strip the trailing space that makes a blank
// context line ` `. While the hunk still owes lines, an empty line is that
// context line — treating it as the end of the hunk drops everything after the
// first blank line in the file, and silently, because the counts still balance
// for the lines that did arrive.
func TestBlankContextLineWithItsSpaceStripped(t *testing.T) {
	files := parseFixture(t, "blank_context_stripped")
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2: %v", len(files), paths(files))
	}
	f := files[0]
	if f.Malformed != "" {
		t.Errorf("a stripped blank context line is treated as damage: %q", f.Malformed)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("got %d hunks, want 1", len(f.Hunks))
	}
	var got []string
	for _, l := range f.Hunks[0].Lines {
		got = append(got, string(rune('0'+int(l.Kind)))+l.Text)
	}
	if len(f.Hunks[0].Lines) != 4 {
		t.Errorf("hunk has %d lines, want 4 (one, blank, -two, +three): %v", len(f.Hunks[0].Lines), got)
	}
	if f.Adds != 1 || f.Dels != 1 {
		t.Errorf("counts are +%d -%d, want +1 -1", f.Adds, f.Dels)
	}
	if files[1].Path() != "y.txt" {
		t.Errorf("second file is %q, want y.txt", files[1].Path())
	}
}
