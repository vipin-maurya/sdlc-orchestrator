// Fixtures under testdata/ were generated once with real git (2.43.0) and are
// committed as bytes. Regenerating one means re-running its recipe here and
// re-capturing its .numstat in the same breath — the oracle is only independent
// while the two describe the same moment.
//
// Every repo below starts as:
//
//	git init -q . && git config user.email f@x && git config user.name f
//	git config core.autocrlf false && git config core.quotePath true
//	git config diff.renames true
//
// and every fixture is captured as, which is what gitx.DiffPatchSince runs:
//
//	git add -A
//	git diff HEAD          > testdata/<name>.patch
//	git diff --numstat HEAD > testdata/<name>.numstat
//
// The changes, by fixture:
//
//	newfile      base: keep.txt; then write added.go (7 lines) and `git add -N added.go`
//	deletion     base: doomed.txt (12 lines), keep.txt; then `rm doomed.txt`
//	rename_pure  base: old_name.txt (8 lines); then `git mv old_name.txt new_name.txt`
//	rename_edit  base: before.txt (10 lines); `git mv before.txt after.txt`,
//	             then sed charlie->CHARLIE and india->INDIA
//	binary       base: logo.png (bytes), keep.txt; rewrite both
//	binary_literal  the same change captured with `git diff --binary HEAD`, so the
//	             parser meets the `GIT binary patch` base85 form it will never be
//	             handed by this call site but must not choke on
//	modeonly     base: script.sh; then `chmod +x script.sh`
//	modeonly_ambiguous   the same, on the path "sub b/script.sh", so the
//	             `diff --git` line contains ` b/` inside a path
//	nonewline    base: tail.txt (no trailing newline), gains.txt, loses.txt;
//	             edit tail.txt keeping it newline-free (two markers, one hunk),
//	             strip gains.txt's trailing newline (marker after the + only),
//	             edit loses.txt normally (control: no marker)
//	nocounts     base: single.txt ("only"); then "ONLY" -> `@@ -1 +1 @@`
//	zerocount    base: keep.txt; then add fresh.txt (5 lines) -> `@@ -0,0 +1,5 @@`
//	lookslikeheader  base: embedded.txt containing "---foo", " diff --git a/x b/y"
//	             and "@@ fake @@"; rewritten so the hunk body carries "----foo",
//	             "- diff --git …", "-@@ fake @@" and "++++ baz"
//	quotedpaths  base: "naïve file.txt" and "dir with spaces/plain name.txt";
//	             edit both. core.quotePath C-quotes the first and git appends a
//	             TAB after the second
//	crlf         base: win.txt written with \r\n; edit one line
//	crlf_to_lf   base: conv.txt written with \r\n; rewritten with \n only
//	multi        one patch with a modify, an append, a deletion, a pure rename
//	             and an addition
//	big          40 files of 100 generated functions each; 9 lines rewritten in
//	             each of 3 places per file. ~3160 lines, 152 KiB, for the W2-K
//	             benchmark
//
// Three fixtures cannot come from git and are derived from ones that did:
//
//	malformed_short  rename_edit.patch with `@@ -1,10 +1,10 @@` rewritten to
//	                 `@@ -1,12 +1,12 @@`. Only the header lies, so its .numstat
//	                 (copied from rename_edit) stays a truthful oracle
//	malformed_long   the same header rewritten to `@@ -1,4 +1,4 @@`, so the body
//	                 overruns it
//	truncated        the first 9000 bytes of big.patch, cut mid-line the way the
//	                 4 MiB capture cap cuts one
//
//	combined         a conflicted merge, captured with plain `git diff` during
//	                 the conflict: `git merge side` after side and main both
//	                 rewrote conflict.txt. No .numstat — Parse refuses it
//	empty            a clean worktree. Zero bytes
package diff

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func load(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".patch"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

func parseFixture(t *testing.T, name string) []*File {
	t.Helper()
	files, err := Parse(load(t, name))
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return files
}

func onlyFile(t *testing.T, name string) *File {
	t.Helper()
	files := parseFixture(t, name)
	if len(files) != 1 {
		t.Fatalf("%s: got %d files, want 1", name, len(files))
	}
	return files[0]
}

func fileNamed(t *testing.T, files []*File, path string) *File {
	t.Helper()
	for _, f := range files {
		if f.Path() == path {
			return f
		}
	}
	t.Fatalf("no file %q among %v", path, paths(files))
	return nil
}

func paths(files []*File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path()
	}
	return out
}

