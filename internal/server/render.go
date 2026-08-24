package server

// The review document as HTML (spec §7). Owner: W2-J.
//
// This file is the browser's half of the block model. internal/review owns the
// document tree and renders it to markdown for the terminal; everything below
// renders the same tree for a reader who is about to press approve.
//
// The work is split in two, and the split is deliberate. Go does the shaping —
// clamping a heading level, dropping the newline span a paragraph carries for
// markdown's benefit, turning a *artifact.Verification into the words
// "confirmed 2/3" — and templates/gate.html does the markup. None of the
// markup is built here, because building it here means handing html/template a
// string it must be told not to escape — and the one type that says so, applied
// to a finding description, is a straight line from "an agent wrote this" to
// "the reviewer's browser ran it" (spec §7.4). TestHouseRules greps this
// package for those types and expects none, this file included, which is why
// none of them is named here even in a comment. Every string below reaches the
// page as plain interpolation.
//
// The consequence is that the per-kind switch exists twice: newBlockView here
// shapes, gate.html draws. TestEveryBlockKindRenders walks review.AllBlockKinds
// through *both* — it shapes each kind and executes the real template on the
// result — so a kind that either half has forgotten fails the test rather than
// disappearing from the page. That walk is the reason the block model exists.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
	"github.com/vipinm/sdlc-orchestrator/internal/review"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

type docSource string

const (
	docLive      docSource = "live"
	docPersisted docSource = "persisted"
)

// gateRenderTimeout caps the git calls review.Render makes (spec §6.1). It is
// derived from the request context as well, so a reader who closes the tab
// stops the diff.
const gateRenderTimeout = 20 * time.Second

// --- the page ------------------------------------------------------------

// gateBody is what gate.html renders. It carries the job separately from the
// document because the two disagree in exactly the case the page has to
// survive: when the live render failed, the document is a snapshot and the job
// row is current.
type gateBody struct {
	Job   *store.Job
	Gate  string
	Title string // the document's own one-line statement of the decision

	// Waiting is false when review.GateFor said nothing is waiting on
	// anybody. Message then carries review.Render's own sentence, so asking
	// the browser about the wrong job reads exactly like asking the CLI.
	Waiting bool
	Message string

	// Source and SourcePath are A2's on-screen label. A reviewer who cannot
	// tell a live render from a snapshot cannot tell whether what they are
	// approving is what is on disk.
	Source     docSource
	SourcePath string

	Blocks  []blockView
	HasDiff bool

	// Pipeline is the strip above the document (shape.go), so a reviewer can
	// see what the gate is a gate *between* before reading a word of it.
	Pipeline []phaseView

	// Queue and Running are the rail beside the document. A gate is decided
	// one at a time, but it is decided in the knowledge of what else is
	// waiting; the rail is that knowledge, and it is why this page can be the
	// only one an operator keeps open.
	//
	// Queue is oldest-first: the job that has been waiting longest is the one
	// that has cost the most by waiting. Running is newest-first and is
	// there to answer "is anything actually happening", which is the question
	// an empty queue raises.
	Queue   []jobRow
	Running []jobRow

	// Findings is every finding in the document, flattened out of Blocks. The
	// decision panel checks them off one by one; the document still renders
	// them in place, and this is the same slice, not a second reading of the
	// artifact.
	Findings []findingView

	// Facts is the decision panel's summary — where the job is, what it is
	// merging, and whether the document above was rendered live. It repeats
	// what the document says on purpose: the panel is what stays on screen
	// while the document scrolls.
	Facts []kvRow

	// Form is W2-I's model, invoked as {{template "decisionForm" .Form}}. The
	// gate page is where the decision is made, so the form belongs on it.
	Form FormModel
}

// blockFindings flattens the document's findings blocks in document order.
// Order matters: the checklist in the panel and the findings in the document
// are read against each other, and a panel sorted differently from the page it
// sits beside is a panel that has to be searched rather than scanned.
func blockFindings(blocks []blockView) []findingView {
	var out []findingView
	for _, b := range blocks {
		out = append(out, b.Findings...)
	}
	return out
}

