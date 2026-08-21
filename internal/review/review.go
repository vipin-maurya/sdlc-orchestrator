// Package review renders what a human is being asked to decide at a gate.
//
// It exists because "the job is parked" is not reviewable information. A gate
// is a request for a judgement, and a judgement needs the thing being judged
// in front of the person making it: the spec and plan before any code exists,
// the diff and the reviewer's findings once it does, the hold reason and the
// trail when a job escalated. One renderer serves both readers — the engine
// writes the document to disk when a job parks, and `sdlc review` prints the
// same text — so what the operator reads in the terminal and what they open
// in an editor can never drift apart.
package review

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
	"github.com/vipinm/sdlc-orchestrator/internal/gitx"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

// Gate names. These are the values written to approvals.gate, and the
// filenames of the rendered documents.
const (
	GateSpec    = "spec"
	GateCode    = "code"
	GateMerge   = "merge"
	GateRelease = "release"
	// GateHold is not an approval gate: a held job is cleared with resume or
	// cancel, not approve/reject. It renders through the same path because the
	// operator's question is identical — what happened, and what do I do.
	GateHold = "hold"
)

// Options describes one rendering. Everything the renderer needs is passed in
// rather than read from config, so the CLI and the engine can call it with
// whatever they already hold.
type Options struct {
	Job      *store.Job
	Gate     string
	DataDir  string
	RepoPath string
	// ShipCommand is shown at the release gate: the operator is approving the
	// running of this command, so it has to be visible before they do.
	ShipCommand []string
	// DefaultBranch is the merge destination shown at the merge gate.
	DefaultBranch string
	// FullDiff inlines the whole patch. Off by default: at the merge gate the
	// patch is routinely thousands of lines, and burying the findings under it
	// is its own kind of invisibility.
	FullDiff bool
	// Events, when non-nil, are the job's events; the hold document quotes the
	// tail of them. The caller supplies them so this package never needs a
	// store handle.
	Events []*store.Event
}

// Doc is a rendered gate document.
type Doc struct {
	Gate string
	// Title is the one-line statement of what is being decided.
	Title string
	// Body is markdown: readable in a terminal, and openable as a file.
	Body string
	// Diff is the full patch when one was produced ("" otherwise). It is kept
	// out of Body so the caller can write it to its own file.
	Diff string
	// Blocking counts findings that were at or above the gate threshold before
	// the pipeline let the job through — always 0 in practice, but a non-zero
	// value would mean a threshold was lowered, which the operator should see.
	Findings int
}

// GateFor maps a job state to the gate a human must decide, "" when the job
// is not waiting on anybody.
func GateFor(state string) string {
	switch state {
	case "AWAITING_SPEC_APPROVAL":
		return GateSpec
	case "AWAITING_CODE_APPROVAL":
		return GateCode
	case "AWAITING_MERGE_APPROVAL":
		return GateMerge
	case "AWAITING_RELEASE_APPROVAL":
		return GateRelease
	case "ESCALATED", "TIMED_OUT":
		return GateHold
	}
	return ""
}

// Dir is where rendered documents live: <data_dir>/jobs/<id>/review.
func Dir(dataDir, jobID string) string {
	return filepath.Join(artifact.Dir(dataDir, jobID), "review")
}

// DocPath is the rendered document for one gate.
func DocPath(dataDir, jobID, gate string) string {
	return filepath.Join(Dir(dataDir, jobID), gate+".md")
}

// DiffPath is the patch written alongside a document that has one.
func DiffPath(dataDir, jobID, gate string) string {
	return filepath.Join(Dir(dataDir, jobID), gate+".diff")
}

