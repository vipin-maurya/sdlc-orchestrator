package server

// The diff viewer (spec §8). Owner: W2-K.
//
// Both views are rendered here, from one []*diff.File, by the server. There is
// no second parser and no client-side re-parse: a unified view and a split view
// that disagreed about what changed would be two answers to the question the
// reviewer is actually asking, and only one of them could be right.
//
// Nothing in this file hands the template a pre-escaped type. A patch is
// arbitrary text written by whoever wrote the code — the one input on this
// server that is guaranteed not to be trustworthy — so every byte of it reaches
// the page through html/template's ordinary escaping, and the intra-line marks
// are made of template structure rather than of markup this file concatenated.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/diff"
	"github.com/vipinm/sdlc-orchestrator/internal/gitx"
	"github.com/vipinm/sdlc-orchestrator/internal/guard"
	"github.com/vipinm/sdlc-orchestrator/internal/review"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// Spec §5.4's two diff bounds, both counted in diff.File.Rows().
const (
	// diffRowBudget is how many diff lines the whole page will render eagerly.
	// Past it the remaining files become stubs linking to their own page: a
	// 40 000-line patch then degrades to something navigable instead of a tab
	// that never finishes laying out.
	diffRowBudget = 20000
	// collapseOver is the size at which one file starts collapsed. It is a
	// default, not a limit — the <details> is still there to open.
	collapseOver = 500
)

const (
	viewUnified = "unified"
	viewSplit   = "split"
	// diffViewCookie is written by POST /prefs/diff-view (W2-I) and only read
	// here.
	diffViewCookie = "sdlc_diff_view"
)

// patchSource is the one thing this page needs from git. It is an interface
// with one method, and openPatchSource is a variable, for the same reason
// review.openDiffSource is: the tests must be able to serve a patch without a
// repository on disk, and every interesting case here — a truncated capture, a
// binary file, a malformed hunk — is one that is tedious to arrange with real
// git and trivial to state as a string.
type patchSource interface {
	DiffPatchSince(ctx context.Context, dir, sha string) (patch string, truncated bool, err error)
}

// openPatchSource is never reassigned in production; tests replace it, and
// never in parallel.
var openPatchSource = func(repoPath string) patchSource { return gitx.Repo{Root: repoPath} }

// patchTimeout bounds the git call behind a page load. gitx has its own
// timeout; this one exists so a request cannot outlive the reader's patience by
// more than this even if the process hangs before git does.
const patchTimeout = 2 * time.Minute

// patch is the patch plus everything the page has to say about where it came
// from. Truncated is carried from DiffPatchSince rather than inferred from
// len(Patch): the reviewer approving a merge from a clipped patch is deciding
// about a change they have not seen, and a length heuristic gets that wrong in
// both directions — a patch that happens to land on the cap reads as truncated,
// and a cap that later changes reads as complete.
type patchResult struct {
	Patch     string
	Truncated bool
	Persisted bool   // the copy review.Write left on disk, not a live render
	Note      string // why there is no patch, or where this one came from
}

// patchFor reads the job's patch, preferring a live render.
//
// It does not go through gateDoc: that renders a gate document, and a gate
// document exists only while a job is parked at a gate. The diff page is
// reachable for a job in any state — including one that is running, merged or
// held — and a diff viewer that answered "nothing is waiting on you" for a job
// mid-flight would be useless exactly when somebody wants to watch it work.
//
// The persisted <gate>.diff is the fallback and not the default, because it is
// a copy of what the branch looked like when the job parked. When the worktree
// is gone (git.cleanup_worktrees) that copy is the only patch there is, and the
// page says which of the two the reader has.
func (s *Server) patchFor(ctx context.Context, j *store.Job, t config.Target) patchResult {
	base := j.Counters.BaseSHA
	if base == "" {
		return patchResult{Note: "this job has no base commit recorded yet, so there is nothing to diff against."}
	}
	if _, err := os.Stat(j.WorktreePath); err != nil {
		if p, ok := s.persistedPatch(j); ok {
			return patchResult{Patch: p, Persisted: true, Note: fmt.Sprintf(
				"worktree %s is gone; this is the patch written when the job parked, not a live render.", j.WorktreePath)}
		}
		return patchResult{Note: fmt.Sprintf("worktree %s is gone; nothing to diff.", j.WorktreePath)}
	}
	dctx, cancel := context.WithTimeout(ctx, patchTimeout)
	defer cancel()
	p, truncated, err := openPatchSource(t.RepoPath).DiffPatchSince(dctx, j.WorktreePath, base)
	if err != nil {
		if p, ok := s.persistedPatch(j); ok {
			return patchResult{Patch: p, Persisted: true, Note: fmt.Sprintf(
				"the live diff failed (%v); this is the patch written when the job parked.", err)}
		}
		return patchResult{Note: fmt.Sprintf("the diff is unavailable: %v", err)}
	}
	return patchResult{Patch: p, Truncated: truncated}
}

