package diff

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

// FuzzParse is kept in the default seed-corpus run rather than behind a long
// -fuzztime, so every CI run exercises it and the cost stays flat. What it
// asserts is the contract the rest of the server leans on without checking:
// Parse returns, it returns only ErrCombinedDiff or nil, and nothing it hands
// back can crash a template. A page that panics halfway through rendering a
// patch is a 500 where a reviewer expected a diff, which is at least loud —
// but a File with an empty Path() is a row in the index with no name on it,
// which is not.
func FuzzParse(f *testing.F) {
	for _, pattern := range []string{"testdata/*.patch", "testdata/split/*.patch"} {
		names, err := filepath.Glob(pattern)
		if err != nil {
			f.Fatal(err)
		}
		for _, n := range names {
			b, err := os.ReadFile(n)
			if err != nil {
				f.Fatal(err)
			}
			f.Add(string(b))
		}
	}
	// Shapes the corpus cannot contain: headers with nothing after them, hunk
	// headers with no file, counts that overflow, and a lone prefix byte.
	for _, s := range []string{
		"",
		"\n",
		"\r\n",
		"diff --git ",
		"diff --git a/x",
		`diff --git "a/x`,
		"--- \n+++ \n@@ -1 +1 @@\n",
		"@@ -1,99999999999999999999 +1,1 @@\n foo\n",
		"@@ -0,0 +0,0 @@\n",
		"-",
		"\\",
		"index \nold mode \nnew mode \nrename from \nrename to \n",
		"diff --git a/x b/y\nGIT binary patch\nliteral 1\nzz\n",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, patch string) {
		files, err := Parse(patch)
		if err != nil {
			if !errors.Is(err, ErrCombinedDiff) {
				t.Fatalf("unexpected error %v", err)
			}
			if files != nil {
				t.Fatalf("files returned alongside an error")
			}
			return
		}
		for _, file := range files {
			if file.Path() == "" {
				t.Fatalf("file with no path: %#v", file)
			}
			rows := 0
			for _, h := range file.Hunks {
				rows += len(h.Lines)
				for _, l := range h.Lines {
					if l.OldNo < 0 || l.NewNo < 0 {
						t.Fatalf("negative line number: %#v", l)
					}
				}
				// Split runs on whatever Parse produced, so it is fuzzed with
				// it rather than separately.
				for _, r := range Split(h) {
					checkSpan(t, r.Left.Text, r.LeftStart, r.LeftEnd)
					checkSpan(t, r.Right.Text, r.RightStart, r.RightEnd)
				}
			}
			if file.Rows() != rows {
				t.Fatalf("Rows() = %d, counted %d", file.Rows(), rows)
			}
		}
	})
}

func checkSpan(t *testing.T, text string, start, end int) {
	t.Helper()
	if start < 0 || end < start || end > len(text) {
		t.Fatalf("span [%d,%d) out of range for %q", start, end, text)
	}
	if !utf8.ValidString(text) {
		return // garbage in, garbage out; only the offsets are ours
	}
	if !utf8.ValidString(text[start:end]) {
		t.Fatalf("span [%d,%d) splits a rune in %q", start, end, text)
	}
}
