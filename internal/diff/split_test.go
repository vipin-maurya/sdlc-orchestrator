package diff

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// splitFixture parses a fixture under testdata/split and returns its one hunk.
// These are hand-written rather than generated: they exist to put a specific
// pairing shape in front of Split, and a shape is easier to state than to
// coax out of git.
func splitFixture(t *testing.T, name string) Hunk {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "split", name+".patch"))
	if err != nil {
		t.Fatal(err)
	}
	files, err := Parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].Hunks) != 1 {
		t.Fatalf("%s: want one file with one hunk, got %d files", name, len(files))
	}
	if files[0].Malformed != "" {
		t.Fatalf("%s: %s — the fixture's own counts are wrong", name, files[0].Malformed)
	}
	return files[0].Hunks[0]
}

func TestSplitPairsChangeBlocksPositionally(t *testing.T) {
	rows := Split(splitFixture(t, "pairs"))
	want := []struct{ left, right string }{
		{"head", "head"},
		{"one", "ONE"},
		{"two", "TWO"},
		{"three", "THREE"},
		{"tail", "tail"},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		r := rows[i]
		if !r.HasLeft || !r.HasRight {
			t.Fatalf("row %d has a filler side", i)
		}
		if r.Left.Text != w.left || r.Right.Text != w.right {
			t.Errorf("row %d = %q | %q, want %q | %q", i, r.Left.Text, r.Right.Text, w.left, w.right)
		}
	}
	// Line numbers come through untouched: the gutters are what a reviewer uses
	// to find the line in their editor.
	if rows[1].Left.OldNo != 2 || rows[1].Right.NewNo != 2 {
		t.Errorf("row 1 numbers = old %d / new %d, want 2 / 2", rows[1].Left.OldNo, rows[1].Right.NewNo)
	}
}

func TestSplitFillsTheShorterSide(t *testing.T) {
	// One deletion against two additions, then an insertion with no deletion
	// before it at all.
	rows := Split(splitFixture(t, "filler"))
	type cell struct {
		text string
		has  bool
	}
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		l, rt := cell{r.Left.Text, r.HasLeft}, cell{r.Right.Text, r.HasRight}
		ls, rs := "·", "·"
		if l.has {
			ls = l.text
		}
		if rt.has {
			rs = rt.text
		}
		got = append(got, ls+" | "+rs)
	}
	want := []string{
		"head | head",
		"gone | added one",
		"· | added two",
		"mid | mid",
		"· | pure insertion",
		"tail | tail",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A filler cell must carry no line number, or the gutter invents a line
	// that is not in either file.
	if rows[2].Left.OldNo != 0 || rows[2].Left.Text != "" {
		t.Errorf("filler cell = %#v, want the zero Line", rows[2].Left)
	}
}

func TestIntraLineMarksTheChangedMiddle(t *testing.T) {
	rows := Split(splitFixture(t, "intraline"))

	r := rows[0]
	if got := r.Left.Text[r.LeftStart:r.LeftEnd]; got != "30" {
		t.Errorf("left mark = %q, want %q", got, "30")
	}
	if got := r.Right.Text[r.RightStart:r.RightEnd]; got != "45" {
		t.Errorf("right mark = %q, want %q", got, "45")
	}

	// The same, across a common prefix and suffix that are both multi-byte.
	u := rows[2]
	if got := u.Left.Text[u.LeftStart:u.LeftEnd]; got != "café" {
		t.Errorf("left mark = %q, want %q", got, "café")
	}
	if got := u.Right.Text[u.RightStart:u.RightEnd]; got != "CAFÉ" {
		t.Errorf("right mark = %q, want %q", got, "CAFÉ")
	}

	// A context row is not a pairing and gets no marks.
	if c := rows[1]; c.LeftStart != c.LeftEnd || c.RightStart != c.RightEnd {
		t.Errorf("context row carries marks: %#v", c)
	}
}

func TestIntraLineMarksAreDroppedWhenTheLinesAreUnrelated(t *testing.T) {
	rows := Split(splitFixture(t, "unrelated"))

	// "the quick brown fox jumps" against "zzz" shares nothing, so the marked
	// span is the whole of both. Lit end to end, the row reads as a rewrite of
	// a line that was simply replaced — which is why the guard exists.
	r := rows[1]
	if r.LeftStart != r.LeftEnd || r.RightStart != r.RightEnd {
		t.Errorf("marks survived on unrelated lines: left [%d,%d) right [%d,%d)",
			r.LeftStart, r.LeftEnd, r.RightStart, r.RightEnd)
	}

	// The guard must not swallow a genuine small edit inside a long line.
	k := rows[2]
	if got := k.Left.Text[k.LeftStart:k.LeftEnd]; got != "XX" {
		t.Errorf("left mark = %q, want %q — the guard fired on a real edit", got, "XX")
	}
	if got := k.Right.Text[k.RightStart:k.RightEnd]; got != "YY" {
		t.Errorf("right mark = %q, want %q", got, "YY")
	}
}

