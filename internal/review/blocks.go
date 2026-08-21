package review

import (
	"fmt"
	"strings"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
)

// BlockKind is the closed set of blocks a gate document is made of. Today
// exactly one renderer switches over it — markdownBlock, in this file — and
// three tests keep that switch honest: TestEveryBlockKindIsRegistered parses
// this file with go/ast and fails the build when a constant declared below is
// missing from AllBlockKinds, TestEveryBlockKindHasAMarkdownCase walks that
// slice and fails when a kind has no case, and TestUnrenderableBlockSaysSo
// pins what an unhandled kind degrades to.
//
// There is no HTML renderer yet. When one arrives it switches over the same
// registered set, and the pair of tests above is what will stop a kind from
// drawing in the terminal and vanishing in the browser: whoever writes it
// should add the HTML equivalent of TestEveryBlockKindHasAMarkdownCase rather
// than trusting the two switches to be kept in step by hand.
type BlockKind string

const (
	BlockHeading   BlockKind = "heading"
	BlockParagraph BlockKind = "paragraph"
	BlockFacts     BlockKind = "facts"
	BlockBullets   BlockKind = "bullets"
	BlockQuote     BlockKind = "quote"
	BlockCode      BlockKind = "code"
	BlockFindings  BlockKind = "findings"
	BlockSteps     BlockKind = "steps"
	BlockActions   BlockKind = "actions"
)

// AllBlockKinds is what the exhaustiveness tests walk. Keeping it in step with
// the const block above is not left to convention:
// TestEveryBlockKindIsRegistered reads this file's syntax tree and compares the
// two sets, so a kind added there and not here fails the suite whether or not
// any render path happens to use it.
var AllBlockKinds = []BlockKind{
	BlockHeading, BlockParagraph, BlockFacts, BlockBullets,
	BlockQuote, BlockCode, BlockFindings, BlockSteps, BlockActions,
}

type SpanKind string

const (
	SpanText     SpanKind = "text"
	SpanStrong   SpanKind = "strong"   // markdown **x**
	SpanEmphasis SpanKind = "emphasis" // markdown _x_
	SpanCode     SpanKind = "code"     // markdown `x`
	SpanPath     SpanKind = "path"     // markdown `x`; a filesystem path
)

// AllSpanKinds is the span-level twin of AllBlockKinds, held to the const block
// below it by TestEverySpanKindIsRegistered for the same reason.
var AllSpanKinds = []SpanKind{SpanText, SpanStrong, SpanEmphasis, SpanCode, SpanPath}

type Span struct {
	Kind SpanKind
	Text string
}

// Item is one bullet plus its indented sub-bullets.
type Item struct {
	Spans []Span
	Sub   [][]Span
}

// Fact is one row of the state/branch/worktree/artifacts list every document
// opens with.
type Fact struct {
	Label string // "state", "branch", "worktree", "artifacts"
	Value string // rendered as code on both surfaces
	Note  string // "" or a parenthesised aside, e.g. "waiting 4m"
}

// Action is one outcome of a decision, and its Decision names the button.
type Action struct {
	Decision string // approve | reject | resume | cancel
	Effect   []Span
}

// Block is one node of the document. It is a single struct with a Kind rather
// than an interface because html/template cannot type-switch: a template
// dispatches on {{.Kind}} and reads the fields that kind uses.
type Block struct {
	Kind BlockKind

	// BlockHeading: 1 for the document title and 2 for a section, which is
	// every heading Render builds today. markdownBlock clamps anything outside
	// 1..6 rather than trusting the field, and the HTML renderer must handle
	// level 1 — it is the title every document opens with.
	Level     int
	Text      string // BlockHeading text; BlockCode body; BlockQuote body
	Lang      string // BlockCode: "" or "diff"
	Truncated bool   // BlockQuote: the source was cut at 40 lines

	Spans    []Span
	Items    []Item
	Facts    []Fact
	Findings []artifact.Finding // already ordered; each surface formats its own badge
	Steps    []artifact.PlanStep
	Actions  []Action
	Commands []string // BlockActions: the CLI equivalents, one per line
}

