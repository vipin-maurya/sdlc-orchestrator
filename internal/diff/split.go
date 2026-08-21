package diff

import "unicode/utf8"

// Row is one line of the side-by-side view. A change block pairs the i-th
// deletion with the i-th addition; whichever side runs out gets filler.
type Row struct {
	Left, Right       Line
	HasLeft, HasRight bool
	// LeftStart/LeftEnd and RightStart/RightEnd are BYTE offsets into the
	// respective Line.Text delimiting the intra-line change span; equal values
	// mean nothing is marked. They are byte offsets because the template slices
	// Text with them, and they are computed on rune boundaries so a slice can
	// never split a UTF-8 sequence.
	LeftStart, LeftEnd   int
	RightStart, RightEnd int
}

// intraLineGuard is spec §8.4's 70%: past it, the marks are dropped.
const intraLineGuard = 0.7

// Split pairs one hunk's lines for side-by-side display.
//
// Pairing is positional, not an LCS. A maximal run of deletions followed by a
// maximal run of additions is one change block, and the i-th of each are shown
// opposite one another. That is what every side-by-side viewer does, it is
// O(n), and it is right for the overwhelmingly common case of an edited line.
// An LCS over the block would be better on the rare reordered-lines case and
// worse everywhere else — it produces a different pairing than the unified view
// implies, and the two views are two renderings of one model that must not tell
// different stories (TestSplitMatchesUnified).
func Split(h Hunk) []Row {
	rows := make([]Row, 0, len(h.Lines))
	for i := 0; i < len(h.Lines); {
		if h.Lines[i].Kind == KindContext {
			l := h.Lines[i]
			rows = append(rows, Row{Left: l, Right: l, HasLeft: true, HasRight: true})
			i++
			continue
		}
		// A block is [deletions][additions]. Either run may be empty: a pure
		// insertion has no deletions before it and a pure deletion no additions
		// after it, and both then pair against filler.
		start := i
		for i < len(h.Lines) && h.Lines[i].Kind == KindDel {
			i++
		}
		dels := h.Lines[start:i]
		start = i
		for i < len(h.Lines) && h.Lines[i].Kind == KindAdd {
			i++
		}
		adds := h.Lines[start:i]
		// A line that is neither context nor del nor add consumes nothing, so
		// without this the loop never advances past it and Split spins forever.
		// Kind is a closed set and addLine only ever writes the three, so this
		// is unreachable from Parse — but Split is exported, takes an exported
		// struct with an exported integer field, and a hang in a request path
		// is a denial of service. One line buys immunity.
		if len(dels) == 0 && len(adds) == 0 {
			i++
			continue
		}

		n := len(dels)
		if len(adds) > n {
			n = len(adds)
		}
		for k := 0; k < n; k++ {
			var r Row
			if k < len(dels) {
				r.Left, r.HasLeft = dels[k], true
			}
			if k < len(adds) {
				r.Right, r.HasRight = adds[k], true
			}
			if r.HasLeft && r.HasRight {
				markIntraLine(&r)
			}
			rows = append(rows, r)
		}
	}
	return rows
}

// markIntraLine narrows a paired row to the span that actually changed: the
// text between the two lines' common rune prefix and common rune suffix.
func markIntraLine(r *Row) {
	a, b := r.Left.Text, r.Right.Text
	if a == b {
		return
	}
	start := commonPrefix(a, b)
	endA, endB := commonSuffix(a[start:], b[start:])
	endA += start
	endB += start

	// The guard. Two lines that were paired only because they sit at the same
	// index in a change block share almost nothing, so the "changed span" comes
	// out as most of both — every character lit up, reading as a rewrite of a
	// line that was merely replaced. Marking nothing is the honest rendering of
	// "these two are not versions of each other".
	span := endA - start
	if e := endB - start; e > span {
		span = e
	}
	longer := len(a)
	if len(b) > longer {
		longer = len(b)
	}
	if longer == 0 || float64(span) > intraLineGuard*float64(longer) {
		return
	}
	r.LeftStart, r.LeftEnd = start, endA
	r.RightStart, r.RightEnd = start, endB
}

// commonPrefix returns the byte length of the leading runes a and b share. It
// steps rune by rune rather than byte by byte so the offset it returns can
// never land inside a UTF-8 sequence — a template slicing Text at a mid-rune
// offset renders a replacement character in place of the letter.
func commonPrefix(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) {
		ra, wa := utf8.DecodeRuneInString(a[i:])
		rb, wb := utf8.DecodeRuneInString(b[i:])
		if ra != rb || wa != wb {
			return i
		}
		i += wa
	}
	return i
}

// commonSuffix returns, for each string, the byte offset at which their shared
// trailing runes begin.
func commonSuffix(a, b string) (endA, endB int) {
	endA, endB = len(a), len(b)
	for endA > 0 && endB > 0 {
		ra, wa := utf8.DecodeLastRuneInString(a[:endA])
		rb, wb := utf8.DecodeLastRuneInString(b[:endB])
		if ra != rb || wa != wb {
			return endA, endB
		}
		endA -= wa
		endB -= wb
	}
	return endA, endB
}