// gateFacts is the panel's summary. Every value is read off the job or off the
// document that was actually rendered — nothing here asks git a second
// question, because the answer would be from a different moment than the
// document above it.
func gateFacts(j *store.Job, src docSource) []kvRow {
	facts := []kvRow{
		{K: "state", V: j.State},
		{K: "from", V: j.PrevState},
		{K: "target", V: j.Target},
		{K: "branch", V: j.Branch},
		{K: "head", V: shortSHA(j.HeadSHA)},
		{K: "base", V: shortSHA(j.Counters.BaseSHA)},
		{K: "agent invocations", V: strconv.Itoa(j.Counters.AgentInvocations)},
	}
	if src == docLive {
		facts = append(facts, kvRow{K: "document", V: "live from the worktree"})
	} else {
		facts = append(facts, kvRow{K: "document", V: "saved snapshot"})
	}
	return facts
}

// rail builds the two lists beside the document. A failure to read the job
// list is not a failure of this page: the document and the decision it is
// asking for are both already in hand, and refusing to draw them because a
// sidebar query failed would withhold exactly what the reader came for.
func (s *Server) rail(current *store.Job) (queue, running []jobRow) {
	jobs, err := s.st.ListJobs()
	if err != nil {
		s.log.Printf("reading the job list for the review rail: %v", err)
		return nil, nil
	}
	for _, j := range jobs {
		row := newJobRow(j)
		switch {
		case row.Gate != "":
			queue = append(queue, row)
		case isRunning(j.State):
			running = append(running, row)
		}
	}
	sort.SliceStable(queue, func(a, b int) bool {
		return queue[a].Job.StateEnteredAt.Before(queue[b].Job.StateEnteredAt)
	})
	sort.SliceStable(running, func(a, b int) bool {
		return running[a].Job.UpdatedAt.After(running[b].Job.UpdatedAt)
	})
	return queue, running
}

// handleGate serves GET /jobs/{id}/gate.
func (s *Server) handleGate(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	// The state-to-gate mapping is review.GateFor and nothing else (spec §6);
	// an empty gate is a job nobody is waiting on, which is a 200 with an
	// explanation rather than an error (spec §5.1).
	gate := review.GateFor(j.State)
	doc, src, err := s.gateDoc(r.Context(), j, gate)
	if err != nil {
		if gate == "" {
			body := &gateBody{Job: j, Message: err.Error()}
			s.render(w, r, "gate.html", s.page(r, j.ID+" — nothing waiting", "jobs", body))
			return
		}
		// Both the live render and the snapshot failed. The reader gets the
		// live failure, because "the worktree is gone" is the actionable half
		// and "there is also no saved copy" is not.
		s.log.Printf("rendering the %s gate for %s: %v", gate, j.ID, err)
		s.fail(w, r, http.StatusInternalServerError, fmt.Sprintf(
			"the %s gate document for %s could not be rendered: %v", gate, j.ID, err))
		return
	}
	body := &gateBody{
		Job:        j,
		Gate:       doc.Gate,
		Title:      doc.Title,
		Waiting:    true,
		Source:     src,
		SourcePath: review.DocPath(s.cfg.Get().Orchestrator.DataDir, j.ID, gate),
		Blocks:     blockViews(doc.Blocks),
		HasDiff:    doc.Diff != "",
		Pipeline:   pipelineFor(j.State, j.PrevState),
		Facts:      gateFacts(j, src),
		Form:       s.formModel(r, j, ""),
	}
	body.Findings = blockFindings(body.Blocks)
	body.Queue, body.Running = s.rail(j)
	s.render(w, r, "gate.html", s.page(r, j.ID+" — "+doc.Gate+" gate", "jobs", body).wide("review"))
}

// --- A2: live, with the snapshot as the fallback -------------------------

