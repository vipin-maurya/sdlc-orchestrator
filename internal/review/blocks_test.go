package review

// These tests hold the block model's two promises: that the terminal document
// is produced from the tree rather than beside it, and that no kind can exist
// without a renderer. The second one is the mechanism the HTML renderer's
// TestEveryBlockKindRenders pairs with — together they make a block that draws
// in one surface and vanishes in the other impossible to write, rather than
// merely possible to catch.

import (
	"context"
	"testing"
)

// TestBodyIsRenderedFromBlocks proves Body is a function of Blocks. The golden
// test already pins what Body says; this pins where it comes from, which is
// what stops a later change from adding a section to the markdown and not to
// the tree — a gate document that reads correctly in the terminal and is
// missing a paragraph in the browser.
func TestBodyIsRenderedFromBlocks(t *testing.T) {
	for _, gate := range []string{GateSpec, GateCode, GateMerge, GateRelease, GateHold} {
		t.Run(gate, func(t *testing.T) {
			doc, err := Render(context.Background(), goldenOptions(gate))
			if err != nil {
				t.Fatalf("Render(%s): %v", gate, err)
			}
			if len(doc.Blocks) == 0 {
				t.Fatal("the document has no blocks")
			}
			if got := renderMarkdown(doc.Blocks); got != doc.Body {
				t.Errorf("Body is not what the blocks render to:\n%s", firstDifference(got, doc.Body))
			}
		})
	}
}

// TestEveryBlockKindHasAMarkdownCase walks the closed set. A kind with no case
// in markdownBlock reports ok == false, which is the only way the omission is
// visible: a missing case and a block with nothing to print both produce no
// output, so comparing rendered strings would pass either way.
func TestEveryBlockKindHasAMarkdownCase(t *testing.T) {
	for _, k := range AllBlockKinds {
		if _, ok := markdownBlock(Block{Kind: k}); !ok {
			t.Errorf("block kind %q has no case in markdownBlock; the terminal would drop it silently", k)
		}
	}
	if _, ok := markdownBlock(Block{Kind: BlockKind("invented")}); ok {
		t.Error("an unknown block kind reported itself as handled, so this test cannot detect a missing case")
	}
}

// TestEverySpanKindHasAMarkdownCase is the same guarantee one level down.
func TestEverySpanKindHasAMarkdownCase(t *testing.T) {
	for _, k := range AllSpanKinds {
		got, ok := markdownSpan(Span{Kind: k, Text: "x"})
		if !ok {
			t.Errorf("span kind %q has no case in markdownSpan", k)
			continue
		}
		// Every span kind carries its text through; an emphasis that ate its
		// own content would still report ok.
		if got == "" {
			t.Errorf("span kind %q rendered nothing", k)
		}
	}
	if _, ok := markdownSpan(Span{Kind: SpanKind("invented"), Text: "x"}); ok {
		t.Error("an unknown span kind reported itself as handled, so this test cannot detect a missing case")
	}
}
