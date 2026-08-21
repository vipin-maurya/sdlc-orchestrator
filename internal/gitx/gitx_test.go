package gitx

import "testing"

// git writes warnings to stderr and execx merges stderr into the captured
// output, so `status --porcelain` output can arrive with diagnostics mixed in.
// Parsing one as a path made a clean tree look dirty and put junk entries in
// the changed-file lists the fix-policy guard and the retry prompt read.
func TestIsPorcelainEntry(t *testing.T) {
	entries := []string{
		" M src/app.txt",
		"M  src/app.txt",
		"MM src/app.txt",
		"A  new.txt",
		"D  gone.txt",
		"?? untracked.txt",
		"!! ignored.txt",
		"R  old.txt -> new.txt",
		"UU conflicted.txt",
	}
	for _, ln := range entries {
		if !isPorcelainEntry(ln) {
			t.Errorf("entry rejected: %q", ln)
		}
	}

	notEntries := []string{
		"",
		"   ",
		"warning: in the working copy of 'src/app.txt', LF will be replaced by CRLF the next time Git touches it",
		"warning: LF will be replaced by CRLF in src/app.txt",
		"hint: Waiting for your editor to close the file...",
		"fatal: not a git repository",
		"   spaces.txt",
		"X? bogus.txt",
	}
	for _, ln := range notEntries {
		if isPorcelainEntry(ln) {
			t.Errorf("non-entry accepted: %q", ln)
		}
	}
}
