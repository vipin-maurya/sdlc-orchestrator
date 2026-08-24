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
	GateScope   = "scope"
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
	// Blocks is the document as a tree. Body is rendered from it rather than
	// built alongside it, so the browser and the terminal cannot come to
	// describe the same gate differently.
	Blocks []Block
	// Diff is the full patch when one was produced ("" otherwise). It is kept
	// out of Body so the caller can write it to its own file.
	Diff string
	// DiffTruncated reports that Diff is the head of a larger patch. Both
	// surfaces state it as a fact rather than guessing from len(Diff).
	DiffTruncated bool
	// Findings is how many findings the document printed, across every review
	// section it rendered — renderReview returns len(r.Findings) and Render
	// sums them. It is not a count of blocking findings and never was: the
	// pipeline has already let the job through by the time a gate document is
	// rendered, so a "blocking" count here would be 0 by construction and say
	// nothing. A non-zero value means the operator is being shown findings
	// that did not block, which is the whole reason renderReview prints them.
	Findings int
}

// GateFor maps a job state to the gate a human must decide, "" when the job
// is not waiting on anybody.
func GateFor(state string) string {
	switch state {
	case "AWAITING_SCOPE_APPROVAL":
		return GateScope
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
// returning the rendered Doc and the path it was written to. This is what the
// engine calls when a job parks, so the artifact exists before anybody asks
// for it — including for a job that parked overnight on a machine whose
// worktree has since been cleaned up.
//
// The Doc comes back rather than just the path because the caller needs the
// same words the document uses — the console notice and the file have to agree
// about what is being decided, and recovering the title by re-reading the
// markdown would make that agreement depend on the heading format.
func Write(ctx context.Context, o Options) (*Doc, string, error) {
	doc, err := Render(ctx, o)
	if err != nil {
		return nil, "", err
	}
	dir := Dir(o.DataDir, o.Job.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", err
	}
	// The patch goes down first so the document can name it: a path is only
	// worth printing once the file behind it is there to open.
	if doc.Diff != "" {
		diffPath := DiffPath(o.DataDir, o.Job.ID, doc.Gate)
		payload := doc.Diff
		if doc.DiffTruncated {
			// A patch clipped mid-hunk at the capture cap looks exactly like a
			// complete one to whatever opens the file next — git apply, a
			// reviewer scrolling to the end, a tool counting hunks. The file
			// says what happened to it in its own bytes rather than relying on
			// the reader having also opened the document beside it.
			if !strings.HasSuffix(payload, "\n") {
				payload += "\n"
			}
			payload += diffTruncationTrailer(o.Job.WorktreePath, o.Job.Counters.BaseSHA)
		}
		if err := os.WriteFile(diffPath, []byte(payload), 0o644); err != nil {
			return nil, "", err
		}
		spans := []Span{
			{Kind: SpanText, Text: "\nFull patch: "},
			{Kind: SpanPath, Text: diffPath},
		}
		if doc.DiffTruncated {
			// Doc.DiffTruncated had no reader at all until here. A line that
			// names a file without saying it is short is the document telling
			// the operator they have the whole change when they do not.
			spans = append(spans, Span{Kind: SpanText, Text: " (truncated at the 4 MiB capture limit)"})
		}
		// A block appended to both, not a string built beside them: Body is a
		// function of Blocks everywhere else, and a line appended to one but
		// not the other is the browser and the terminal disagreeing about
		// whether the patch is on disk.
		//
		// Only the new block is rendered, not the whole document again.
		// renderMarkdown is a concatenation of per-block strings, so appending
		// this one block's markdown is the same bytes as re-rendering all of
		// them — TestWriteKeepsBodyAFunctionOfBlocks is what holds that. The
		// re-render copied the entire document, inlined patch included, to add
		// one line.
		blk := Block{Kind: BlockParagraph, Spans: spans}
		doc.Blocks = append(doc.Blocks, blk)
		doc.Body += renderMarkdown([]Block{blk})
	}
	docPath := DocPath(o.DataDir, o.Job.ID, doc.Gate)
	if err := os.WriteFile(docPath, []byte(doc.Body), 0o644); err != nil {
		return nil, "", err
	}
	return doc, docPath, nil
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
	art := artifact.ArtifactsDir(o.DataDir, o.Job.ID)

	d.Title = gateTitle(gate, o)
	d.Blocks = append(d.Blocks,
		Block{Kind: BlockHeading, Level: 1, Text: fmt.Sprintf("%s — %s", o.Job.ID, o.Job.IssueTitle)},
		para(Span{Kind: SpanStrong, Text: d.Title}),
		Block{Kind: BlockFacts, Facts: []Fact{
			{Label: "state", Value: o.Job.State, Note: "waiting " + humanSince(o.Job.StateEnteredAt)},
			{Label: "branch", Value: o.Job.Branch},
			{Label: "worktree", Value: o.Job.WorktreePath},
			{Label: "artifacts", Value: art},
		}},
	)

	switch gate {
	case GateScope:
		renderIssue(&d.Blocks, o.Job)
		renderProblem(&d.Blocks, art)
	case GateSpec:
		renderIssue(&d.Blocks, o.Job)
		renderSpec(&d.Blocks, art)
		renderPlan(&d.Blocks, art)
		d.Findings += renderReview(&d.Blocks, art, latestReview(art, "design_review"), "Design review")
	case GateCode:
		renderImplementation(&d.Blocks, art, "implementation.json", "Implementation")
		d.Findings += renderReview(&d.Blocks, art, latestReview(art, "code_review"), "Code review")
		renderDiff(ctx, &d.Blocks, d, o)
	case GateMerge:
		renderImplementation(&d.Blocks, art, "implementation.json", "Implementation")
		d.Findings += renderReview(&d.Blocks, art, preferVerified(art, "final_review.json"), "Final review")
		renderDiff(ctx, &d.Blocks, d, o)
	case GateRelease:
		d.Blocks = append(d.Blocks,
			section("Merged"),
			para(
				Span{Kind: SpanCode, Text: o.Job.Branch},
				Span{Kind: SpanText, Text: " is merged into "},
				Span{Kind: SpanCode, Text: orDash(o.DefaultBranch)},
				Span{Kind: SpanText, Text: ". Approving runs the release command:"},
			),
		)
		if len(o.ShipCommand) > 0 {
			d.Blocks = append(d.Blocks, Block{Kind: BlockCode, Text: strings.Join(o.ShipCommand, " ")})
		} else {
			d.Blocks = append(d.Blocks, asideOf(txt("No "), code("ship.command"), txt(" is configured for this target; the release state will fail.")))
		}
		renderImplementation(&d.Blocks, art, "implementation.json", "What was merged")
	case GateHold:
		renderHold(&d.Blocks, o)
		renderDiff(ctx, &d.Blocks, d, o)
	}

	renderActions(&d.Blocks, gate, o)
	d.Body = renderMarkdown(d.Blocks)
	return d, nil
}

func gateTitle(gate string, o Options) string {
	switch gate {
	case GateScope:
		return "Approve the scoped problem before a spec is written."
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

func renderIssue(bs *[]Block, j *store.Job) {
	body := strings.TrimSpace(j.IssueBody)
	if body == "" {
		body = "_(no body)_"
	}
	text, truncated := clipLines(body, 40)
	*bs = append(*bs,
		section("Issue"),
		Block{Kind: BlockQuote, Text: text, Truncated: truncated},
	)
}

// renderProblem prints the scoped problem and says why the job is parked. The
// two causes — the agent raised a blocking question, or human_gates asks for a
// checkpoint — need different responses from the operator, so the document
// distinguishes them rather than leaving them to be inferred from the presence
// of a questions section.
func renderProblem(bs *[]Block, art string) {
	p, err := artifact.LoadProblem(filepath.Join(art, "problem.json"))
	if err != nil {
		*bs = append(*bs, section("Scoped problem"),
			aside("problem.json is missing or unreadable: "+err.Error()))
		return
	}
	*bs = append(*bs, section("Scoped problem"), para(txt(p.ProblemStatement)))
	bullets(bs, "In scope", p.InScope)
	bullets(bs, "Out of scope", p.OutOfScope)
	bullets(bs, "Success criteria", p.SuccessCriteria)

	if len(p.Assumptions) > 0 {
		items := make([]Item, 0, len(p.Assumptions))
		for _, a := range p.Assumptions {
			items = append(items, Item{
				Spans: []Span{txt(a.Assumption)},
				Sub:   [][]Span{{Span{Kind: SpanEmphasis, Text: "basis: " + a.Basis}}},
			})
		}
		*bs = append(*bs, section("Assumptions"), Block{Kind: BlockBullets, Items: items})
	}

	blocking := p.BlockingQuestions()
	if len(p.OpenQuestions) > 0 {
		// Blocking questions first: they are the ones an answer is needed for.
		ordered := append(append([]artifact.OpenQuestion{}, blocking...), nonBlocking(p)...)
		items := make([]Item, 0, len(ordered))
		for _, q := range ordered {
			label := q.ID + ": " + q.Question
			if !q.Blocking {
				label += " (not blocking)"
			}
			it := Item{Spans: []Span{txt(label)}}
			if q.WhyItMatters != "" {
				it.Sub = [][]Span{{Span{Kind: SpanEmphasis, Text: "why it matters: " + q.WhyItMatters}}}
			}
			items = append(items, it)
		}
		*bs = append(*bs, section("Open questions"), Block{Kind: BlockBullets, Items: items})
	}

	if len(blocking) > 0 {
		// aside, not asideOf: this remark quotes no command or path, and
		// asideOf exists for the ones that do.
		*bs = append(*bs, aside(fmt.Sprintf(
			"The scoping agent is blocked on %d question(s) and will not write a spec until they are settled. "+
				"Rejecting with your answers re-scopes; approving waives them and plans on the assumptions above.",
			len(blocking))))
		return
	}
	*bs = append(*bs, asideOf(
		txt("The scoping agent reported clarity "), code(p.Clarity),
		txt(" and raised nothing blocking. This job is parked because "), code("policies.human_gates"),
		txt(" lists "), code("scope"), txt("."),
	))
}

// nonBlocking is the complement of Problem.BlockingQuestions.
func nonBlocking(p *artifact.Problem) []artifact.OpenQuestion {
	var out []artifact.OpenQuestion
	for _, q := range p.OpenQuestions {
		if !q.Blocking {
			out = append(out, q)
		}
	}
	return out
}

func renderSpec(bs *[]Block, art string) {
	s, err := artifact.LoadSpec(filepath.Join(art, "spec.json"))
	if err != nil {
		*bs = append(*bs, section("Spec"), aside(fmt.Sprintf("unavailable: %v", err)))
		return
	}
	*bs = append(*bs,
		section("Spec"),
		para(Span{Kind: SpanText, Text: s.IssueSummary}),
		para(
			Span{Kind: SpanStrong, Text: "Approach."},
			Span{Kind: SpanText, Text: " " + s.Approach},
		),
	)
	bullets(bs, "Acceptance criteria", s.AcceptanceCriteria)
	bullets(bs, "Files it expects to touch", s.AffectedFiles)
	bullets(bs, "Error paths", s.ErrorPaths)
	bullets(bs, "Out of scope", s.OutOfScope)
	bullets(bs, "Compatibility concerns", s.CompatibilityConcerns)
}

func renderPlan(bs *[]Block, art string) {
	p, err := artifact.LoadPlan(filepath.Join(art, "plan.json"))
	if err != nil {
		*bs = append(*bs, section("Plan"), aside(fmt.Sprintf("unavailable: %v", err)))
		return
	}
	*bs = append(*bs,
		section(fmt.Sprintf("Plan (%d steps)", len(p.Steps))),
		// The steps travel as artifact.PlanStep rather than as formatted
		// lines: the browser wants to lay out the files and the verification
		// command differently from the terminal, and it can only do that from
		// the fields.
		Block{Kind: BlockSteps, Steps: p.Steps},
	)
	bullets(bs, "Risks", p.Risks)
}

func renderImplementation(bs *[]Block, art, name, heading string) {
	im, err := artifact.LoadImplementation(filepath.Join(art, name))
	if err != nil {
		return
	}
	*bs = append(*bs, section(heading), para(Span{Kind: SpanText, Text: im.Summary}))
	bullets(bs, "Files changed", im.FilesChanged)
	bullets(bs, "Tests added or changed", im.TestsAddedOrChanged)
	var skipped []string
	for _, s := range im.SkippedSteps() {
		skipped = append(skipped, fmt.Sprintf("%s — %s", s.ID, s.Note))
	}
	bullets(bs, "Plan steps NOT done", skipped)
}

// renderReview prints a review artifact's summary and every finding. Findings
// below the gate threshold are the reason this exists: the pipeline let the
// job through because nothing blocked, but "nothing blocked" is not "nothing
// was found", and the approver is the last reader either way.
func renderReview(bs *[]Block, art, name, heading string) int {
	if name == "" {
		return 0
	}
	r, err := artifact.LoadReview(filepath.Join(art, name))
	if err != nil {
		return 0
	}
	*bs = append(*bs,
		section(heading),
		para(Span{Kind: SpanText, Text: r.Summary}),
		asideOf(txt("source: "), code(name)),
	)
	if len(r.Findings) == 0 {
		*bs = append(*bs, para(Span{Kind: SpanText, Text: "No findings."}))
		return 0
	}
	// The findings go across whole, not pre-formatted. The severity badge and
	// the verification verdict are the two things each surface draws in its
	// own idiom, and formatting them into a string here is what would force
	// the browser to un-format them again.
	*bs = append(*bs, Block{Kind: BlockFindings, Findings: r.Findings})
	return len(r.Findings)
}

// diffSource is the part of gitx.Repo renderDiff uses. It exists so the
// section's branches — the stat that failed, the patch that was clipped, the
// inline fence — can be rendered byte for byte without a repository on disk.
// The golden fixture is deliberately git-free, and a document whose wording is
// only ever exercised on a machine that happens to have git installed is a
// document nothing pins.
type diffSource interface {
	DiffStatSince(ctx context.Context, dir, sha string) (string, error)
	DiffPatchSince(ctx context.Context, dir, sha string) (patch string, truncated bool, err error)
}

// openDiffSource is what renderDiff reads its diff through. Production never
// reassigns it; tests in this package do, and never in parallel.
var openDiffSource = func(repoPath string) diffSource { return gitx.Repo{Root: repoPath} }

func renderDiff(ctx context.Context, bs *[]Block, d *Doc, o Options) {
	base := o.Job.Counters.BaseSHA
	if base == "" {
		return
	}
	if _, err := os.Stat(o.Job.WorktreePath); err != nil {
		*bs = append(*bs, section("Diff"),
			aside(fmt.Sprintf("worktree %s is gone; nothing to diff", o.Job.WorktreePath)))
		return
	}
	repo := openDiffSource(o.RepoPath)
	dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stat, err := repo.DiffStatSince(dctx, o.Job.WorktreePath, base)
	if err != nil {
		*bs = append(*bs, section("Diff"), aside(fmt.Sprintf("unavailable: %v", err)))
		return
	}
	*bs = append(*bs,
		section("Diff vs base"),
		Block{Kind: BlockCode, Text: strings.TrimRight(stat, "\n")},
	)
	patch, truncated, err := repo.DiffPatchSince(dctx, o.Job.WorktreePath, base)
	if err == nil {
		d.Diff, d.DiffTruncated = patch, truncated
		if truncated {
			// "what follows is its head" is what this used to say, from here —
			// above the branch that decides whether anything follows at all.
			// With --diff off, nothing does, and the document promised a patch
			// it never printed. What is true either way is that the capture
			// stopped short, which is also what the reader needs in order to
			// distrust the .diff file Write puts on disk.
			*bs = append(*bs, asideOf(
				txt("The patch exceeds the 4 MiB capture limit, so only its head was captured. "),
				code(fmt.Sprintf("git -C %s diff %s", o.Job.WorktreePath, Short(base))),
				txt(" for all of it.")))
		}
		if o.FullDiff {
			*bs = append(*bs, Block{Kind: BlockCode, Lang: "diff", Text: strings.TrimRight(patch, "\n")})
		} else {
			// No path to the patch file here: Render does not write it, and only
			// Write knows it is on disk. Naming it from here would point a reader
			// at a file that exists only when the engine happened to park this
			// job — the git command works either way.
			*bs = append(*bs, asideOf(
				txt("Re-run with "), code("--diff"), txt(" to read it inline, or "),
				code(fmt.Sprintf("git -C %s diff %s", o.Job.WorktreePath, Short(base))), txt(".")))
		}
	}
}

func renderHold(bs *[]Block, o Options) {
	*bs = append(*bs, section("Why it stopped"), para(Span{Kind: SpanText, Text: orDash(o.Job.HoldReason)}))
	if len(o.Events) == 0 {
		return
	}
	evs := o.Events
	if len(evs) > 12 {
		evs = evs[len(evs)-12:]
	}
	lines := make([]string, 0, len(evs))
	for _, e := range evs {
		// The padding is what makes the columns line up, so a short field
		// leaves trailing spaces on the line. They are part of the document
		// and nothing downstream may trim them.
		lines = append(lines, fmt.Sprintf("%s  %-18s %-12s %s",
			e.CreatedAt.Local().Format("15:04:05"), e.State, e.Kind, oneLine(e.Detail, 90)))
	}
	*bs = append(*bs, section("Last events"), Block{Kind: BlockCode, Text: strings.Join(lines, "\n")})
}

// renderActions is the part that must never be missing: a document that shows
// the change but not how to accept or refuse it leaves the operator exactly
// where they started.
func renderActions(bs *[]Block, gate string, o Options) {
	id := o.Job.ID
	blk := Block{Kind: BlockActions}
	text := func(s string) []Span { return []Span{{Kind: SpanText, Text: s}} }
	switch gate {
	case GateScope:
		blk.Actions = []Action{
			{Decision: "approve", Effect: text("planning starts from this problem statement; any open questions are waived")},
			{Decision: "reject", Effect: text("scoping runs again with your answers as its instruction")},
		}
	case GateSpec:
		blk.Actions = []Action{
			{Decision: "approve", Effect: text("implementation starts from this plan")},
			{Decision: "reject", Effect: text("planning runs again with your reason as its instruction")},
		}
	case GateCode:
		blk.Actions = []Action{
			{Decision: "approve", Effect: text("the change goes to build and test")},
			{Decision: "reject", Effect: text("a fix round starts with your reason as its instruction")},
		}
	case GateMerge:
		blk.Actions = []Action{
			{Decision: "approve", Effect: []Span{
				{Kind: SpanText, Text: "merged into "},
				{Kind: SpanCode, Text: orDash(o.DefaultBranch)},
				{Kind: SpanText, Text: " (no fast-forward)"},
			}},
			{Decision: "reject", Effect: []Span{
				{Kind: SpanText, Text: "a fix round starts with your reason; "},
				{Kind: SpanCode, Text: "--cancel"},
				{Kind: SpanText, Text: " ends the job instead"},
			}},
		}
	case GateRelease:
		blk.Actions = []Action{
			{Decision: "approve", Effect: text("the release command runs")},
			{Decision: "reject", Effect: text("the job completes without releasing (the merge stands)")},
		}
	case GateHold:
		blk.Actions = []Action{
			{Decision: "resume", Effect: text("the job re-enters the state it stopped in, or the one you name")},
			{Decision: "cancel", Effect: text("the job ends and its worktree is cleaned up")},
		}
	}
	if gate == GateHold {
		blk.Commands = []string{
			fmt.Sprintf("sdlc resume %s [--to STATE] [--note \"do X instead\"]", id),
			fmt.Sprintf("sdlc cancel %s", id),
		}
	} else {
		blk.Commands = []string{
			fmt.Sprintf("sdlc approve %s [--note \"...\"]", id),
			fmt.Sprintf("sdlc reject  %s --reason \"...\"", id),
		}
	}
	*bs = append(*bs, section("What happens next"), blk)
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

// section is a level-2 heading, which is every heading the document has below
// its title.
func section(text string) Block {
	return Block{Kind: BlockHeading, Level: 2, Text: text}
}

// aside is one of the document's italic remarks — "unavailable: ...", "the
// worktree is gone", "re-run with --diff". They are emphasis rather than plain
// text because each one is the renderer speaking about the document rather
// than the document's own content.
//
// The text may carry backticks of its own, as several of these do. Spans do
// not nest, so an aside that quotes a command is one emphasis span containing
// the backticks rather than an emphasis wrapped around a code span; markdown
// reads it correctly either way, and a renderer that cannot nest is better
// than a document that changes.
func aside(text string) Block {
	return para(Span{Kind: SpanEmphasis, Text: text})
}

// asideOf is aside for the ones that quote a command or a filename. The parts
// are given structurally rather than as a string with backticks in it: the
// markdown is identical either way, but the browser needs to know which parts
// are code, and a renderer that recovered that by scanning for backticks would
// be re-parsing one surface's delimiters out of the other's text — the implicit
// contract the block model exists to remove.
func asideOf(parts ...Span) Block {
	return para(Span{Kind: SpanEmphasis, Children: parts})
}

// txt and code are shorthand for asideOf's parts.
func txt(s string) Span  { return Span{Kind: SpanText, Text: s} }
func code(s string) Span { return Span{Kind: SpanCode, Text: s} }

func bullets(bs *[]Block, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	list := Block{Kind: BlockBullets, Items: make([]Item, 0, len(items))}
	for _, it := range items {
		list.Items = append(list.Items, Item{Spans: []Span{{Kind: SpanText, Text: it}}})
	}
	*bs = append(*bs,
		para(Span{Kind: SpanStrong, Text: heading + "."}),
		list,
	)
}

// clipLines cuts s to maxLines and reports whether anything was dropped. The
// caller keeps the flag on the block rather than appending a marker to the
// text, so the truncation is a fact both surfaces can present in their own
// way instead of a sentence one of them has to parse back out.
func clipLines(s string, maxLines int) (string, bool) {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= maxLines {
		return strings.Join(lines, "\n"), false
	}
	return strings.Join(lines[:maxLines], "\n"), true
}

func oneLine(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", "")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// diffTruncationTrailer is appended to the .diff artifact when the capture cap
// cut the patch short, so the file is self-describing. It names the command
// that produces the whole change, because the worktree and the base sha are
// the two things a reader of a stray .diff file does not have.
func diffTruncationTrailer(worktree, base string) string {
	return fmt.Sprintf("\n[sdlc] This patch stops here: it hit the 4 MiB capture limit and is only the head\n"+
		"[sdlc] of a larger change. Run `git -C %s diff %s` for all of it.\n",
		worktree, Short(base))
}

// Short abbreviates a SHA to the 12 characters these documents quote. It is
// exported because internal/server prints the same SHAs beside the same
// documents, and the two must not choose different widths.
func Short(sha string) string {
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
