// Package gitx wraps the git operations the orchestrator owns (SPEC §7):
// branch + worktree provisioning, commits on behalf of agents, diff
// inspection for policy checks, rebase/merge/push, and cleanup. Agents never
// run these; every mutation of history goes through this package.
package gitx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/execx"
)

type Repo struct {
	// Root is the main checkout (targets.<t>.repo_path).
	Root string
}

const gitTimeout = 5 * time.Minute

func (r Repo) git(ctx context.Context, dir string, args ...string) (string, error) {
	if dir == "" {
		dir = r.Root
	}
	res, out, err := execx.RunCapture(ctx, execx.Cmd{
		Argv:    append([]string{"git"}, args...),
		Dir:     dir,
		Timeout: gitTimeout,
		// Callers parse this output. git puts warnings on stderr — notably
		// the Windows "LF will be replaced by CRLF" notice — and a merged
		// stream turns those into phantom filenames. Failures still carry
		// stderr through for the error message below.
		StderrSeparate: true,
	}, 1<<20)
	if err != nil {
		return out, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if res.ExitCode != 0 {
		return out, fmt.Errorf("git %s (exit %d): %s", strings.Join(args, " "), res.ExitCode, strings.TrimSpace(out))
	}
	return out, nil
}

// GitOK reports whether Root is a git work tree.
func (r Repo) GitOK(ctx context.Context) error {
	_, err := r.git(ctx, "", "rev-parse", "--is-inside-work-tree")
	return err
}

// EnsureWorktree creates branch (from base) and a worktree at path, and makes
// sure the worktrees dir is excluded via .git/info/exclude. Idempotent: an
// existing worktree at path for the same branch is reused.
func (r Repo) EnsureWorktree(ctx context.Context, path, branch, base string) error {
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		if _, err := r.git(ctx, path, "rev-parse", "--is-inside-work-tree"); err == nil {
			return nil
		}
	}
	if err := r.excludeFromStatus(path); err != nil {
		return err
	}
	// Create the branch if missing.
	if _, err := r.git(ctx, "", "rev-parse", "--verify", "refs/heads/"+branch); err != nil {
		if _, err := r.git(ctx, "", "branch", branch, base); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_, err := r.git(ctx, "", "worktree", "add", path, branch)
	return err
}

// excludeFromStatus adds the worktree parent dir to .git/info/exclude so the
// main checkout's status stays clean.
func (r Repo) excludeFromStatus(worktreePath string) error {
	rel, err := filepath.Rel(r.Root, filepath.Dir(worktreePath))
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil // worktrees dir lives outside the repo; nothing to exclude
	}
	gitDir := filepath.Join(r.Root, ".git")
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		return nil
	}
	exclude := filepath.Join(gitDir, "info", "exclude")
	line := "/" + filepath.ToSlash(rel) + "/"
	data, _ := os.ReadFile(exclude)
	if strings.Contains(string(data), line) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(exclude, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\n", line)
	return err
}