// persistedPatch reads the <gate>.diff review.Write leaves beside the gate
// document. The job's own gate is tried first and the rest in the order a
// reviewer would want them: the merge gate's patch is the whole change, the
// code gate's is the same change one gate earlier, and hold's is whatever the
// branch held when it stopped.
func (s *Server) persistedPatch(j *store.Job) (string, bool) {
	gates := []string{review.GateMerge, review.GateCode, review.GateHold}
	if g := review.GateFor(j.State); g != "" {
		gates = append([]string{g}, gates...)
	}
	for _, g := range gates {
		b, err := os.ReadFile(review.DiffPath(s.cfg.Orchestrator.DataDir, j.ID, g))
		if err == nil && len(b) > 0 {
			return string(b), true
		}
	}
	return "", false
}

// --- the page model ------------------------------------------------------

// diffToggle is exactly what _forms.html's diffViewToggle reads. It is a type
// here rather than a field on the page because that template is W2-I's and it
// takes its own dot: passing the whole page would tie its shape to this one's.
type diffToggle struct {
	CSRF     string
	DiffView string
}

type diffPage struct {
	Job    *store.Job
	View   string // "unified" | "split"
	Split  bool
	Toggle diffToggle
	Files  []*fileView
	Adds   int
	Dels   int

	Truncated bool
	Persisted bool
	Note      string
	ParseErr  string
	Empty     bool
	Stubs     int

	// Single is set on /jobs/{id}/diff/{index}, where there is one file, no
	// budget and no file index.
	Single bool
	// Focus is the ?file= index, or -1. The focused file is expanded and
	// rendered whatever the budget says: it is the file the reader asked for.
	Focus int
}

type fileView struct {
	Index    int
	Path     string
	OldPath  string
	NewPath  string
	Status   string
	Adds     int
	Dels     int
	Binary   bool
	Renamed  bool
	ModeOnly bool
	OldMode  string
	NewMode  string
	// Malformed is diff.File.Malformed: the parser understood some of this
	// file and not all of it, and the page must say so rather than render the
	// half it got as though it were the whole.
	Malformed string
	IsTest    bool
	Rows      int
	Open      bool
	// Stub means the row budget was spent before this file: it renders as a
	// header and a link to its own page.
	Stub bool
	// Split is the page's view, carried down so the file's template can pick a
	// rendering without reaching back up to the page.
	Split bool

	Hunks []hunkView
}

// hunkView carries both renderings. Only the one the current view needs is
// filled — building both would double the work on every page for a reader who
// asked to see one of them.
type hunkView struct {
	Header  string
	Section string
	Lines   []lineView // unified
	Rows    []rowView  // split
}

type lineView struct {
	Class     string // "add" | "del" | "ctx"
	Sign      string
	Old       string // "" when the line does not exist on that side
	New       string
	Text      string
	NoNewline bool
}

type rowView struct{ Left, Right cellView }

// cellView is one side of a split row, already sliced into the three parts the
// template needs. Mark is the intra-line change span; when it is empty the
// template emits no <span> at all, which is what "nothing is marked" looks
// like. The slicing happens here, in Go, because the alternative — handing the
// template offsets and letting it build the markup — is the concatenation this
// file exists to avoid.
type cellView struct {
	Has       bool
	Class     string // "add" | "del" | "ctx" | "nil"
	No        string
	Pre       string
	Mark      string
	Post      string
	NoNewline bool
}

// --- handlers ------------------------------------------------------------

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	t, ok := s.target(w, r, j)
	if !ok {
		return
	}
	res := s.patchFor(r.Context(), j, t)
	p := s.newDiffPage(r, j, res)
	if p.ParseErr == "" && !p.Empty {
		s.fillFiles(p, res)
	}
	s.render(w, r, "diff.html", s.page(r, "Diff — "+j.ID, "jobs", p))
}