// gateDoc renders the gate document, preferring a live render.
//
// While a job is parked its branch is not moving, so the live render and the
// copy review.Write left on disk at park time normally agree word for word.
// When they disagree it is because a human touched the worktree — and the live
// truth is what belongs beside the approve button. The persisted copy exists
// for the case where a live render cannot happen at all (the worktree was
// cleaned up, the target was renamed out of the config, the repo moved), so
// that is the only case it serves, and the page names which of the two the
// reader is looking at.
//
// The persisted file is markdown with no block tree behind it, and this
// package refuses to grow a markdown parser (spec §7.1). It comes back as a
// single BlockCode: unstyled, complete, and honestly labelled, which is the
// right trade for a fallback that only fires when the live source is gone.
func (s *Server) gateDoc(ctx context.Context, j *store.Job, gate string) (*review.Doc, docSource, error) {
	doc, liveErr := s.liveDoc(ctx, j, gate)
	if liveErr == nil {
		return doc, docLive, nil
	}
	if gate == "" {
		// There is no gate, so there is no <gate>.md to look for and nothing
		// the reader is waiting to decide. liveErr is review.Render's "nothing
		// is waiting on you", which the caller shows as prose.
		return nil, docLive, liveErr
	}
	saved, err := s.persistedDoc(j, gate)
	if err != nil {
		return nil, docLive, liveErr
	}
	return saved, docPersisted, nil
}

// liveDoc is the same call `sdlc review` makes, with the same arguments, so the
// browser and the terminal cannot describe one gate differently.
func (s *Server) liveDoc(ctx context.Context, j *store.Job, gate string) (*review.Doc, error) {
	t, err := s.cfg.Get().Target(j.Target)
	if err != nil {
		return nil, err
	}
	// Only the hold document quotes the event trail; asking for events at the
	// other gates is a query nobody reads.
	var evs []*store.Event
	if gate == review.GateHold {
		evs, _ = s.st.ListEvents(j.ID)
	}
	// Capped at 20s on top of the request's context (spec §6.1): review.Render
	// shells out to git twice under a two-minute timeout of its own, and a
	// browser must not be able to hold a git process for two minutes per tab.
	rctx, cancel := context.WithTimeout(ctx, gateRenderTimeout)
	defer cancel()
	return review.Render(rctx, review.Options{
		Job:           j,
		Gate:          gate,
		DataDir:       s.cfg.Get().Orchestrator.DataDir,
		RepoPath:      t.RepoPath,
		DefaultBranch: t.DefaultBranch,
		ShipCommand:   t.Ship.Command,
		// FullDiff stays off: the patch has its own page, and inlining
		// thousands of lines under the findings is its own kind of
		// invisibility.
		Events: evs,
	})
}

// diffTruncationSentinel is the first line of the trailer review.Write appends
// to a patch the 4 MiB capture limit cut short.
const diffTruncationSentinel = "[sdlc] This patch stops here:"

// persistedDoc reads the copy review.Write left beside the job's artifacts.
//
// DiffTruncated is read from the trailer review.Write wrote into the file, not
// inferred from len(patch): amendment A1 voided the length heuristic, and a
// banner that appears because a patch happens to be large is a banner nobody
// believes the third time.
func (s *Server) persistedDoc(j *store.Job, gate string) (*review.Doc, error) {
	dataDir := s.cfg.Get().Orchestrator.DataDir
	body, err := os.ReadFile(review.DocPath(dataDir, j.ID, gate))
	if err != nil {
		return nil, err
	}
	doc := &review.Doc{
		Gate: gate,
		Body: string(body),
		// One code block, deliberately. Re-deriving a tree from markdown means
		// parsing markdown, and a parser here would be a second opinion about
		// what the document says.
		Blocks: []review.Block{{Kind: review.BlockCode, Text: strings.TrimRight(string(body), "\n")}},
	}
	if patch, err := os.ReadFile(review.DiffPath(dataDir, j.ID, gate)); err == nil {
		doc.Diff = string(patch)
		doc.DiffTruncated = strings.Contains(string(patch), diffTruncationSentinel)
	}
	return doc, nil
}

// --- blocks --------------------------------------------------------------

// blockView is one block shaped for gate.html. It is one struct with a Kind
// rather than an interface for the same reason review.Block is: a template
// cannot type-switch, so it dispatches on .Kind and reads the fields that kind
// uses.
type blockView struct {
	Kind review.BlockKind

	// Handled is false for a kind newBlockView has no case for. The template
	// draws a visible notice for those. A block that silently disappeared from
	// a merge document is the failure the block model exists to prevent: a
	// document short one section looks exactly like a document that had
	// nothing to say.
	Handled bool

	Level     int    // BlockHeading, clamped to 1..6
	Text      string // BlockHeading text; BlockCode body
	CodeClass string // BlockCode: the class gate.html puts on the <pre>
	Truncated bool   // BlockQuote

	Spans    []spanView
	Items    []itemView
	Facts    []review.Fact
	Quote    []quotePara
	Findings []findingView
	Steps    []artifact.PlanStep
	Actions  []actionView
	Commands []string
}