// ExcludePattern appends a pattern to .git/info/exclude (shared by all
// worktrees of the repo). Used for the .sdlc/ exchange directory.
func (r Repo) ExcludePattern(pattern string) error {
	gitDir := filepath.Join(r.Root, ".git")
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		return nil
	}
	exclude := filepath.Join(gitDir, "info", "exclude")
	data, _ := os.ReadFile(exclude)
	for _, ln := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(ln) == pattern {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(exclude, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\n", pattern)
	return err
}

func (r Repo) RemoveWorktree(ctx context.Context, path string) error {
	_, err := r.git(ctx, "", "worktree", "remove", "--force", path)
	return err
}

func (r Repo) DeleteBranch(ctx context.Context, branch string) error {
	_, err := r.git(ctx, "", "branch", "-D", branch)
	return err
}

func (r Repo) HeadSHA(ctx context.Context, dir string) (string, error) {
	out, err := r.git(ctx, dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// IsDirty reports whether the worktree has uncommitted changes (staged,
// unstaged, or untracked). It goes through DirtyFiles so a git warning on
// stderr cannot be mistaken for uncommitted work — a false positive here
// would fail an innocent reviewer state under reviewer_diff_must_be_empty.
// IsAncestor reports whether commit a is an ancestor of commit b (a commit
// is its own ancestor). It is how the orchestrator tells "the branch moved
// forward under me" from "the branch diverged": the first is safe to adopt,
// the second must never be resolved by discarding one side.
func (r Repo) IsAncestor(ctx context.Context, dir, a, b string) (bool, error) {
	if a == "" || b == "" {
		return false, fmt.Errorf("IsAncestor: empty sha (a=%q b=%q)", a, b)
	}
	if a == b {
		return true, nil
	}
	if dir == "" {
		dir = r.Root
	}
	res, _, err := execx.RunCapture(ctx, execx.Cmd{
		Argv:           []string{"git", "merge-base", "--is-ancestor", a, b},
		Dir:            dir,
		Timeout:        gitTimeout,
		StderrSeparate: true,
	}, 1<<16)
	if err != nil {
		return false, err
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		// 128 = one of the shas is unknown to this repository.
		return false, fmt.Errorf("merge-base --is-ancestor %s %s: exit %d", a, b, res.ExitCode)
	}
}

func (r Repo) IsDirty(ctx context.Context, dir string) (bool, error) {
	files, err := r.DirtyFiles(ctx, dir)
	if err != nil {
		return false, err
	}
	return len(files) > 0, nil
}

// porcelainCodes are the status characters git uses in `status --porcelain`
// (v1) index/worktree columns.
const porcelainCodes = " MADRCUT?!"

// isPorcelainEntry reports whether ln is a real `status --porcelain` entry
// rather than a diagnostic. StderrSeparate above already keeps git's warnings
// (on Windows, most often "in the working copy of 'x', LF will be replaced by
// CRLF") out of this stream; this is the second line of defence, since one
// stray line here becomes a phantom changed file in the fix-policy guard, the
// implementation post-condition, and the retry prompt.
func isPorcelainEntry(ln string) bool {
	if len(ln) < 4 || ln[2] != ' ' {
		return false
	}
	return strings.IndexByte(porcelainCodes, ln[0]) >= 0 &&
		strings.IndexByte(porcelainCodes, ln[1]) >= 0 &&
		!(ln[0] == ' ' && ln[1] == ' ')
}

// DirtyFiles lists paths (repo-relative, slash-separated) with uncommitted
// changes of any kind.
func (r Repo) DirtyFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := r.git(ctx, dir, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if !isPorcelainEntry(ln) {
			continue
		}
		p := strings.TrimSpace(ln[3:])
		if i := strings.Index(p, " -> "); i >= 0 { // rename: keep destination
			p = p[i+4:]
		}
		p = strings.Trim(p, `"`)
		files = append(files, p)
	}
	return files, nil
}

// ResetHardClean discards all uncommitted work: reset --hard <sha> (or HEAD)
// plus clean -fd. Used by crash-resume reconciliation (restartable states).
func (r Repo) ResetHardClean(ctx context.Context, dir, sha string) error {
	target := sha
	if target == "" {
		target = "HEAD"
	}
	if _, err := r.git(ctx, dir, "reset", "--hard", target); err != nil {
		return err
	}
	_, err := r.git(ctx, dir, "clean", "-fd")
	return err
}

// CommitAll stages everything and commits. Returns the new HEAD sha. If there
// is nothing to commit it returns the current HEAD and no error.
func (r Repo) CommitAll(ctx context.Context, dir, message string) (string, error) {
	if _, err := r.git(ctx, dir, "add", "-A"); err != nil {
		return "", err
	}
	dirty, err := r.IsDirty(ctx, dir)
	if err != nil {
		return "", err
	}
	if dirty {
		if _, err := r.git(ctx, dir, "commit", "-m", message); err != nil {
			return "", err
		}
	}
	return r.HeadSHA(ctx, dir)
}

// DiffNamesSince lists files changed between sha and the current worktree
// (committed and uncommitted), repo-relative slash paths.
func (r Repo) DiffNamesSince(ctx context.Context, dir, sha string) ([]string, error) {
	out, err := r.git(ctx, dir, "diff", "--name-only", sha)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var files []string
	add := func(p string) {
		p = strings.TrimSpace(strings.Trim(p, `"`))
		if p != "" && !seen[p] {
			seen[p] = true
			files = append(files, p)
		}
	}
	for _, ln := range strings.Split(out, "\n") {
		add(ln)
	}
	dirtyList, err := r.DirtyFiles(ctx, dir)
	if err != nil {
		return nil, err
	}
	for _, p := range dirtyList {
		add(p)
	}
	return files, nil
}

// DiffPatchSince returns the full unified diff between sha and the worktree
// (bounded to ~4MB). Staged for reviewers so they never need shell access.
func (r Repo) DiffPatchSince(ctx context.Context, dir, sha string) (string, error) {
	res, out, err := execx.RunCapture(ctx, execx.Cmd{
		Argv:           []string{"git", "diff", sha},
		Dir:            dir,
		Timeout:        gitTimeout,
		StderrSeparate: true, // reviewers read this patch; keep warnings out of it
	}, 4<<20)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("git diff %s (exit %d): %s", sha, res.ExitCode, strings.TrimSpace(out))
	}
	return out, nil
}

// DiffStatSince renders a human diff stat between sha and HEAD for approval
// display.
func (r Repo) DiffStatSince(ctx context.Context, dir, sha string) (string, error) {
	out, err := r.git(ctx, dir, "diff", "--stat", sha)
	return out, err
}

func (r Repo) HasRemote(ctx context.Context) (bool, error) {
	out, err := r.git(ctx, "", "remote")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func (r Repo) Fetch(ctx context.Context) error {
	_, err := r.git(ctx, "", "fetch", "--all", "--prune")
	return err
}

// RebaseOnto rebases the branch checked out in worktree dir onto base.
// On conflict the rebase is aborted and an error returned.
func (r Repo) RebaseOnto(ctx context.Context, dir, base string) error {
	if _, err := r.git(ctx, dir, "rebase", base); err != nil {
		_, _ = r.git(ctx, dir, "rebase", "--abort")
		return fmt.Errorf("rebase onto %s failed (aborted): %w", base, err)
	}
	return nil
}

// MergeNoFF merges branch into the branch currently checked out at Root
// (expected: the default branch). Refuses if Root's checkout is dirty.
func (r Repo) MergeNoFF(ctx context.Context, branch, message string) error {
	dirty, err := r.IsDirty(ctx, r.Root)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("main checkout %s is dirty; refusing to merge", r.Root)
	}
	_, err = r.git(ctx, r.Root, "merge", "--no-ff", "-m", message, branch)
	if err != nil {
		_, _ = r.git(ctx, r.Root, "merge", "--abort")
		return err
	}
	return nil
}

func (r Repo) CheckoutBranch(ctx context.Context, branch string) error {
	_, err := r.git(ctx, r.Root, "checkout", branch)
	return err
}

func (r Repo) CurrentBranch(ctx context.Context, dir string) (string, error) {
	out, err := r.git(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out), err
}

// Push pushes the given branch to origin. Never force.
func (r Repo) Push(ctx context.Context, branch string) error {
	_, err := r.git(ctx, r.Root, "push", "origin", branch)
	return err
}
