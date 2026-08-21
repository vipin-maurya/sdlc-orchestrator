package review

// These tests hold the block model's two promises: that the terminal document
// is produced from the tree rather than beside it, and that no kind can exist
// without a renderer.
//
// The second promise is enforced in two links, because either one alone is a
// convention rather than a mechanism. TestEveryBlockKindIsRegistered and
// TestEverySpanKindIsRegistered read blocks.go's syntax tree and hold the
// AllBlockKinds / AllSpanKinds slices to the const blocks they claim to
// enumerate; TestEveryBlockKindHasAMarkdownCase and
// TestEverySpanKindHasAMarkdownCase then walk those slices and hold
// markdownBlock / markdownSpan to them. A kind added to a const block and
// nowhere else fails the first pair whether or not any document happens to
// use it.
//
// There is no HTML renderer yet. When there is one, it switches over the same
// registered set and needs its own equivalent of the second pair — the first
// pair already serves both surfaces.

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
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

// --- the registration mechanism -------------------------------------------

// blocksGoSource is the file the const blocks and the registration slices both
// live in. Reading it as source rather than reflecting over the package is the
// whole point: a constant that exists but is referenced nowhere is invisible
// at run time, and invisible is exactly the failure mode being ruled out.
const blocksGoSource = "blocks.go"

// TestEveryBlockKindIsRegistered fails when a BlockKind constant is missing
// from AllBlockKinds. Without it the exhaustiveness tests are conventional:
// they walk the slice, so a kind left out of the slice is a kind they never
// ask about, and a document using it degrades to "_unrenderable block_" with
// the whole suite green.
func TestEveryBlockKindIsRegistered(t *testing.T) {
	declared := declaredConstants(t, "BlockKind")
	registered := registeredIdents(t, "AllBlockKinds")
	assertSameSet(t, "BlockKind", "AllBlockKinds", declared, registered)
	// The mechanism only works if the parse found something; a rename that
	// made both sets empty would otherwise pass silently.
	if len(declared) == 0 {
		t.Fatalf("no BlockKind constants were found in %s; this test is no longer reading what it thinks it is", blocksGoSource)
	}
	// And the run-time slice must agree with the parsed one, or the test is
	// checking source that the binary does not use.
	if len(AllBlockKinds) != len(registered) {
		t.Errorf("AllBlockKinds has %d entries at run time and %d in the source", len(AllBlockKinds), len(registered))
	}
}

// TestEverySpanKindIsRegistered is the same guarantee one level down, and the
// level where the omission is worse: writeSpans renders an unregistered kind
// through its fallback, so an unregistered SpanCode-alike would have lost its
// backticks with no test to notice.
func TestEverySpanKindIsRegistered(t *testing.T) {
	declared := declaredConstants(t, "SpanKind")
	registered := registeredIdents(t, "AllSpanKinds")
	assertSameSet(t, "SpanKind", "AllSpanKinds", declared, registered)
	if len(declared) == 0 {
		t.Fatalf("no SpanKind constants were found in %s; this test is no longer reading what it thinks it is", blocksGoSource)
	}
	if len(AllSpanKinds) != len(registered) {
		t.Errorf("AllSpanKinds has %d entries at run time and %d in the source", len(AllSpanKinds), len(registered))
	}
}

// parseBlocksGo parses the source of the file the kinds are declared in.
// go/ast is stdlib, so this costs the package no dependency.
func parseBlocksGo(t *testing.T) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), blocksGoSource, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", blocksGoSource, err)
	}
	return f
}

// declaredConstants returns the names of every constant declared with the
// named type. A grouped const declaration may state the type once and let the
// following specs inherit it, so the type in force is carried down the group
// exactly as the compiler carries it.
func declaredConstants(t *testing.T, typeName string) []string {
	t.Helper()
	var names []string
	for _, decl := range parseBlocksGo(t).Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		inForce := ""
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if vs.Type != nil {
				if id, ok := vs.Type.(*ast.Ident); ok {
					inForce = id.Name
				} else {
					inForce = ""
				}
			}
			if inForce != typeName {
				continue
			}
			for _, n := range vs.Names {
				if n.Name != "_" {
					names = append(names, n.Name)
				}
			}
		}
	}
	return names
}

// registeredIdents returns the identifiers listed in the named slice literal.
func registeredIdents(t *testing.T, varName string) []string {
	t.Helper()
	var names []string
	found := false
	for _, decl := range parseBlocksGo(t).Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != varName || len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.CompositeLit)
			if !ok {
				t.Fatalf("%s is not a slice literal, so this test can no longer read it", varName)
			}
			found = true
			for _, el := range lit.Elts {
				id, ok := el.(*ast.Ident)
				if !ok {
					t.Fatalf("%s contains an element that is not a plain identifier; this test only understands named constants", varName)
				}
				names = append(names, id.Name)
			}
		}
	}
	if !found {
		t.Fatalf("no declaration of %s was found in %s", varName, blocksGoSource)
	}
	return names
}

