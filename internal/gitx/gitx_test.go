package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

// git drives a real temp repo here: the point of the test is what git's own
// diff output does to the capture cap, which a fake would have to assume.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func tempRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, tmp, "init", "-b", "main", "repo")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "app.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")
	return repo
}

// A patch that hit the 4 MiB cap and a patch that happens to be 4 MiB long are
// the same string, so length cannot tell them apart. A merge gate that showed
// the head of a clipped patch without saying so would put a reviewer's name on
// a change they never saw.
func TestDiffPatchSinceReportsTruncation(t *testing.T) {
	repo := tempRepo(t)
	r := Repo{Root: repo}
	ctx := context.Background()

	base := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))

	// A small edit stays well inside the cap.
	if err := os.WriteFile(filepath.Join(repo, "app.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-am", "small change")
	patch, truncated, err := r.DiffPatchSince(ctx, repo, base)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Errorf("truncated = true for a %d-byte patch", len(patch))
	}
	if !strings.Contains(patch, "+v2") {
		t.Errorf("patch missing the change:\n%s", patch)
	}

	// Now a file whose added lines alone overrun the cap. Plain ASCII, so git
	// diffs it as text rather than reporting "Binary files differ" — which
	// would produce a two-line patch and quietly pass this test.
	const big = 5 << 20
	line := strings.Repeat("y", 63) + "\n"
	var sb strings.Builder
	for sb.Len() < big {
		sb.WriteString(line)
	}
	if err := os.WriteFile(filepath.Join(repo, "big.txt"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "big file")

	patch, truncated, err = r.DiffPatchSince(ctx, repo, base)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Errorf("truncated = false for a patch of %d bytes over a %d-byte source", len(patch), sb.Len())
	}
	if len(patch) != 4<<20 {
		t.Errorf("patch is %d bytes, want exactly the 4 MiB cap", len(patch))
	}
}
