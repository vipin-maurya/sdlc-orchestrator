package diff

import (
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