// renderMarkdown concatenates each block's own markdown, trailing blank line
// included. Blocks are deliberately NOT joined with a separator: the document's
// spacing is irregular by construction — a facts list runs line-by-line with a
// single blank line after it, a fenced block ends with exactly one newline, a
// findings list has none between entries — and the golden test holds all of it
// to the byte. Each block therefore owns the whitespace that follows it.
func renderMarkdown(blocks []Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		s, ok := markdownBlock(blk)
		if !ok {
			// Unreachable while TestEveryBlockKindHasAMarkdownCase passes. If it
			// is ever reached, the block says so where the reader will see it:
			// a block that silently disappeared from a merge document is the
			// failure this model exists to prevent, and a document that is short
			// one section looks exactly like a document that had nothing to say.
			s = fmt.Sprintf("_unrenderable block: %s_\n\n", blk.Kind)
		}
		b.WriteString(s)
	}
	return b.String()
}

// markdownBlock renders one block, reporting whether its kind was handled at
// all. The bool is the whole mechanism behind
// TestEveryBlockKindHasAMarkdownCase: a kind that falls through to the default
// is detectable here and nowhere else, since a missing case and a block with
// nothing to print both produce no output.
func markdownBlock(blk Block) (string, bool) {
	var b strings.Builder
	switch blk.Kind {
	case BlockHeading:
		// Clamped, not trusted: strings.Repeat panics on a negative count, and
		// a level of 0 renders a leading space and no heading at all. This is
		// the one function whose job is to keep a malformed document readable,
		// so it may not be the thing that crashes the process showing a human
		// their gate.
		fmt.Fprintf(&b, "%s %s\n\n", strings.Repeat("#", headingLevel(blk.Level)), blk.Text)

	case BlockParagraph:
		// One trailing newline, not two. The blank line that follows a
		// paragraph in the body of the document is carried by para()'s final
		// span, because Write's "Full patch:" line is a paragraph that ends
		// with exactly one newline (golden_test.go's
		// TestFullPatchLineIsExactBytes) — so the renderer cannot be the thing
		// that decides every paragraph is followed by a blank line.
		writeSpans(&b, blk.Spans)
		b.WriteString("\n")

	case BlockFacts:
		for _, f := range blk.Facts {
			fmt.Fprintf(&b, "- %s: `%s`", f.Label, f.Value)
			if f.Note != "" {
				fmt.Fprintf(&b, " (%s)", f.Note)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")

	case BlockBullets:
		for _, it := range blk.Items {
			b.WriteString("- ")
			writeSpans(&b, it.Spans)
			b.WriteString("\n")
			for _, sub := range it.Sub {
				b.WriteString("  - ")
				writeSpans(&b, sub)
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")

	case BlockQuote:
		// Every line is prefixed unconditionally, blank ones included: "> " on
		// its own keeps a paragraph break inside the quote instead of joining
		// two paragraphs of an issue body into one.
		lines := strings.Split(blk.Text, "\n")
		for _, ln := range lines {
			b.WriteString("> " + ln + "\n")
		}
		if blk.Truncated {
			b.WriteString(">\n> _(truncated)_\n")
		}
		b.WriteString("\n")

	case BlockCode:
		// Text is written exactly as it arrived. Trailing spaces inside a fence
		// are real output — the event table pads its columns — and trimming
		// them here would be this renderer changing the document's words.
		fmt.Fprintf(&b, "```%s\n%s\n```\n\n", blk.Lang, blk.Text)

	case BlockFindings:
		for _, f := range blk.Findings {
			loc := ""
			if f.File != "" {
				loc = " `" + f.File + "`"
			}
			fmt.Fprintf(&b, "- **[%s]**%s %s\n", f.Severity, loc, f.Description)
			if f.Recommendation != "" {
				fmt.Fprintf(&b, "  - recommendation: %s\n", f.Recommendation)
			}
			if v := f.Verification; v != nil {
				verdict := "refuted"
				if v.Survived {
					verdict = "confirmed"
				}
				fmt.Fprintf(&b, "  - verifiers: %s %d/%d (was %s)\n", verdict, v.Confirmed, v.Votes, v.OriginalSeverity)
			}
		}
		b.WriteString("\n")

	case BlockSteps:
		for _, s := range blk.Steps {
			fmt.Fprintf(&b, "- **%s** %s\n", s.ID, s.Description)
			if len(s.Files) > 0 {
				fmt.Fprintf(&b, "  - files: %s\n", strings.Join(s.Files, ", "))
			}
			if s.Verification != "" {
				fmt.Fprintf(&b, "  - verify: %s\n", s.Verification)
			}
		}
		b.WriteString("\n")

	case BlockActions:
		for _, a := range blk.Actions {
			fmt.Fprintf(&b, "- %s → ", a.Decision)
			writeSpans(&b, a.Effect)
			b.WriteString("\n")
		}
		if len(blk.Actions) > 0 {
			// The blank line separates the outcomes from the commands. With no
			// outcomes to separate there is none, which is what a gate this
			// package does not know about renders today.
			b.WriteString("\n")
		}
		b.WriteString("```\n")
		for _, c := range blk.Commands {
			b.WriteString(c + "\n")
		}
		b.WriteString("```\n")

	default:
		return "", false
	}
	return b.String(), true
}

func writeSpans(b *strings.Builder, spans []Span) {
	for _, s := range spans {
		text, ok := markdownSpan(s)
		if !ok {
			// Same reasoning as renderMarkdown's, and the same shape: unhandled
			// is visible, never silently dropped. Carrying the text through
			// unmarked — which is what this used to do — turns a lost `x` into
			// plain x, a difference no reader can see and no test can catch.
			// The text still survives; only its emphasis is replaced by a
			// statement that the emphasis was lost.
			text = fmt.Sprintf("_unrenderable %s span: %s_", s.Kind, s.Text)
		}
		b.WriteString(text)
	}
}

// markdownSpan renders one inline span, reporting whether its kind was handled.
// The span set is closed for the same reason the block set is:
// TestEverySpanKindIsRegistered holds AllSpanKinds to the const block and
// TestEverySpanKindHasAMarkdownCase holds this switch to AllSpanKinds, so a
// span kind cannot exist without a rendering. A future HTML renderer switches
// over the same registered set and needs its own equivalent of the second
// test; the first one already covers both surfaces.
func markdownSpan(s Span) (string, bool) {
	switch s.Kind {
	case SpanText:
		return s.Text, true
	case SpanStrong:
		return "**" + s.Text + "**", true
	case SpanEmphasis:
		return "_" + s.Text + "_", true
	case SpanCode, SpanPath:
		// Both are backticks in markdown; they differ only in what the HTML
		// renderer is told the string is, which is why SpanPath exists at all.
		return "`" + s.Text + "`", true
	}
	return "", false
}

// para is a paragraph followed by a blank line — which is every paragraph the
// document builds, and not the one Write appends. The trailing newline is a
// span rather than something markdownBlock adds, because §2.4 pins Write's
// "Full patch:" line as a BlockParagraph of exactly two spans rendering to one
// trailing newline; a renderer that added the blank line itself could not
// produce both shapes.
//
// The newline is its own SpanText and never folded into the preceding span: a
// paragraph ending in an emphasis or a code span would otherwise close its
// delimiter after the newline and emit "_x\n_".
// The result never aliases the caller's slice. append(spans, ...) writes into
// the variadic backing array, so para(effect...) followed by a second
// para(effect...) — or by any later append to effect — would have the two
// documents scribbling over each other's last span. No call site spreads a
// slice today; the copy is what makes the first one that does harmless.
func para(spans ...Span) Block {
	out := make([]Span, len(spans), len(spans)+1)
	copy(out, spans)
	return Block{Kind: BlockParagraph, Spans: append(out, Span{Kind: SpanText, Text: "\n"})}
}

// headingLevel clamps to the levels markdown has. Anything below 1 would make
// strings.Repeat panic or emit a heading with no hashes; anything above 6 is
// not a heading in any renderer, so it draws as the deepest one there is.
func headingLevel(n int) int {
	if n < 1 {
		return 1
	}
	if n > 6 {
		return 6
	}
	return n
}