// Write renders and persists the document (and its patch, when there is one),
// returning the document path. This is what the engine calls when a job parks,
// so the artifact exists before anybody asks for it — including for a job that
// parked overnight on a machine whose worktree has since been cleaned up.
func Write(ctx context.Context, o Options) (string, error) {
	doc, err := Render(ctx, o)
	if err != nil {
		return "", err
	}
	dir := Dir(o.DataDir, o.Job.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	docPath := DocPath(o.DataDir, o.Job.ID, doc.Gate)
	if err := os.WriteFile(docPath, []byte(doc.Body), 0o644); err != nil {
		return "", err
	}
	if doc.Diff != "" {
		if err := os.WriteFile(DiffPath(o.DataDir, o.Job.ID, doc.Gate), []byte(doc.Diff), 0o644); err != nil {
			return "", err
		}
	}
	return docPath, nil
}

// Render builds the document for o.Gate. Every source it reads is optional:
// a missing artifact, an unreadable worktree or a repo that has moved produce
// a document that says so rather than an error, because a gate the operator
// cannot read is worse than one with a hole in it.
func Render(ctx context.Context, o Options) (*Doc, error) {
	if o.Job == nil {
		return nil, fmt.Errorf("review: no job")
	}
	gate := o.Gate
	if gate == "" {
		gate = GateFor(o.Job.State)
	}
	if gate == "" {
		return nil, fmt.Errorf("%s is in state %s — nothing is waiting on you", o.Job.ID, o.Job.State)
	}
	d := &Doc{Gate: gate}
	var b strings.Builder
	art := artifact.ArtifactsDir(o.DataDir, o.Job.ID)

	d.Title = gateTitle(gate, o)
	fmt.Fprintf(&b, "# %s — %s\n\n", o.Job.ID, o.Job.IssueTitle)
	fmt.Fprintf(&b, "**%s**\n\n", d.Title)
	fmt.Fprintf(&b, "- state: `%s` (waiting %s)\n", o.Job.State, humanSince(o.Job.StateEnteredAt))
	fmt.Fprintf(&b, "- branch: `%s`\n", o.Job.Branch)
	fmt.Fprintf(&b, "- worktree: `%s`\n", o.Job.WorktreePath)
	fmt.Fprintf(&b, "- artifacts: `%s`\n", art)
	b.WriteString("\n")

	switch gate {
	case GateSpec:
		renderIssue(&b, o.Job)
		renderSpec(&b, art)
		renderPlan(&b, art)
		d.Findings += renderReview(&b, art, latestReview(art, "design_review"), "Design review")
	case GateCode:
		renderImplementation(&b, art, "implementation.json", "Implementation")
		d.Findings += renderReview(&b, art, latestReview(art, "code_review"), "Code review")
		renderDiff(ctx, &b, d, o)
	case GateMerge:
		renderImplementation(&b, art, "implementation.json", "Implementation")
		d.Findings += renderReview(&b, art, preferVerified(art, "final_review.json"), "Final review")
		renderDiff(ctx, &b, d, o)
	case GateRelease:
		fmt.Fprintf(&b, "## Merged\n\n`%s` is merged into `%s`. Approving runs the release command:\n\n",
			o.Job.Branch, orDash(o.DefaultBranch))
		if len(o.ShipCommand) > 0 {
			fmt.Fprintf(&b, "```\n%s\n```\n\n", strings.Join(o.ShipCommand, " "))
		} else {
			b.WriteString("_No `ship.command` is configured for this target; the release state will fail._\n\n")
		}
		renderImplementation(&b, art, "implementation.json", "What was merged")
	case GateHold:
		renderHold(&b, o)
		renderDiff(ctx, &b, d, o)
	}

	renderActions(&b, gate, o)
	d.Body = b.String()
	return d, nil
}

func gateTitle(gate string, o Options) string {
	switch gate {
	case GateSpec:
		return "Approve the spec and plan before any code is written."
	case GateCode:
		return "Approve the implementation before it goes to build and test."
	case GateMerge:
		return fmt.Sprintf("Approve merging this change into %s.", orDash(o.DefaultBranch))
	case GateRelease:
		return "Approve the release."
	case GateHold:
		return "This job stopped and needs a decision."
	}
	return "Decision needed."
}

func renderIssue(b *strings.Builder, j *store.Job) {
	b.WriteString("## Issue\n\n")
	body := strings.TrimSpace(j.IssueBody)
	if body == "" {
		body = "_(no body)_"
	}
	b.WriteString(quote(body, 40))
	b.WriteString("\n")
}

func renderSpec(b *strings.Builder, art string) {
	s, err := artifact.LoadSpec(filepath.Join(art, "spec.json"))
	if err != nil {
		fmt.Fprintf(b, "## Spec\n\n_unavailable: %v_\n\n", err)
		return
	}
	b.WriteString("## Spec\n\n")
	fmt.Fprintf(b, "%s\n\n", s.IssueSummary)
	fmt.Fprintf(b, "**Approach.** %s\n\n", s.Approach)
	bullets(b, "Acceptance criteria", s.AcceptanceCriteria)
	bullets(b, "Files it expects to touch", s.AffectedFiles)
	bullets(b, "Error paths", s.ErrorPaths)
	bullets(b, "Out of scope", s.OutOfScope)
	bullets(b, "Compatibility concerns", s.CompatibilityConcerns)
}

func renderPlan(b *strings.Builder, art string) {
	p, err := artifact.LoadPlan(filepath.Join(art, "plan.json"))
	if err != nil {
		fmt.Fprintf(b, "## Plan\n\n_unavailable: %v_\n\n", err)
		return
	}
	fmt.Fprintf(b, "## Plan (%d steps)\n\n", len(p.Steps))
	for _, s := range p.Steps {
		fmt.Fprintf(b, "- **%s** %s\n", s.ID, s.Description)
		if len(s.Files) > 0 {
			fmt.Fprintf(b, "  - files: %s\n", strings.Join(s.Files, ", "))
		}
		if s.Verification != "" {
			fmt.Fprintf(b, "  - verify: %s\n", s.Verification)
		}
	}
	b.WriteString("\n")
	bullets(b, "Risks", p.Risks)
}

func renderImplementation(b *strings.Builder, art, name, heading string) {
	im, err := artifact.LoadImplementation(filepath.Join(art, name))
	if err != nil {
		return
	}
	fmt.Fprintf(b, "## %s\n\n%s\n\n", heading, im.Summary)
	bullets(b, "Files changed", im.FilesChanged)
	bullets(b, "Tests added or changed", im.TestsAddedOrChanged)
	var skipped []string
	for _, s := range im.SkippedSteps() {
		skipped = append(skipped, fmt.Sprintf("%s — %s", s.ID, s.Note))
	}
	bullets(b, "Plan steps NOT done", skipped)
}

// renderReview prints a review artifact's summary and every finding. Findings
// below the gate threshold are the reason this exists: the pipeline let the
// job through because nothing blocked, but "nothing blocked" is not "nothing
// was found", and the approver is the last reader either way.
func renderReview(b *strings.Builder, art, name, heading string) int {
	if name == "" {
		return 0
	}
	r, err := artifact.LoadReview(filepath.Join(art, name))
	if err != nil {
		return 0
	}
	fmt.Fprintf(b, "## %s\n\n%s\n\n", heading, r.Summary)
	fmt.Fprintf(b, "_source: `%s`_\n\n", name)
	if len(r.Findings) == 0 {
		b.WriteString("No findings.\n\n")
		return 0
	}
	for _, f := range r.Findings {
		loc := ""
		if f.File != "" {
			loc = " `" + f.File + "`"
		}
		fmt.Fprintf(b, "- **[%s]**%s %s\n", f.Severity, loc, f.Description)
		if f.Recommendation != "" {
			fmt.Fprintf(b, "  - recommendation: %s\n", f.Recommendation)
		}
		if v := f.Verification; v != nil {
			verdict := "refuted"
			if v.Survived {
				verdict = "confirmed"
			}
			fmt.Fprintf(b, "  - verifiers: %s %d/%d (was %s)\n", verdict, v.Confirmed, v.Votes, v.OriginalSeverity)
		}
	}
	b.WriteString("\n")
	return len(r.Findings)
}

func renderDiff(ctx context.Context, b *strings.Builder, d *Doc, o Options) {
	base := o.Job.Counters.BaseSHA
	if base == "" {
		return
	}
	if _, err := os.Stat(o.Job.WorktreePath); err != nil {
		fmt.Fprintf(b, "## Diff\n\n_worktree %s is gone; nothing to diff_\n\n", o.Job.WorktreePath)
		return
	}
	repo := gitx.Repo{Root: o.RepoPath}
	dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stat, err := repo.DiffStatSince(dctx, o.Job.WorktreePath, base)
	if err != nil {
		fmt.Fprintf(b, "## Diff\n\n_unavailable: %v_\n\n", err)
		return
	}
	b.WriteString("## Diff vs base\n\n")
	fmt.Fprintf(b, "```\n%s\n```\n\n", strings.TrimRight(stat, "\n"))
	patch, err := repo.DiffPatchSince(dctx, o.Job.WorktreePath, base)
	if err == nil {
		d.Diff = patch
		fmt.Fprintf(b, "Full patch: `%s`\n\n", DiffPath(o.DataDir, o.Job.ID, d.Gate))
		if o.FullDiff {
			fmt.Fprintf(b, "```diff\n%s\n```\n\n", strings.TrimRight(patch, "\n"))
		} else {
			fmt.Fprintf(b, "_Re-run with `--diff` to read it inline, or `git -C %s diff %s`._\n\n",
				o.Job.WorktreePath, short(base))
		}
	}
}

func renderHold(b *strings.Builder, o Options) {
	fmt.Fprintf(b, "## Why it stopped\n\n%s\n\n", orDash(o.Job.HoldReason))
	if len(o.Events) == 0 {
		return
	}
	b.WriteString("## Last events\n\n```\n")
	evs := o.Events
	if len(evs) > 12 {
		evs = evs[len(evs)-12:]
	}
	for _, e := range evs {
		fmt.Fprintf(b, "%s  %-18s %-12s %s\n",
			e.CreatedAt.Local().Format("15:04:05"), e.State, e.Kind, oneLine(e.Detail, 90))
	}
	b.WriteString("```\n\n")
}

// renderActions is the part that must never be missing: a document that shows
// the change but not how to accept or refuse it leaves the operator exactly
// where they started.
func renderActions(b *strings.Builder, gate string, o Options) {
	id := o.Job.ID
	b.WriteString("## What happens next\n\n")
	switch gate {
	case GateSpec:
		fmt.Fprintf(b, "- approve → implementation starts from this plan\n")
		fmt.Fprintf(b, "- reject → planning runs again with your reason as its instruction\n\n")
	case GateCode:
		fmt.Fprintf(b, "- approve → the change goes to build and test\n")
		fmt.Fprintf(b, "- reject → a fix round starts with your reason as its instruction\n\n")
	case GateMerge:
		fmt.Fprintf(b, "- approve → merged into `%s` (no fast-forward)\n", orDash(o.DefaultBranch))
		fmt.Fprintf(b, "- reject → a fix round starts with your reason; `--cancel` ends the job instead\n\n")
	case GateRelease:
		fmt.Fprintf(b, "- approve → the release command runs\n")
		fmt.Fprintf(b, "- reject → the job completes without releasing (the merge stands)\n\n")
	case GateHold:
		b.WriteString("- resume → the job re-enters the state it stopped in, or the one you name\n")
		b.WriteString("- cancel → the job ends and its worktree is cleaned up\n\n")
	}
	b.WriteString("```\n")
	if gate == GateHold {
		fmt.Fprintf(b, "sdlc resume %s [--to STATE] [--note \"do X instead\"]\n", id)
		fmt.Fprintf(b, "sdlc cancel %s\n", id)
	} else {
		fmt.Fprintf(b, "sdlc approve %s [--note \"...\"]\n", id)
		fmt.Fprintf(b, "sdlc reject  %s --reason \"...\"\n", id)
	}
	b.WriteString("```\n")
}

// --- helpers -------------------------------------------------------------

var roundRe = regexp.MustCompile(`\.r(\d+)\.`)

// latestReview finds the highest-numbered round of a review family, preferring
// the verified copy. A gate document that quoted round 1 of a review that ran
// three times would be quietly describing a superseded decision.
func latestReview(art, prefix string) string {
	entries, err := os.ReadDir(art)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, prefix+".r") && strings.HasSuffix(n, ".json") {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return ""
	}
	round := func(n string) int {
		m := roundRe.FindStringSubmatch(n)
		if len(m) != 2 {
			return 0
		}
		v, _ := strconv.Atoi(m[1])
		return v
	}
	sort.Slice(names, func(i, j int) bool {
		ri, rj := round(names[i]), round(names[j])
		if ri != rj {
			return ri < rj
		}
		// Same round: the verified copy sorts last and therefore wins.
		return len(names[i]) < len(names[j])
	})
	return names[len(names)-1]
}

func preferVerified(art, name string) string {
	v := strings.TrimSuffix(name, ".json") + ".verified.json"
	if _, err := os.Stat(filepath.Join(art, v)); err == nil {
		return v
	}
	if _, err := os.Stat(filepath.Join(art, name)); err != nil {
		return ""
	}
	return name
}

func bullets(b *strings.Builder, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "**%s.**\n\n", heading)
	for _, it := range items {
		fmt.Fprintf(b, "- %s\n", it)
	}
	b.WriteString("\n")
}

func quote(s string, maxLines int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	truncated := false
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	for i, ln := range lines {
		lines[i] = "> " + ln
	}
	out := strings.Join(lines, "\n") + "\n"
	if truncated {
		out += ">\n> _(truncated)_\n"
	}
	return out
}

func oneLine(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", "")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func humanSince(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