func (s *Server) handleDiffFile(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	t, ok := s.target(w, r, j)
	if !ok {
		return
	}
	idx, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "a file index is a number")
		return
	}
	res := s.patchFor(r.Context(), j, t)
	p := s.newDiffPage(r, j, res)
	p.Single = true
	if p.ParseErr != "" || p.Empty {
		s.render(w, r, "difffile.html", s.page(r, "Diff — "+j.ID, "jobs", p))
		return
	}
	files, err := s.diffFor(j, res.Patch)
	if err != nil {
		p.ParseErr = err.Error()
		s.render(w, r, "difffile.html", s.page(r, "Diff — "+j.ID, "jobs", p))
		return
	}
	if idx < 0 || idx >= len(files) {
		s.fail(w, r, http.StatusNotFound, fmt.Sprintf("this patch has %d files, so there is no file %d", len(files), idx))
		return
	}
	// No budget on this page: it is the escape hatch the budget's stubs link
	// to, so refusing to render the file here would leave the reader nowhere
	// left to go.
	fv := s.fileView(idx, files[idx], p.Split, true)
	fv.IsTest = matchesAny(s.testGlobs(), files[idx].Path())
	fv.Open = true
	p.Files = []*fileView{fv}
	p.Adds, p.Dels = fv.Adds, fv.Dels
	s.render(w, r, "difffile.html", s.page(r, fv.Path+" — "+j.ID, "jobs", p))
}

