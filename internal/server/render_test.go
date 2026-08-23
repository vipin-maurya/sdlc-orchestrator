package server

// Tests for the HTML renderer. Owner: W2-J.
//
// TestEveryBlockKindRenders and TestEverySpanKindRenders are the browser's half
// of the anti-drift mechanism internal/review describes in blocks.go: that
// package's TestEveryBlockKindHasAMarkdownCase holds the terminal to
// review.AllBlockKinds, and these hold the page to the same slice. They run
// each kind through both halves of this surface — newBlockView, which shapes,
// and templates/gate.html, which draws — because either half can lose a kind
// and only the pair rendering something visible proves neither did.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
	"github.com/vipinm/sdlc-orchestrator/internal/review"
)

// --- exhaustiveness -------------------------------------------------------

// blockFixture is one kind's sample block and what its HTML must contain. The
// wants are what make the walk more than a smoke test: a branch that matches
// the kind and prints nothing renders no error and no content, which is the
// silent-drop this whole model exists to rule out.
type blockFixture struct {
	block review.Block
	want  []string
}

// blockFixtures is keyed by kind rather than being a slice, so a kind added to
// review.AllBlockKinds with no entry here fails the walk instead of being
// skipped by it.
var blockFixtures = map[review.BlockKind]blockFixture{
	review.BlockHeading: {
		block: review.Block{Kind: review.BlockHeading, Level: 1, Text: "SENTINEL-heading"},
		want:  []string{"<h1>", "SENTINEL-heading"},
	},
	review.BlockParagraph: {
		block: review.Block{Kind: review.BlockParagraph, Spans: []review.Span{
			{Kind: review.SpanText, Text: "SENTINEL-paragraph"},
			{Kind: review.SpanText, Text: "\n"},
		}},
		want: []string{"<p>", "SENTINEL-paragraph"},
	},
	review.BlockFacts: {
		block: review.Block{Kind: review.BlockFacts, Facts: []review.Fact{
			{Label: "SENTINEL-label", Value: "SENTINEL-value", Note: "SENTINEL-note"},
		}},
		want: []string{"SENTINEL-label", "SENTINEL-value", "SENTINEL-note"},
	},
	review.BlockBullets: {
		block: review.Block{Kind: review.BlockBullets, Items: []review.Item{{
			Spans: []review.Span{{Kind: review.SpanText, Text: "SENTINEL-bullet"}},
			Sub:   [][]review.Span{{{Kind: review.SpanText, Text: "SENTINEL-sub"}}},
		}}},
		want: []string{"<li>", "SENTINEL-bullet", "SENTINEL-sub"},
	},
	review.BlockQuote: {
		block: review.Block{Kind: review.BlockQuote, Text: "SENTINEL-quote", Truncated: true},
		want:  []string{"<blockquote>", "SENTINEL-quote", "(truncated)"},
	},
	review.BlockCode: {
		block: review.Block{Kind: review.BlockCode, Lang: "diff", Text: "SENTINEL-code"},
		want:  []string{"<pre", "SENTINEL-code"},
	},
	review.BlockFindings: {
		block: review.Block{Kind: review.BlockFindings, Findings: []artifact.Finding{{
			Severity: "major", File: "SENTINEL-file", Description: "SENTINEL-finding",
			Recommendation: "SENTINEL-recommendation",
			Verification: &artifact.Verification{
				Votes: 3, Confirmed: 2, Survived: true, OriginalSeverity: "blocker",
			},
		}}},
		want: []string{"SENTINEL-file", "SENTINEL-finding", "SENTINEL-recommendation", "confirmed 2/3", "was blocker"},
	},
	review.BlockSteps: {
		block: review.Block{Kind: review.BlockSteps, Steps: []artifact.PlanStep{{
			ID: "SENTINEL-id", Description: "SENTINEL-step",
			Files: []string{"SENTINEL-file"}, Verification: "SENTINEL-verify",
		}}},
		want: []string{"SENTINEL-id", "SENTINEL-step", "SENTINEL-file", "SENTINEL-verify"},
	},
	review.BlockActions: {
		block: review.Block{Kind: review.BlockActions,
			Actions: []review.Action{{Decision: "approve", Effect: []review.Span{
				{Kind: review.SpanText, Text: "SENTINEL-effect"},
			}}},
			Commands: []string{"SENTINEL-command"},
		},
		want: []string{"approve", "SENTINEL-effect", "SENTINEL-command"},
	},
}