type itemView struct {
	Spans []spanView
	Sub   [][]spanView
}

// quotePara is one blank-line-separated paragraph of a quoted issue body, kept
// as its lines. HTML collapses newlines, so the lines are rejoined with <br>
// in the template and the paragraphs become separate <p>: the quote keeps the
// shape the issue was written in without anything here parsing it.
type quotePara struct {
	Lines []string
}

type findingView struct {
	Severity string
	// SevClass is a class name from a closed set, never the severity string
	// itself. Severity arrives from an agent's JSON; it is escaped wherever it
	// is printed, but a class attribute built from it would still let that
	// agent choose which of this page's styles it wears.
	SevClass       string
	File           string
	Description    string
	Recommendation string

	Verified         bool
	Verdict          string // "confirmed" | "refuted"
	Confirmed        int
	Votes            int
	OriginalSeverity string
}

type actionView struct {
	Decision string
	Effect   []spanView
}

// spanView is one inline run. Kind survives into the view rather than being
// resolved to a tag here, because resolving it to a tag means producing markup
// in Go.
type spanView struct {
	Kind    review.SpanKind
	Text    string
	Handled bool
	// Children is set for a span that wraps others — emphasis around inline
	// code, which is how the asides that quote a command are built. When it is
	// set, Text is empty and the template recurses instead of printing it.
	Children []spanView
}

// blockViews shapes a whole document. An unhandled kind becomes a view that
// says so rather than being dropped, mirroring renderMarkdown.
func blockViews(blocks []review.Block) []blockView {
	out := make([]blockView, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, newBlockView(b))
	}
	return out
}

// newBlockView is the browser's markdownBlock: one case per review.BlockKind,
// and a Handled flag that is the only way a missing case is detectable at all.
// A missing case and a block with nothing to print both draw nothing, so
// comparing rendered HTML would pass either way.
func newBlockView(b review.Block) blockView {
	v := blockView{Kind: b.Kind, Handled: true}
	switch b.Kind {
	case review.BlockHeading:
		// Clamped, not trusted, exactly as markdownBlock clamps it. Level 1 is
		// the document title every gate opens with; anything outside 1..6 is
		// not a heading in any renderer and draws as the nearest one that is.
		v.Level, v.Text = headingLevel(b.Level), b.Text

	case review.BlockParagraph:
		v.Spans = paragraphSpans(b.Spans)

	case review.BlockFacts:
		v.Facts = b.Facts

	case review.BlockBullets:
		v.Items = make([]itemView, 0, len(b.Items))
		for _, it := range b.Items {
			iv := itemView{Spans: spanViews(it.Spans)}
			for _, sub := range it.Sub {
				iv.Sub = append(iv.Sub, spanViews(sub))
			}
			v.Items = append(v.Items, iv)
		}

	case review.BlockQuote:
		v.Quote, v.Truncated = quoteParas(b.Text), b.Truncated

	case review.BlockCode:
		// Text is carried through untouched. The hold table's %-12s padding
		// leaves real trailing spaces on its lines and the golden test pins
		// them; trimming here would be this renderer changing the document's
		// words. <pre> preserves them.
		v.Text, v.CodeClass = b.Text, codeClass(b.Lang)

	case review.BlockFindings:
		v.Findings = make([]findingView, 0, len(b.Findings))
		for _, f := range b.Findings {
			v.Findings = append(v.Findings, newFindingView(f))
		}

	case review.BlockSteps:
		v.Steps = b.Steps

	case review.BlockActions:
		v.Actions = make([]actionView, 0, len(b.Actions))
		for _, a := range b.Actions {
			v.Actions = append(v.Actions, actionView{Decision: a.Decision, Effect: spanViews(a.Effect)})
		}
		v.Commands = b.Commands

	default:
		v.Handled = false
	}
	return v
}