// handleDiffPatch serves the patch byte for byte. It carries no banner and no
// wrapper: this is the endpoint a reviewer pipes into `git apply` or their own
// tools, and a page that helpfully prepended a warning would corrupt every one
// of them. The truncation warning belongs on the page a human reads, which is
// where it is.
func (s *Server) handleDiffPatch(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	t, ok := s.target(w, r, j)
	if !ok {
		return
	}
	res := s.patchFor(r.Context(), j, t)
	if res.Patch == "" && res.Note != "" {
		// Not 200 with an empty body: an empty patch and an unobtainable one
		// are different facts, and a tool handed the second as the first
		// reports "no changes" about a change it never saw. Not 404 either —
		// the route and the job both exist, and what is missing is a patch this
		// job's state cannot produce, which is what 409 says.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintln(w, res.Note)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// inline, not an attachment: the reviewer asked to read it, and a browser
	// that offers a download instead makes `curl`-shaped work out of looking.
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fmt.Fprint(w, res.Patch)
}

// --- model building ------------------------------------------------------

func (s *Server) newDiffPage(r *http.Request, j *store.Job, res patchResult) *diffPage {
	view := diffViewFrom(r)
	p := &diffPage{
		Job:       j,
		View:      view,
		Split:     view == viewSplit,
		Truncated: res.Truncated,
		Persisted: res.Persisted,
		Note:      res.Note,
		Empty:     strings.TrimSpace(res.Patch) == "",
		Focus:     -1,
		Toggle:    diffToggle{CSRF: csrfToken(r), DiffView: view},
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("file")); err == nil && n >= 0 {
		p.Focus = n
	}
	return p
}

func (s *Server) fillFiles(p *diffPage, res patchResult) {
	files, err := s.diffFor(p.Job, res.Patch)
	if err != nil {
		p.ParseErr = err.Error()
		return
	}
	tests := s.testGlobs()
	spent, stubbing := 0, false
	for i, f := range files {
		rows := f.Rows()
		focused := i == p.Focus
		render := focused || (!stubbing && spent+rows <= diffRowBudget)
		if !render {
			// Every later file stubs too. Rendering a small file that happens
			// to follow a huge one would put the budget's effect in a place
			// nobody can predict from the page.
			stubbing = true
		}
		fv := s.fileView(i, f, p.Split, render)
		fv.IsTest = matchesAny(tests, f.Path())
		if render {
			spent += rows
			fv.Open = focused || rows <= collapseOver
		} else {
			p.Stubs++
		}
		p.Adds += f.Adds
		p.Dels += f.Dels
		p.Files = append(p.Files, fv)
	}
}

// testGlobs compiles policies.test_file_globs. A glob the operator wrote badly
// costs the badge and nothing else — the diff itself is what this page is for,
// and refusing to render it over a bad pattern would be a strange trade.
func (s *Server) testGlobs() guard.GlobSet {
	set, err := guard.CompileGlobs(s.cfg.Policies.TestFileGlobs)
	if err != nil {
		s.log.Printf("policies.test_file_globs does not compile, so no test badges: %v", err)
		return nil
	}
	return set
}

func matchesAny(set guard.GlobSet, path string) bool {
	for _, g := range set {
		if g.Match(path) {
			return true
		}
	}
	return false
}

func (s *Server) fileView(i int, f *diff.File, split, render bool) *fileView {
	fv := &fileView{
		Index:     i,
		Path:      sanitize(f.Path()),
		OldPath:   sanitize(f.OldPath),
		NewPath:   sanitize(f.NewPath),
		Status:    string(f.Status),
		Adds:      f.Adds,
		Dels:      f.Dels,
		Binary:    f.Binary,
		Renamed:   f.Status == diff.StatusRenamed || f.Status == diff.StatusCopied,
		ModeOnly:  f.Status == diff.StatusModeOnly,
		OldMode:   f.OldMode,
		NewMode:   f.NewMode,
		Malformed: sanitize(f.Malformed),
		Rows:      f.Rows(),
		Stub:      !render,
		Split:     split,
	}
	if !render || f.Binary {
		// A binary file has no hunks to render and never gets bytes on the
		// page: "binary file changed" is the whole truth about it.
		return fv
	}
	for _, h := range f.Hunks {
		hv := hunkView{Header: hunkHeader(h), Section: sanitize(h.Section)}
		if split {
			for _, row := range diff.Split(h) {
				hv.Rows = append(hv.Rows, rowView{
					Left:  cell(row.Left, row.HasLeft, row.LeftStart, row.LeftEnd, true),
					Right: cell(row.Right, row.HasRight, row.RightStart, row.RightEnd, false),
				})
			}
		} else {
			for _, l := range h.Lines {
				hv.Lines = append(hv.Lines, lineView{
					Class:     kindClass(l.Kind),
					Sign:      kindSign(l.Kind),
					Old:       lineNo(l.OldNo),
					New:       lineNo(l.NewNo),
					Text:      sanitize(l.Text),
					NoNewline: l.NoNewline,
				})
			}
		}
		fv.Hunks = append(fv.Hunks, hv)
	}
	return fv
}

// cell slices one side of a split row into before/marked/after.
//
// start and end are byte offsets into Line.Text, computed on rune boundaries
// for valid UTF-8. They are still clamped: Row is an exported struct with
// exported int fields, and a slice expression out of range in a request path is
// a panic where a page should be. Each piece is then run through sanitize
// separately, after the slicing, so a bad byte cannot move an offset.
func cell(l diff.Line, has bool, start, end int, left bool) cellView {
	if !has {
		return cellView{Class: "nil"}
	}
	c := cellView{Has: true, Class: kindClass(l.Kind), NoNewline: l.NoNewline}
	if left {
		c.No = lineNo(l.OldNo)
	} else {
		c.No = lineNo(l.NewNo)
	}
	n := len(l.Text)
	if start < 0 || end > n || start > end {
		start, end = 0, 0
	}
	c.Pre = sanitize(l.Text[:start])
	c.Mark = sanitize(l.Text[start:end])
	c.Post = sanitize(l.Text[end:])
	return c
}

// sanitize makes a string safe to print as text, which here means only one
// thing: valid UTF-8. The parser reproduces whatever bytes were in the patch,
// and a patch of a latin-1 source file contains bytes that are not UTF-8 at
// all; written into a UTF-8 page they produce a document the browser has to
// guess at. Escaping is html/template's job and is not duplicated here — this
// function must never be the reason a caller thinks it can skip it.
func sanitize(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}

func hunkHeader(h diff.Hunk) string {
	return fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
}

func kindClass(k diff.Kind) string {
	switch k {
	case diff.KindAdd:
		return "add"
	case diff.KindDel:
		return "del"
	}
	return "ctx"
}

func kindSign(k diff.Kind) string {
	switch k {
	case diff.KindAdd:
		return "+"
	case diff.KindDel:
		return "-"
	}
	return " "
}

// lineNo renders 0 — "this line does not exist on this side" — as an empty
// gutter rather than as the number zero, which no file has.
func lineNo(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// diffViewFrom resolves the view: the query string first because it is what the
// reader just clicked, then the cookie, then unified. An unrecognised value
// falls through rather than failing — ?view=nonsense is a typo or a stale
// bookmark, not a reason to refuse to show somebody their diff.
func diffViewFrom(r *http.Request) string {
	if v := r.URL.Query().Get("view"); v == viewSplit || v == viewUnified {
		return v
	}
	if c, err := r.Cookie(diffViewCookie); err == nil {
		if c.Value == viewSplit || c.Value == viewUnified {
			return c.Value
		}
	}
	return viewUnified
}