func TestEveryBlockKindRenders(t *testing.T) {
	e := newEnv(t)
	for _, k := range review.AllBlockKinds {
		f, ok := blockFixtures[k]
		if !ok {
			t.Errorf("block kind %q has no fixture in blockFixtures, so this walk never asks whether it renders", k)
			continue
		}
		v := newBlockView(f.block)
		if !v.Handled {
			t.Errorf("block kind %q has no case in newBlockView; the browser would drop it", k)
			continue
		}
		html := renderNamed(t, e, "gateBlock", v)
		if strings.Contains(html, "unrenderable") {
			t.Errorf("block kind %q has no branch in gate.html:\n%s", k, html)
			continue
		}
		for _, want := range f.want {
			if !strings.Contains(html, want) {
				t.Errorf("block kind %q rendered without %q:\n%s", k, want, html)
			}
		}
	}
	// The other half of the mechanism: a kind with no case must report itself
	// as unhandled, or the loop above cannot tell a missing branch from a
	// branch that draws nothing.
	v := newBlockView(review.Block{Kind: review.BlockKind("invented")})
	if v.Handled {
		t.Fatal("an unknown block kind reported itself as handled, so this test cannot detect a missing case")
	}
	if html := renderNamed(t, e, "gateBlock", v); !strings.Contains(html, "unrenderable block: invented") {
		t.Errorf("an unhandled block vanished from the page instead of saying so:\n%s", html)
	}
}

// TestSpanKindsRoundTrip is the same guarantee one level down: every span kind
// carries its text into the page, wearing the element its kind names.
func TestSpanKindsRoundTrip(t *testing.T) {
	e := newEnv(t)
	want := map[review.SpanKind]string{
		review.SpanText:     "SENTINEL-span",
		review.SpanStrong:   "<strong>SENTINEL-span</strong>",
		review.SpanEmphasis: "<em>SENTINEL-span</em>",
		review.SpanCode:     "<code>SENTINEL-span</code>",
		review.SpanPath:     `<code class="path">SENTINEL-span</code>`,
	}
	for _, k := range review.AllSpanKinds {
		w, ok := want[k]
		if !ok {
			t.Errorf("span kind %q has no expectation here, so this walk never asks whether it renders", k)
			continue
		}
		v := newSpanView(review.Span{Kind: k, Text: "SENTINEL-span"})
		if !v.Handled {
			t.Errorf("span kind %q has no case in newSpanView", k)
			continue
		}
		got := strings.TrimSpace(renderNamed(t, e, "gateSpan", v))
		if got != w {
			t.Errorf("span kind %q rendered %q, want %q", k, got, w)
		}
	}
	v := newSpanView(review.Span{Kind: review.SpanKind("invented"), Text: "SENTINEL-span"})
	if v.Handled {
		t.Fatal("an unknown span kind reported itself as handled")
	}
	got := renderNamed(t, e, "gateSpan", v)
	if !strings.Contains(got, "unrenderable") || !strings.Contains(got, "SENTINEL-span") {
		t.Errorf("an unhandled span lost its text or its notice: %q", got)
	}
}

// --- the shaping the markdown renderer learned the hard way ---------------

// TestHeadingLevelIsClamped covers the title level 1 and the two ends of the
// clamp. markdownBlock clamps rather than trusting the field, and so does this
// one: the renderer whose job is to keep a malformed document readable may not
// be the thing that fails on one.
func TestHeadingLevelIsClamped(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		level int
		want  string
	}{
		{1, "<h1>"}, {2, "<h2>"}, {3, "<h3>"}, {6, "<h6>"},
		{0, "<h1>"}, {-3, "<h1>"}, {9, "<h6>"},
	} {
		v := newBlockView(review.Block{Kind: review.BlockHeading, Level: tc.level, Text: "T"})
		html := renderNamed(t, e, "gateBlock", v)
		if !strings.Contains(html, tc.want) {
			t.Errorf("heading level %d rendered %q, want %s", tc.level, strings.TrimSpace(html), tc.want)
		}
	}
}