func assertSameSet(t *testing.T, typeName, varName string, declared, registered []string) {
	t.Helper()
	inVar := make(map[string]bool, len(registered))
	for _, n := range registered {
		if inVar[n] {
			t.Errorf("%s lists %s twice", varName, n)
		}
		inVar[n] = true
	}
	inConst := make(map[string]bool, len(declared))
	for _, n := range declared {
		inConst[n] = true
	}
	var missing, extra []string
	for _, n := range declared {
		if !inVar[n] {
			missing = append(missing, n)
		}
	}
	for _, n := range registered {
		if !inConst[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("%s constant(s) %s are declared in %s but not listed in %s. "+
			"Every exhaustiveness test walks %s, so an unregistered kind renders through the fallback with the suite green — add it there.",
			typeName, strings.Join(missing, ", "), blocksGoSource, varName, varName)
	}
	if len(extra) > 0 {
		t.Errorf("%s lists %s, which is not a %s constant in %s",
			varName, strings.Join(extra, ", "), typeName, blocksGoSource)
	}
}

// --- visible degradation ---------------------------------------------------

// TestUnrenderableBlockSaysSo pins renderMarkdown's fallback. It had no test
// anywhere, which is an odd gap for the one line whose entire job is to be
// noticed.
func TestUnrenderableBlockSaysSo(t *testing.T) {
	got := renderMarkdown([]Block{{Kind: BlockKind("callout")}})
	if want := "_unrenderable block: callout_\n\n"; got != want {
		t.Errorf("renderMarkdown of an unknown kind = %q, want %q", got, want)
	}
}

// TestUnrenderableSpanSaysSo is the span-level twin. The fallback used to emit
// the span's text bare, so an unhandled `x` became x: a document that reads as
// if it never had a code span at all, and a difference no golden could ever
// show as anything but ordinary wording.
func TestUnrenderableSpanSaysSo(t *testing.T) {
	var b strings.Builder
	writeSpans(&b, []Span{
		{Kind: SpanText, Text: "before "},
		{Kind: SpanKind("keycap"), Text: "x"},
		{Kind: SpanText, Text: " after"},
	})
	got := b.String()
	if want := "before _unrenderable keycap span: x_ after"; got != want {
		t.Errorf("writeSpans with an unknown kind = %q, want %q", got, want)
	}
	if !strings.Contains(got, "x") {
		t.Error("the fallback dropped the span's text; it is meant to survive without its emphasis")
	}
}

// --- heading levels --------------------------------------------------------

// TestHeadingLevelIsClamped covers the one input that could crash the process
// rendering a gate: strings.Repeat panics on a negative count, so a heading
// built with a computed level used to take the whole command down inside the
// function whose stated job is visible degradation.
func TestHeadingLevelIsClamped(t *testing.T) {
	for _, tc := range []struct {
		level int
		want  string
	}{
		{-1, "# x\n\n"},
		{0, "# x\n\n"},
		{1, "# x\n\n"},
		{2, "## x\n\n"},
		{6, "###### x\n\n"},
		{7, "###### x\n\n"},
		{99, "###### x\n\n"},
	} {
		t.Run(fmt.Sprintf("level %d", tc.level), func(t *testing.T) {
			got, ok := markdownBlock(Block{Kind: BlockHeading, Level: tc.level, Text: "x"})
			if !ok {
				t.Fatal("BlockHeading reported itself unhandled")
			}
			if got != tc.want {
				t.Errorf("level %d rendered %q, want %q", tc.level, got, tc.want)
			}
		})
	}
}

// --- para must not alias its caller ---------------------------------------

// TestParaDoesNotWriteIntoItsCallersSlice covers the shape no call site uses
// yet. Spreading a slice into a variadic parameter passes the caller's own
// backing array, so append(spans, newline) writes the newline into the
// caller's spare capacity — and the caller's next append overwrites it, from
// inside a Block that was already finished.
func TestParaDoesNotWriteIntoItsCallersSlice(t *testing.T) {
	// Capacity beyond length is what makes the aliasing possible; a slice
	// built up by append has it routinely.
	effect := make([]Span, 1, 4)
	effect[0] = Span{Kind: SpanText, Text: "merged into main"}

	built := para(effect...)
	// The caller carries on using its slice, exactly as a builder would.
	effect = append(effect, Span{Kind: SpanCode, Text: "--cancel"})

	if got := renderMarkdown([]Block{built}); got != "merged into main\n\n" {
		t.Errorf("the finished paragraph changed when its caller appended: %q", got)
	}
	// Two paragraphs off one slice must not share a span either.
	first, second := para(effect...), para(effect...)
	second.Spans[len(second.Spans)-1] = Span{Kind: SpanText, Text: "!"}
	if last := first.Spans[len(first.Spans)-1]; last.Text != "\n" {
		t.Errorf("two paragraphs share their trailing span: the first now ends %q", last.Text)
	}
	if len(effect) != 2 || effect[1].Text != "--cancel" {
		t.Errorf("para modified the caller's slice: %+v", effect)
	}
}

// --- the nested bullet shape ----------------------------------------------

// TestNestedBulletsRenderIndented pins Item.Sub. No call site produces a
// sub-bullet today, so this is the only thing that says what the shape is —
// and the HTML renderer has to draw the same one. Two spaces and "- ", the
// indentation markdown needs for a nested list.
func TestNestedBulletsRenderIndented(t *testing.T) {
	got, ok := markdownBlock(Block{Kind: BlockBullets, Items: []Item{
		{
			Spans: []Span{{Kind: SpanText, Text: "internal/execx/execx.go"}},
			Sub: [][]Span{
				{{Kind: SpanText, Text: "redacts before capture"}},
				{{Kind: SpanCode, Text: "RunCapture"}, {Kind: SpanText, Text: " only"}},
			},
		},
		{Spans: []Span{{Kind: SpanText, Text: "internal/execx/redact.go"}}},
	}})
	if !ok {
		t.Fatal("BlockBullets reported itself unhandled")
	}
	want := "- internal/execx/execx.go\n" +
		"  - redacts before capture\n" +
		"  - `RunCapture` only\n" +
		"- internal/execx/redact.go\n" +
		"\n"
	if got != want {
		t.Errorf("nested bullets rendered\n%q\nwant\n%q", got, want)
	}
}