func TestIntraLineOffsetsNeverSplitARune(t *testing.T) {
	// Byte offsets are what the template slices with, so an offset landing
	// inside a UTF-8 sequence puts a replacement character on the page in place
	// of a letter. Checked over every fixture, not just the unicode one.
	for _, h := range allFixtureHunks(t) {
		for _, r := range Split(h) {
			check := func(side string, text string, start, end int) {
				t.Helper()
				if start < 0 || end < start || end > len(text) {
					t.Fatalf("%s offsets [%d,%d) out of range for %q", side, start, end, text)
				}
				if !utf8.ValidString(text[start:end]) ||
					!utf8.ValidString(text[:start]) ||
					!utf8.ValidString(text[end:]) {
					t.Errorf("%s offsets [%d,%d) split a rune in %q", side, start, end, text)
				}
			}
			check("left", r.Left.Text, r.LeftStart, r.LeftEnd)
			check("right", r.Right.Text, r.RightStart, r.RightEnd)
		}
	}
}

// TestSplitMatchesUnified is the invariant that stops the two views from ever
// telling different stories about the same patch. They are two renderings of
// one model, and a reviewer who toggles between them is entitled to see the
// same change.
//
// The comparison is per column, not row by row. A change block is transposed by
// construction — the unified view lists every deletion then every addition,
// while the split view interleaves them into pairs — so reading a row's left
// then right cell would compare a/c/b/d against a/b/c/d and fail on a correct
// implementation. What must hold is that the left column, read top to bottom,
// is exactly the unified view's deletions, and the right column exactly its
// additions.
func TestSplitMatchesUnified(t *testing.T) {
	for _, h := range allFixtureHunks(t) {
		var unifiedDel, unifiedAdd, unifiedCtx []Line
		for _, l := range h.Lines {
			switch l.Kind {
			case KindDel:
				unifiedDel = append(unifiedDel, l)
			case KindAdd:
				unifiedAdd = append(unifiedAdd, l)
			default:
				unifiedCtx = append(unifiedCtx, l)
			}
		}

		var splitDel, splitAdd, splitCtx []Line
		for _, r := range Split(h) {
			if r.HasLeft && r.HasRight && r.Left.Kind == KindContext {
				if r.Left != r.Right {
					t.Fatalf("context row shows different text on each side: %#v", r)
				}
				splitCtx = append(splitCtx, r.Left)
				continue
			}
			if r.HasLeft {
				splitDel = append(splitDel, r.Left)
			}
			if r.HasRight {
				splitAdd = append(splitAdd, r.Right)
			}
		}

		same := func(what string, a, b []Line) {
			t.Helper()
			if len(a) != len(b) {
				t.Fatalf("%s: split has %d, unified has %d", what, len(a), len(b))
			}
			for i := range a {
				if a[i] != b[i] {
					t.Fatalf("%s[%d]: split %#v, unified %#v", what, i, a[i], b[i])
				}
			}
		}
		same("deletions", splitDel, unifiedDel)
		same("additions", splitAdd, unifiedAdd)
		same("context", splitCtx, unifiedCtx)
	}
}

// allFixtureHunks is every hunk of every fixture in both corpora. testdata/ is
// the generated one and testdata/split/ the hand-written shapes; the invariant
// has to hold over both or it is only an invariant about the easy cases.
func allFixtureHunks(t *testing.T) []Hunk {
	t.Helper()
	var out []Hunk
	for _, pattern := range []string{"testdata/*.patch", "testdata/split/*.patch"} {
		names, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			b, err := os.ReadFile(n)
			if err != nil {
				t.Fatal(err)
			}
			files, err := Parse(string(b))
			if err != nil {
				continue // combined.patch, refused by design
			}
			for _, f := range files {
				out = append(out, f.Hunks...)
			}
		}
	}
	if len(out) < 130 {
		t.Fatalf("only %d hunks in the corpus; fixtures went missing", len(out))
	}
	return out
}