// TestParagraphTrailingNewlineIsInvisible pins the newline span review.para
// appends to every paragraph. It exists so the markdown output is byte-pinned;
// in HTML it must contribute nothing, and printing it puts a stray space after
// a paragraph that ends in a code span.
func TestParagraphTrailingNewlineIsInvisible(t *testing.T) {
	e := newEnv(t)
	v := newBlockView(review.Block{Kind: review.BlockParagraph, Spans: []review.Span{
		{Kind: review.SpanCode, Text: "sdlc approve"},
		{Kind: review.SpanText, Text: "\n"},
	}})
	if n := len(v.Spans); n != 1 {
		t.Fatalf("the trailing newline span survived shaping: %d spans", n)
	}
	if got := strings.TrimSpace(renderNamed(t, e, "gateBlock", v)); got != "<p><code>sdlc approve</code></p>" {
		t.Errorf("paragraph rendered %q", got)
	}
}

// TestCodeBlockKeepsTrailingSpaces pins the hold table's padding. The %-12s
// columns leave real trailing spaces on their lines, review's golden test holds
// them to the byte, and <pre> is what carries them into the browser unchanged.
func TestCodeBlockKeepsTrailingSpaces(t *testing.T) {
	e := newEnv(t)
	// Two padded lines, so the assertion holds whether a trim would take the
	// whole block's tail or only the interior line's. Both are bytes the hold
	// table means to have.
	const table = "12:00:01  ESCALATED         hold         \n12:00:02  ESCALATED         progress     "
	v := newBlockView(review.Block{Kind: review.BlockCode, Text: table})
	if v.Text != table {
		t.Errorf("shaping changed the block's bytes:\n got %q\nwant %q", v.Text, table)
	}
	want := `<pre class="code">` + table + `</pre>`
	if got := strings.TrimSpace(renderNamed(t, e, "gateBlock", v)); got != want {
		t.Errorf("the padding did not survive the page:\n got %q\nwant %q", got, want)
	}
}

// TestActionsWithoutOutcomesStillShowTheCommands mirrors markdownBlock's rule
// that the separator appears only when there is something to separate: a gate
// this package does not know about has commands and no outcomes.
func TestActionsWithoutOutcomesStillShowTheCommands(t *testing.T) {
	e := newEnv(t)
	v := newBlockView(review.Block{Kind: review.BlockActions, Commands: []string{"sdlc approve JOB-1"}})
	html := renderNamed(t, e, "gateBlock", v)
	if strings.Contains(html, "<ul") {
		t.Errorf("an empty outcome list was drawn:\n%s", html)
	}
	if !strings.Contains(html, "sdlc approve JOB-1") {
		t.Errorf("the commands went missing:\n%s", html)
	}
}

// TestEmptyIssueBodyRendersItsPlaceholder records what the block model hands
// the browser for an issue with no body: review.renderIssue substitutes the
// literal string "_(no body)_", and nothing in this package parses markdown, so
// the underscores reach the page as characters. That is faithful rather than
// pretty, and it is pinned here so a change to either side is deliberate.
func TestEmptyIssueBodyRendersItsPlaceholder(t *testing.T) {
	e := newEnv(t)
	v := newBlockView(review.Block{Kind: review.BlockQuote, Text: "_(no body)_"})
	if html := renderNamed(t, e, "gateBlock", v); !strings.Contains(html, "_(no body)_") {
		t.Errorf("the placeholder did not reach the page:\n%s", html)
	}
}

// --- the page -------------------------------------------------------------