func wantLines(t *testing.T, h Hunk, want []Line) {
	t.Helper()
	if !reflect.DeepEqual(h.Lines, want) {
		t.Errorf("hunk lines mismatch\n got %#v\nwant %#v", h.Lines, want)
	}
}

// --- §8.3 case 1 ----------------------------------------------------------

func TestParseNewFile(t *testing.T) {
	f := onlyFile(t, "newfile")
	if f.Status != StatusAdded {
		t.Errorf("Status = %q, want %q", f.Status, StatusAdded)
	}
	// The path must come from `+++`/the diff --git header. A parser that reads
	// it off `--- ` names every new file /dev/null.
	if f.Path() != "added.go" || f.NewPath != "added.go" {
		t.Errorf("Path = %q, NewPath = %q, want added.go", f.Path(), f.NewPath)
	}
	if f.OldPath != "" {
		t.Errorf("OldPath = %q, want empty: the old side does not exist", f.OldPath)
	}
	if f.NewMode != "100644" {
		t.Errorf("NewMode = %q, want 100644", f.NewMode)
	}
	if f.Adds != 7 || f.Dels != 0 {
		t.Errorf("counts = +%d -%d, want +7 -0", f.Adds, f.Dels)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("got %d hunks, want 1", len(f.Hunks))
	}
	h := f.Hunks[0]
	if h.OldStart != 0 || h.OldLines != 0 || h.NewStart != 1 || h.NewLines != 7 {
		t.Errorf("hunk header = -%d,%d +%d,%d, want -0,0 +1,7", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
	}
	wantLines(t, h, []Line{
		{Kind: KindAdd, NewNo: 1, Text: "package main"},
		{Kind: KindAdd, NewNo: 2, Text: ""},
		{Kind: KindAdd, NewNo: 3, Text: `import "fmt"`},
		{Kind: KindAdd, NewNo: 4, Text: ""},
		{Kind: KindAdd, NewNo: 5, Text: "func main() {"},
		{Kind: KindAdd, NewNo: 6, Text: "\tfmt.Println(\"hello\")"},
		{Kind: KindAdd, NewNo: 7, Text: "}"},
	})
}

// --- §8.3 case 2 ----------------------------------------------------------

func TestParseDeletion(t *testing.T) {
	f := onlyFile(t, "deletion")
	if f.Status != StatusDeleted {
		t.Errorf("Status = %q, want %q", f.Status, StatusDeleted)
	}
	// A deleted 400-line file is the change most worth seeing, so it stays in
	// the list, keeps its lines, and answers Path() from the old side.
	if f.Path() != "doomed.txt" {
		t.Errorf("Path = %q, want doomed.txt", f.Path())
	}
	if f.NewPath != "" {
		t.Errorf("NewPath = %q, want empty", f.NewPath)
	}
	if f.OldMode != "100644" {
		t.Errorf("OldMode = %q, want 100644", f.OldMode)
	}
	if f.Adds != 0 || f.Dels != 12 {
		t.Errorf("counts = +%d -%d, want +0 -12", f.Adds, f.Dels)
	}
	h := f.Hunks[0]
	if h.NewStart != 0 || h.NewLines != 0 {
		t.Errorf("new side = %d,%d, want 0,0", h.NewStart, h.NewLines)
	}
	if len(h.Lines) != 12 {
		t.Fatalf("got %d lines, want 12", len(h.Lines))
	}
	for i, l := range h.Lines {
		if l.Kind != KindDel || l.OldNo != i+1 || l.NewNo != 0 || l.Text != "line "+strconv.Itoa(i+1) {
			t.Fatalf("line %d = %#v", i, l)
		}
	}
}

// --- §8.3 case 3 ----------------------------------------------------------

func TestParsePureRenameHasNoHunks(t *testing.T) {
	f := onlyFile(t, "rename_pure")
	// A hunk-driven parser drops this file entirely: there is no @@ anywhere in
	// the chunk. It has to appear, with a rename badge and 0/0.
	if len(f.Hunks) != 0 {
		t.Errorf("got %d hunks, want 0", len(f.Hunks))
	}
	if f.Status != StatusRenamed {
		t.Errorf("Status = %q, want %q", f.Status, StatusRenamed)
	}
	if f.OldPath != "old_name.txt" || f.NewPath != "new_name.txt" {
		t.Errorf("paths = %q -> %q, want old_name.txt -> new_name.txt", f.OldPath, f.NewPath)
	}
	if f.Adds != 0 || f.Dels != 0 || f.Rows() != 0 {
		t.Errorf("counts = +%d -%d rows=%d, want all zero", f.Adds, f.Dels, f.Rows())
	}
}

// --- §8.3 case 4 ----------------------------------------------------------

func TestParseRenameWithEdits(t *testing.T) {
	f := onlyFile(t, "rename_edit")
	if f.Status != StatusRenamed {
		t.Errorf("Status = %q, want %q", f.Status, StatusRenamed)
	}
	if f.OldPath != "before.txt" || f.NewPath != "after.txt" {
		t.Errorf("paths = %q -> %q", f.OldPath, f.NewPath)
	}
	if f.Adds != 2 || f.Dels != 2 {
		t.Errorf("counts = +%d -%d, want +2 -2", f.Adds, f.Dels)
	}
	if len(f.Hunks) != 1 || f.Rows() != 12 {
		t.Fatalf("hunks=%d rows=%d, want 1 hunk of 12 rows", len(f.Hunks), f.Rows())
	}
	wantLines(t, f.Hunks[0], []Line{
		{Kind: KindContext, OldNo: 1, NewNo: 1, Text: "alpha"},
		{Kind: KindContext, OldNo: 2, NewNo: 2, Text: "bravo"},
		{Kind: KindDel, OldNo: 3, Text: "charlie"},
		{Kind: KindAdd, NewNo: 3, Text: "CHARLIE"},
		{Kind: KindContext, OldNo: 4, NewNo: 4, Text: "delta"},
		{Kind: KindContext, OldNo: 5, NewNo: 5, Text: "echo"},
		{Kind: KindContext, OldNo: 6, NewNo: 6, Text: "foxtrot"},
		{Kind: KindContext, OldNo: 7, NewNo: 7, Text: "golf"},
		{Kind: KindContext, OldNo: 8, NewNo: 8, Text: "hotel"},
		{Kind: KindDel, OldNo: 9, Text: "india"},
		{Kind: KindAdd, NewNo: 9, Text: "INDIA"},
		{Kind: KindContext, OldNo: 10, NewNo: 10, Text: "juliet"},
	})
}

// --- §8.3 case 5 ----------------------------------------------------------

func TestParseBinary(t *testing.T) {
	for _, name := range []string{"binary", "binary_literal"} {
		t.Run(name, func(t *testing.T) {
			files := parseFixture(t, name)
			if len(files) != 2 {
				t.Fatalf("got %d files, want 2", len(files))
			}
			png := fileNamed(t, files, "logo.png")
			if !png.Binary {
				t.Error("Binary = false; the page would try to display bytes")
			}
			if len(png.Hunks) != 0 || png.Adds != 0 || png.Dels != 0 {
				t.Errorf("hunks=%d +%d -%d, want no line content", len(png.Hunks), png.Adds, png.Dels)
			}
			// The text file in the same patch must survive the binary one. The
			// base85 payload of the --binary form is what would swallow it.
			keep := fileNamed(t, files, "keep.txt")
			if keep.Binary || keep.Adds != 1 || keep.Dels != 0 {
				t.Errorf("keep.txt = binary:%v +%d -%d, want text +1 -0", keep.Binary, keep.Adds, keep.Dels)
			}
		})
	}
}

// --- §8.3 case 6 ----------------------------------------------------------

func TestParseModeOnly(t *testing.T) {
	f := onlyFile(t, "modeonly")
	// A file becoming executable is a real review finding and has no lines at
	// all to announce itself with.
	if f.Status != StatusModeOnly {
		t.Errorf("Status = %q, want %q", f.Status, StatusModeOnly)
	}
	if f.OldMode != "100644" || f.NewMode != "100755" {
		t.Errorf("modes = %q -> %q, want 100644 -> 100755", f.OldMode, f.NewMode)
	}
	if len(f.Hunks) != 0 || f.Adds != 0 || f.Dels != 0 {
		t.Errorf("hunks=%d +%d -%d, want all zero", len(f.Hunks), f.Adds, f.Dels)
	}
	if f.Path() != "script.sh" {
		t.Errorf("Path = %q", f.Path())
	}
}

// TestModeOnlyPathContainingTheSeparator pins §8.3 case 11's ambiguity. A
// mode-only chunk has no ---/+++ and no rename lines, so its path can only come
// from the `diff --git` line, and here that line reads
// `diff --git a/sub b/script.sh b/sub b/script.sh` — three candidate split
// points. Splitting on the last ` b/`, which is what the spec proposes, names
// the file "script.sh" and puts it in the wrong directory silently. Preferring
// the split that makes both sides equal gets it right, and the two sides of a
// mode-only chunk are always equal.
func TestModeOnlyPathContainingTheSeparator(t *testing.T) {
	f := onlyFile(t, "modeonly_ambiguous")
	if f.Path() != "sub b/script.sh" {
		t.Errorf("Path = %q, want %q", f.Path(), "sub b/script.sh")
	}
	if f.OldPath != f.NewPath {
		t.Errorf("paths disagree: %q vs %q", f.OldPath, f.NewPath)
	}
}

// --- §8.3 case 7 ----------------------------------------------------------

func TestNoNewlineMarkerDoesNotCountAsALine(t *testing.T) {
	files := parseFixture(t, "nonewline")

	// tail.txt: both sides lack the trailing newline, so one hunk carries two
	// markers. Each attaches to the line above it, not to a side the parser
	// picked.
	tail := fileNamed(t, files, "tail.txt")
	wantLines(t, tail.Hunks[0], []Line{
		{Kind: KindContext, OldNo: 1, NewNo: 1, Text: "alpha"},
		{Kind: KindContext, OldNo: 2, NewNo: 2, Text: "bravo"},
		{Kind: KindDel, OldNo: 3, Text: "charlie", NoNewline: true},
		{Kind: KindAdd, NewNo: 3, Text: "CHARLIE", NoNewline: true},
	})

	// gains.txt: the marker follows the addition, and arrives *after* the
	// hunk's counts are already satisfied.
	gains := fileNamed(t, files, "gains.txt")
	wantLines(t, gains.Hunks[0], []Line{
		{Kind: KindContext, OldNo: 1, NewNo: 1, Text: "one"},
		{Kind: KindContext, OldNo: 2, NewNo: 2, Text: "two"},
		{Kind: KindDel, OldNo: 3, Text: "three"},
		{Kind: KindAdd, NewNo: 3, Text: "three", NoNewline: true},
	})

	// The marker is not a line: counts, line numbers and Rows() are the same as
	// the control file that has no marker at all.
	loses := fileNamed(t, files, "loses.txt")
	for _, f := range []*File{tail, gains, loses} {
		if f.Adds != 1 || f.Dels != 1 {
			t.Errorf("%s: counts = +%d -%d, want +1 -1", f.Path(), f.Adds, f.Dels)
		}
		if f.Rows() != 4 {
			t.Errorf("%s: Rows = %d, want 4", f.Path(), f.Rows())
		}
		if got := f.Hunks[0].Lines[3].NewNo; got != 3 {
			t.Errorf("%s: last line NewNo = %d, want 3", f.Path(), got)
		}
	}
	if loses.Hunks[0].Lines[3].NoNewline {
		t.Error("loses.txt: NoNewline set on a file that has a trailing newline")
	}
}

// --- §8.3 case 8 ----------------------------------------------------------

func TestHunkHeaderWithoutCounts(t *testing.T) {
	// `@@ -1 +1 @@`: an absent count means exactly one line, not zero.
	f := onlyFile(t, "nocounts")
	h := f.Hunks[0]
	if h.OldStart != 1 || h.OldLines != 1 || h.NewStart != 1 || h.NewLines != 1 {
		t.Errorf("hunk = -%d,%d +%d,%d, want -1,1 +1,1", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
	}
	wantLines(t, h, []Line{
		{Kind: KindDel, OldNo: 1, Text: "only"},
		{Kind: KindAdd, NewNo: 1, Text: "ONLY"},
	})
	if f.Malformed != "" {
		t.Errorf("Malformed = %q on a well-formed countless hunk", f.Malformed)
	}

	// `@@ -0,0 +1,5 @@`: the old side is empty and start 0 is legal.
	z := onlyFile(t, "zerocount")
	zh := z.Hunks[0]
	if zh.OldStart != 0 || zh.OldLines != 0 || zh.NewStart != 1 || zh.NewLines != 5 {
		t.Errorf("hunk = -%d,%d +%d,%d, want -0,0 +1,5", zh.OldStart, zh.OldLines, zh.NewStart, zh.NewLines)
	}
	if z.Adds != 5 || z.Malformed != "" {
		t.Errorf("+%d, Malformed=%q", z.Adds, z.Malformed)
	}
}

func TestHunkSectionIsKept(t *testing.T) {
	// git's function-context hint is the only thing telling a reader which
	// function a hunk starting at line 39 is inside.
	f := parseFixture(t, "big")[0]
	if got := f.Hunks[0].Section; got != `func F01_006() string { return "value 01 006" }` {
		t.Errorf("Section = %q", got)
	}
}

// --- §8.3 case 9 ----------------------------------------------------------

func TestMalformedHunkIsFlaggedNotDropped(t *testing.T) {
	// Both directions of a lying header. In neither is a line allowed to
	// disappear to make the arithmetic work.
	for _, name := range []string{"malformed_short", "malformed_long"} {
		t.Run(name, func(t *testing.T) {
			f := onlyFile(t, name)
			if f.Malformed == "" {
				t.Fatal("Malformed is empty; the page would render this as complete")
			}
			if len(f.Hunks) != 1 || f.Rows() != 12 {
				t.Fatalf("hunks=%d rows=%d, want 1 hunk of 12 rows (nothing discarded)", len(f.Hunks), f.Rows())
			}
			if f.Adds != 2 || f.Dels != 2 {
				t.Errorf("counts = +%d -%d, want +2 -2", f.Adds, f.Dels)
			}
			// The body is the same as the well-formed fixture it was derived
			// from; only the header was rewritten.
			wantLines(t, f.Hunks[0], onlyFile(t, "rename_edit").Hunks[0].Lines)
		})
	}
}

// --- §8.3 case 10 ---------------------------------------------------------

func TestContentThatLooksLikeAHeader(t *testing.T) {
	// Inside a hunk every line carries a prefix byte. A parser that matches
	// file-header patterns here loses embedded.txt from the review entirely and
	// says nothing about it.
	f := onlyFile(t, "lookslikeheader")
	if f.Path() != "embedded.txt" {
		t.Fatalf("Path = %q", f.Path())
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("got %d hunks, want 1", len(f.Hunks))
	}
	wantLines(t, f.Hunks[0], []Line{
		{Kind: KindContext, OldNo: 1, NewNo: 1, Text: "intro"},
		{Kind: KindDel, OldNo: 2, Text: "---foo"},
		{Kind: KindDel, OldNo: 3, Text: " diff --git a/x b/y"},
		{Kind: KindDel, OldNo: 4, Text: "@@ fake @@"},
		{Kind: KindAdd, NewNo: 2, Text: "---bar"},
		{Kind: KindAdd, NewNo: 3, Text: " diff --git a/p b/q"},
		{Kind: KindAdd, NewNo: 4, Text: "@@ also fake @@"},
		{Kind: KindAdd, NewNo: 5, Text: "+++ baz"},
		{Kind: KindContext, OldNo: 5, NewNo: 6, Text: "outro"},
	})
	if f.Adds != 4 || f.Dels != 3 || f.Malformed != "" {
		t.Errorf("+%d -%d Malformed=%q, want +4 -3 and no flag", f.Adds, f.Dels, f.Malformed)
	}
}

// --- §8.3 case 11 ---------------------------------------------------------

func TestQuotedPaths(t *testing.T) {
	files := parseFixture(t, "quotedpaths")
	if len(files) != 2 {
		t.Fatalf("got %d files (%v), want 2", len(files), paths(files))
	}
	// core.quotePath C-quotes the non-ASCII name as "a/na\303\257ve file.txt".
	naive := fileNamed(t, files, "naïve file.txt")
	if naive.OldPath != "naïve file.txt" {
		t.Errorf("OldPath = %q", naive.OldPath)
	}
	// The plain name has a space, so git appends a TAB after it on the ---/+++
	// lines. A parser that takes the rest of the line keeps the tab in the path
	// and then fails to match it against anything.
	plain := fileNamed(t, files, "dir with spaces/plain name.txt")
	if strings.ContainsAny(plain.Path(), "\t\"") {
		t.Errorf("Path = %q still carries the separator or its quotes", plain.Path())
	}
}

// --- §8.3 case 12 ---------------------------------------------------------

func TestCRLF(t *testing.T) {
	// crlf.patch: the file's content is CRLF, the patch's own terminators are
	// LF. The \r belongs to the content and must survive, or a CRLF-to-LF
	// conversion renders as two identical lines and reads as a parser bug.
	f := onlyFile(t, "crlf")
	for _, l := range f.Hunks[0].Lines {
		if !strings.HasSuffix(l.Text, "\r") {
			t.Errorf("line %q lost its \\r", l.Text)
		}
	}

	conv := onlyFile(t, "crlf_to_lf")
	del, add := conv.Hunks[0].Lines[0], conv.Hunks[0].Lines[2]
	if del.Text == add.Text {
		t.Errorf("a CRLF-to-LF change rendered as two identical lines %q", del.Text)
	}
	if del.Text != "alpha\r" || add.Text != "alpha" {
		t.Errorf("got %q -> %q, want \"alpha\\r\" -> \"alpha\"", del.Text, add.Text)
	}

	// crlf_endings.patch: the patch file itself is CRLF, structural lines
	// included. There the \r is the terminator and comes off.
	e := onlyFile(t, "crlf_endings")
	if e.Path() != "single.txt" {
		t.Fatalf("Path = %q; the header did not survive its own terminators", e.Path())
	}
	wantLines(t, e.Hunks[0], []Line{
		{Kind: KindDel, OldNo: 1, Text: "only"},
		{Kind: KindAdd, NewNo: 1, Text: "ONLY"},
	})
}

// --- §8.3 case 13 ---------------------------------------------------------

func TestCombinedDiffIsRejected(t *testing.T) {
	// Refusing is the point. Every one of these lines has a plausible reading
	// that is wrong: `@@@ -1,1 -1,1 +1,5 @@@` looks like a hunk header, and
	// `++<<<<<<< HEAD` looks like an added line.
	if _, err := Parse(load(t, "combined")); err != ErrCombinedDiff {
		t.Fatalf("err = %v, want ErrCombinedDiff", err)
	}
	for _, s := range []string{
		"@@@ -1,1 -1,1 +1,5 @@@\n",
		"diff --cc conflict.txt\n@@@ -1,1 -1,1 +1,5 @@@\n",
		"diff --combined conflict.txt\n",
	} {
		if _, err := Parse(s); err != ErrCombinedDiff {
			t.Errorf("Parse(%q) = %v, want ErrCombinedDiff", s, err)
		}
	}
}

// --- §8.3 case 14 ---------------------------------------------------------

func TestEmptyAndTruncatedPatch(t *testing.T) {
	// Empty is a fact about the gate, not an error: the page says "no changes".
	for _, s := range []string{"", "\n", load(t, "empty")} {
		files, err := Parse(s)
		if err != nil {
			t.Errorf("Parse(%q) = %v, want no error", s, err)
		}
		if len(files) != 0 {
			t.Errorf("Parse(%q) returned %d files", s, len(files))
		}
	}

	// Truncated: parse what is there. The cut file keeps its lines and is
	// flagged, because a patch the parser half-understood must say so.
	files := parseFixture(t, "truncated")
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3", len(files))
	}
	for _, f := range files[:2] {
		if f.Malformed != "" {
			t.Errorf("%s: Malformed = %q on a complete file", f.Path(), f.Malformed)
		}
	}
	last := files[2]
	if last.Malformed == "" {
		t.Error("the cut file is not flagged; the page would render it as complete")
	}
	if last.Rows() == 0 {
		t.Error("the cut file lost every line it did have")
	}
	// A patch cut mid-word must not lose the file's identity.
	if last.Path() != "pkg/file03.go" {
		t.Errorf("Path = %q, want pkg/file03.go", last.Path())
	}
}

// --- the general case, and the shape the viewers consume -------------------

func TestParseMultiFileKeepsEveryFileAndItsOrder(t *testing.T) {
	files := parseFixture(t, "multi")
	got := paths(files)
	want := []string{"a.txt", "b.txt", "c.txt", "d_new.txt", "e.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	wantStatus := []Status{StatusModified, StatusModified, StatusDeleted, StatusRenamed, StatusAdded}
	for i, f := range files {
		if f.Status != wantStatus[i] {
			t.Errorf("%s: Status = %q, want %q", f.Path(), f.Status, wantStatus[i])
		}
	}
}

func TestRowsCountsEveryLineOfEveryHunk(t *testing.T) {
	f := parseFixture(t, "big")[0]
	n := 0
	for _, h := range f.Hunks {
		n += len(h.Lines)
	}
	if f.Rows() != n || n == 0 {
		t.Errorf("Rows = %d, want %d", f.Rows(), n)
	}
}

// --- AC-25: the independent oracle ----------------------------------------

// TestCountsMatchNumstat checks the parser's arithmetic against git's own,
// captured at fixture-generation time and committed beside each patch. It is a
// file rather than a `git diff --numstat` invocation so that this package keeps
// its defining property — no process, no filesystem beyond testdata — and so
// the test is identical on a machine with no git installed.
func TestCountsMatchNumstat(t *testing.T) {
	patches, err := filepath.Glob(filepath.Join("testdata", "*.patch"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, p := range patches {
		name := strings.TrimSuffix(filepath.Base(p), ".patch")
		raw, err := os.ReadFile(filepath.Join("testdata", name+".numstat"))
		if os.IsNotExist(err) {
			// combined and truncated have no honest oracle: one is refused,
			// the other is a fragment of a patch git never produced.
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Run(name, func(t *testing.T) {
			files := parseFixture(t, name)
			rows := parseNumstat(string(raw))
			if len(rows) != len(files) {
				t.Fatalf("numstat lists %d files, parser found %d (%v)", len(rows), len(files), paths(files))
			}
			for _, r := range rows {
				f := fileNamed(t, files, r.path)
				if r.binary {
					if !f.Binary {
						t.Errorf("%s: numstat says binary, parser does not", r.path)
					}
					continue
				}
				if f.Adds != r.adds || f.Dels != r.dels {
					t.Errorf("%s: parser +%d -%d, git +%d -%d", r.path, f.Adds, f.Dels, r.adds, r.dels)
				}
			}
			checked++
		})
	}
	if checked < 15 {
		t.Errorf("only %d fixtures carried an oracle; the corpus lost its .numstat files", checked)
	}
}

type numstatRow struct {
	adds, dels int
	binary     bool
	path       string
}

func parseNumstat(s string) []numstatRow {
	var out []numstatRow
	for _, line := range strings.Split(s, "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		r := numstatRow{path: numstatPath(fields[2])}
		if fields[0] == "-" {
			r.binary = true
		} else {
			r.adds, _ = strconv.Atoi(fields[0])
			r.dels, _ = strconv.Atoi(fields[1])
		}
		out = append(out, r)
	}
	return out
}

// numstatPath resolves numstat's display path to the name File.Path() answers.
// A rename is shown as "old => new", or compacted to "pre{old => new}post" when
// the two share a prefix or suffix; either way the new name is what the file is
// called now.
func numstatPath(s string) string {
	s = unquotePath(s)
	open := strings.Index(s, "{")
	arrow := strings.Index(s, " => ")
	if arrow < 0 {
		return s
	}
	if open >= 0 && open < arrow {
		if shut := strings.Index(s[arrow:], "}"); shut >= 0 {
			return s[:open] + s[arrow+len(" => "):arrow+shut] + s[arrow+shut+1:]
		}
	}
	return s[arrow+len(" => "):]
}