func newFindingView(f artifact.Finding) findingView {
	v := findingView{
		Severity:       f.Severity,
		SevClass:       sevClass(f.Severity),
		File:           f.File,
		Description:    f.Description,
		Recommendation: f.Recommendation,
	}
	if ver := f.Verification; ver != nil {
		v.Verified = true
		v.Verdict = "refuted"
		if ver.Survived {
			v.Verdict = "confirmed"
		}
		v.Confirmed, v.Votes, v.OriginalSeverity = ver.Confirmed, ver.Votes, ver.OriginalSeverity
	}
	return v
}

// paragraphSpans shapes a paragraph's spans and drops the trailing newline
// most of them carry.
//
// review.para appends a SpanText of "\n" to every paragraph it builds, because
// the markdown output is pinned to the byte and the blank line after a
// paragraph has to live somewhere. In HTML a <p> is the paragraph break, so
// that span has nothing to contribute — and it is dropped rather than printed
// because a lone newline inside a <p> that ends in <code> renders as a stray
// space after the closing tag.
func paragraphSpans(spans []review.Span) []spanView {
	if n := len(spans); n > 0 {
		last := spans[n-1]
		if last.Kind == review.SpanText && strings.TrimSpace(last.Text) == "" {
			spans = spans[:n-1]
		}
	}
	return spanViews(spans)
}

func spanViews(spans []review.Span) []spanView {
	out := make([]spanView, 0, len(spans))
	for _, s := range spans {
		out = append(out, newSpanView(s))
	}
	return out
}

// newSpanView is the browser's markdownSpan. Handled travels with the span for
// the same reason it travels with a block: an emphasis that lost its case would
// otherwise render as plain text, which no reader can see and no test can
// catch. The text survives either way; only the emphasis is replaced by a
// statement that the emphasis was lost.
func newSpanView(s review.Span) spanView {
	v := spanView{Kind: s.Kind, Text: s.Text, Handled: true}
	switch s.Kind {
	case review.SpanText, review.SpanStrong, review.SpanEmphasis, review.SpanCode, review.SpanPath:
	default:
		v.Handled = false
	}
	// A wrapping span carries no text of its own. Shaping the children here
	// rather than in the template keeps the Handled flag meaningful all the way
	// down: an unrenderable child says so where it sits, instead of the whole
	// aside vanishing.
	for _, c := range s.Children {
		v.Children = append(v.Children, newSpanView(c))
	}
	return v
}

// quoteParas splits a quoted body into paragraphs at blank lines. markdownBlock
// prefixes every line including the blank ones, for the same reason: two
// paragraphs of an issue body must not join into one.
func quoteParas(text string) []quotePara {
	var out []quotePara
	cur := quotePara{}
	for _, ln := range strings.Split(text, "\n") {
		if strings.TrimSpace(ln) == "" {
			if len(cur.Lines) > 0 {
				out = append(out, cur)
				cur = quotePara{}
			}
			continue
		}
		cur.Lines = append(cur.Lines, ln)
	}
	if len(cur.Lines) > 0 {
		out = append(out, cur)
	}
	return out
}

// headingLevel clamps to the levels HTML has, which are the levels markdown
// has, and for the same reason: this is the function whose job is to keep a
// malformed document readable, so it may not be the thing that fails on one.
func headingLevel(n int) int {
	if n < 1 {
		return 1
	}
	if n > 6 {
		return 6
	}
	return n
}

// codeClass maps a block's language to a class from a closed set. review emits
// "" or "diff" today; anything else styles as plain code rather than as a
// class name chosen by whatever produced the document.
func codeClass(lang string) string {
	if lang == "diff" {
		return "code diff"
	}
	return "code"
}

// sevClass is the badge class for a severity, and app.css defines one for each
// of artifact.SeverityRank's keys. An unknown severity gets the neutral badge:
// it is still printed as text beside it, so the reader sees the word the agent
// actually wrote.
func sevClass(sev string) string {
	if _, ok := artifact.SeverityRank[sev]; ok {
		return "badge " + sev
	}
	return "badge"
}