func TestGatePageCarriesTitleFindingsAndActions(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "Add a widget")
	writeReview(t, e, j.ID, "final_review.json", artifact.Finding{
		Severity: "major", File: "internal/widget/widget.go",
		Description:    "the retry loop never terminates",
		Recommendation: "bound it",
	})

	res := e.get("/jobs/" + j.ID + "/gate")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET gate = %d", res.StatusCode)
	}
	body := e.body(res)
	for _, want := range []string{
		"Add a widget",                          // the level-1 title block
		"Approve merging this change into main", // the document's own title line
		"the retry loop never terminates",       // the finding
		"internal/widget/widget.go",
		"sdlc approve " + j.ID,                // the actions block's commands
		`data-doc-source="live"`,              // A2's label
		`action="/jobs/` + j.ID + `/approve"`, // W2-I's decision form
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the gate page is missing %q", want)
		}
	}
}

// TestFindingDescriptionIsEscaped is §7.4 on the page that matters most: a
// reviewer's browser executing an implementer's payload is a straight line from
// "agent output" to "approve button".
func TestFindingDescriptionIsEscaped(t *testing.T) {
	e := newEnv(t)
	const payload = `<img src=x onerror="alert(1)">`
	j := e.job("AWAITING_MERGE_APPROVAL", payload)
	writeReview(t, e, j.ID, "final_review.json", artifact.Finding{
		Severity: "blocker", File: payload, Description: payload, Recommendation: payload,
	})
	body := e.body(e.get("/jobs/" + j.ID + "/gate"))
	if strings.Contains(body, payload) {
		t.Error("agent text reached the page unescaped")
	}
	if !strings.Contains(body, "&lt;img src=x onerror=") {
		t.Errorf("the escaped description is not on the page at all:\n%s", body)
	}
}

// TestGatePageFallsBackToPersistedDocument is A2. The live render is made
// impossible the way it actually becomes impossible — the job names a target
// the loaded config no longer defines — and the page must both show the saved
// copy and say that is what it is showing.
func TestGatePageFallsBackToPersistedDocument(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "Add a widget")
	const saved = "# saved snapshot of the merge gate\n\nSENTINEL-persisted\n"
	writeGateDoc(t, e, j.ID, "merge", saved)

	j.Target = "renamed-away"
	if err := e.st.UpdateJob(j); err != nil {
		t.Fatal(err)
	}

	res := e.get("/jobs/" + j.ID + "/gate")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET gate = %d", res.StatusCode)
	}
	body := e.body(res)
	if !strings.Contains(body, "SENTINEL-persisted") {
		t.Errorf("the persisted document is not on the page:\n%s", body)
	}
	if !strings.Contains(body, `data-doc-source="persisted"`) {
		t.Error("the page does not say the reader has a snapshot rather than a live render")
	}
	if strings.Contains(body, `data-doc-source="live"`) {
		t.Error("the page claims to be live while serving the saved copy")
	}
	// gateDoc's own answer, so the label and the source cannot disagree.
	_, src, err := e.srv.gateDoc(context.Background(), j, "merge")
	if err != nil || src != docPersisted {
		t.Fatalf("gateDoc = %v, %v; want the persisted source", src, err)
	}
}

// TestGateDocPrefersLiveOverThePersistedCopy is the other half of A2: the saved
// copy exists, the live render works, and the live render is what the reviewer
// gets. When the two disagree it is because somebody touched the worktree, and
// the live truth is what belongs beside the approve button.
func TestGateDocPrefersLiveOverThePersistedCopy(t *testing.T) {
	e := newEnv(t)
	j := e.job("AWAITING_MERGE_APPROVAL", "Add a widget")
	writeGateDoc(t, e, j.ID, "merge", "SENTINEL-persisted\n")

	doc, src, err := e.srv.gateDoc(context.Background(), j, "merge")
	if err != nil {
		t.Fatal(err)
	}
	if src != docLive {
		t.Fatalf("gateDoc chose %q with a working live render", src)
	}
	if strings.Contains(doc.Body, "SENTINEL-persisted") {
		t.Error("the live document carries the saved copy's text")
	}
	if body := e.body(e.get("/jobs/" + j.ID + "/gate")); strings.Contains(body, "SENTINEL-persisted") {
		t.Error("the page served the saved copy while a live render was available")
	}
}

