package review

import (
	"fmt"
	"strings"

	"github.com/vipinm/sdlc-orchestrator/internal/artifact"
)

// BlockKind is the closed set of blocks a gate document is made of. Both
// renderers switch over this set, and TestEveryBlockKindRenders fails the build
// the moment a kind exists with no HTML case — which is what makes drift
// between the terminal and the browser unrepresentable rather than merely
// testable.
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

// AllBlockKinds is what the exhaustiveness tests walk. A kind added to the
// const block but not here is invisible to them, so the two are updated
// together or the mechanism is off.
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

	Level     int    // BlockHeading: 2 or 3
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
		fmt.Fprintf(&b, "%s %s\n\n", strings.Repeat("#", blk.Level), blk.Text)

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
			// Same reasoning as renderMarkdown's: unhandled is visible, never
			// silently dropped. The text survives even when its emphasis does.
			text = s.Text
		}
		b.WriteString(text)
	}
}

// markdownSpan renders one inline span, reporting whether its kind was handled.
// The span set is closed for the same reason the block set is: the HTML
// renderer switches over the same list, and TestEverySpanKindHasAMarkdownCase
// is what stops one surface from learning a span the other cannot draw.
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
func para(spans ...Span) Block {
	return Block{Kind: BlockParagraph, Spans: append(spans, Span{Kind: SpanText, Text: "\n"})}
}