// TestGatePageWithNothingWaiting: a job nobody is waiting on is a 200 with the
// same sentence review.Render produces, not an error page (spec §5.1).
func TestGatePageWithNothingWaiting(t *testing.T) {
	e := newEnv(t)
	j := e.job("BUILDING", "Add a widget")
	res := e.get("/jobs/" + j.ID + "/gate")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET gate = %d, want 200", res.StatusCode)
	}
	body := e.body(res)
	if !strings.Contains(body, "nothing is waiting on you") {
		t.Errorf("the page does not say why there is nothing to decide:\n%s", body)
	}
	if strings.Contains(body, "data-doc-source") {
		t.Error("the page labelled a document it does not have")
	}
}

// TestHoldGateOffersResumeAndCancel: a held job is cleared with resume or
// cancel, not approve or reject, so the page invokes W2-I's holdForm instead of
// its decisionForm. Neither form is defined here — a second copy of the approve
// button is exactly the thing exclusive ownership of _forms.html prevents.
func TestHoldGateOffersResumeAndCancel(t *testing.T) {
	e := newEnv(t)
	j := e.job("ESCALATED", "Add a widget")
	body := e.body(e.get("/jobs/" + j.ID + "/gate"))
	for _, want := range []string{
		"This job stopped and needs a decision",
		`action="/jobs/` + j.ID + `/resume"`,
		`action="/jobs/` + j.ID + `/cancel"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the hold page is missing %q", want)
		}
	}
	if strings.Contains(body, `/approve"`) {
		t.Error("the hold page offers an approve button")
	}
}

// --- helpers --------------------------------------------------------------

// renderNamed executes one of gate.html's named templates, which is how these
// tests reach a single block without a whole page around it. The template comes
// from the parsed set the server actually serves, not from a copy parsed here.
func renderNamed(t *testing.T, e *env, name string, data any) string {
	t.Helper()
	tpl, ok := e.srv.tpl["gate.html"]
	if !ok {
		t.Fatal("gate.html was not parsed")
	}
	var b strings.Builder
	if err := tpl.ExecuteTemplate(&b, name, data); err != nil {
		t.Fatalf("executing %q: %v", name, err)
	}
	return b.String()
}

func writeReview(t *testing.T, e *env, jobID, name string, findings ...artifact.Finding) {
	t.Helper()
	dir := artifact.ArtifactsDir(e.cfg.Orchestrator.DataDir, jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(artifact.Review{
		Schema: "review/1", Reviewed: "final", Summary: "one finding", Findings: findings,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeGateDoc puts a document where review.Write puts one, which is the only
// place gateDoc looks for the fallback.
func writeGateDoc(t *testing.T, e *env, jobID, gate, body string) {
	t.Helper()
	p := review.DocPath(e.cfg.Orchestrator.DataDir, jobID, gate)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNestedSpansReachTheBrowser holds the one nesting the document uses:
// emphasis wrapping inline code, as in the asides that quote a command.
//
// It exists because the exhaustiveness walk cannot catch this. That walk asks
// whether every KIND has a case, and every kind did — while a wrapping span's
// children were dropped on the floor and four asides rendered as empty italics.
// A kind being handled is not the same as a span being rendered.
func TestNestedSpansReachTheBrowser(t *testing.T) {
	e := newEnv(t)
	blk := review.Block{Kind: review.BlockParagraph, Spans: []review.Span{{
		Kind: review.SpanEmphasis,
		Children: []review.Span{
			{Kind: review.SpanText, Text: "Re-run with "},
			{Kind: review.SpanCode, Text: "--diff"},
			{Kind: review.SpanText, Text: " to read it inline."},
		},
	}}}
	got := renderNamed(t, e, "gateBlock", newBlockView(blk))
	for _, want := range []string{"<em>", "Re-run with ", "<code>--diff</code>", " to read it inline.", "</em>"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML is missing %q:\n%s", want, got)
		}
	}
	// The markdown surface's literal backticks must not survive into HTML: a
	// grave accent here means somebody went back to a flat emphasis string.
	if strings.Contains(got, "`") {
		t.Errorf("a literal backtick reached the page, so the aside is flat text again:\n%s", got)
	}
}
