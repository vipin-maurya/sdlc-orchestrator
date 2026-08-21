// Package diff parses a unified patch into a structure two viewers render.
//
// It is its own package because it is the only part of the web UI that can be
// wrong quietly: a dropped file or a misattributed hunk produces a page that
// looks right and describes a change that did not happen, on the screen where
// a human decides whether to merge. Pure function from bytes to structs — no
// process, no filesystem, no config — so it can be tested to exhaustion and
// fuzzed.
package diff

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Status string

const (
	StatusAdded    Status = "added"
	StatusDeleted  Status = "deleted"
	StatusModified Status = "modified"
	StatusRenamed  Status = "renamed"
	StatusCopied   Status = "copied"
	StatusModeOnly Status = "mode"
)

type Kind int

const (
	KindContext Kind = iota
	KindAdd
	KindDel
)

type File struct {
	OldPath, NewPath string
	Status           Status
	OldMode, NewMode string
	Binary           bool
	Hunks            []Hunk
	Adds, Dels       int
	// Malformed is non-empty when the patch could not be fully reconciled —
	// a hunk header whose counts disagree with its body, most often. Lines are
	// never discarded to make the numbers work; the file renders with a visible
	// warning instead, because a patch the parser half-understood must announce
	// itself rather than look complete.
	Malformed string

	// malformedOver latches once Malformed passes its cap, and
	// malformedDropped counts what was refused after that. Unexported: the cap
	// is an implementation detail of not letting a hostile patch allocate
	// without bound, and Malformed already says so in words when it fires.
	malformedOver    bool
	malformedDropped int
}

type Hunk struct {
	OldStart, OldLines, NewStart, NewLines int
	Section                                string
	Lines                                  []Line
}

type Line struct {
	Kind         Kind
	OldNo, NewNo int // 0 when the line does not exist on that side
	Text         string
	NoNewline    bool
}

// Path is the name to show for the file: the new path, falling back to the old
// one for a deletion.
func (f *File) Path() string {
	if f.NewPath != "" {
		return f.NewPath
	}
	return f.OldPath
}

// Rows is the number of diff lines across every hunk. The page budget (20 000)
// and the collapse threshold (500) are both counted in these.
func (f *File) Rows() int {
	n := 0
	for i := range f.Hunks {
		n += len(f.Hunks[i].Lines)
	}
	return n
}

// ErrCombinedDiff rejects a merge diff rather than misparsing it. This call
// site never produces one; a parser that quietly turned @@@ into plausible
// nonsense would be worse than one that refuses.
var ErrCombinedDiff = errors.New("diff: combined diffs (@@@) are not supported")

// unknownPath is what a file whose headers name no path at all is called. It is
// a stand-in rather than a reason to drop the file: the hunks are still real
// changes, and Malformed says on the page that the parser could not name them.
const unknownPath = "(unknown path)"

// devNull is the marker for "this side of the change does not exist". It is
// kept distinct from the empty string so a `--- /dev/null` header can stop the
// diff --git line from filling the old path back in: Path() promises the new
// path with the old one as a fallback *for a deletion*, and an added file whose
// OldPath were populated from `diff --git a/x b/x` would break that promise.
const devNull = "\x00/dev/null"

// hunkRe accepts both `@@ -12,7 +12,9 @@` and the countless `@@ -1 +1 @@`,
// where an absent count means exactly one line. Everything after the closing
// @@ is git's function-context hint and is kept verbatim.
var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)$`)

// parseState is the machine's whole alphabet.
//
// The loop tracks whether it is inside a hunk because inside one, every line
// carries a prefix byte: a removed line `---foo` arrives as `----foo` and a
// context line `diff --git a/x b/y` arrives with a leading space. Classifying
// by first byte only while lines remain in the hunk, and matching file-header
// patterns only outside one, is the difference between parsing a patch and
// parsing a patch that happens not to contain its own syntax. This is the
// classic unified-diff bug and it fails silently: the file after the false
// header simply disappears from the review.
type parseState int

const (
	stHeader parseState = iota
	stHunk
	stBinaryPayload
)

type parser struct {
	state parseState
	files []*File

	cur *File
	// hunk stays open after its declared counts run out, and is only closed by
	// the next hunk or the next file. A `\ No newline at end of file` marker for
	// the last line of a hunk arrives after those counts are satisfied, and so
	// does an over-long body; both belong to the hunk above them and there is
	// nowhere else to put them.
	hunk *Hunk

	// hdrOld/hdrNew are the paths recovered from the `diff --git` line. They are
	// held back rather than assigned, because the ---/+++ and rename lines are
	// unambiguous and this one is not (spec §8.3 case 11).
	hdrOld, hdrNew string

	sawOldHeader bool // a `--- ` line has been seen for the current file
	sawOldNull   bool // ... and it named /dev/null
	sawNewNull   bool
	sawModeLine  bool // `old mode`/`new mode`, which is what makes a file mode-only

	oldRem, newRem int // lines the open hunk still owes, per side
	oldNo, newNo   int
}

// Parse is the whole parsing API.
//
// It requires git's default a/ b/ path prefixes. A patch produced with
// diff.noprefix or diff.mnemonicPrefix set — both of which live in a user's
// ~/.gitconfig and so vary per machine — has a different `diff --git` header,
// and the paths come out mangled rather than refused. Callers pin the prefixes
// on the command line for exactly this reason; see gitx.DiffPatchSince.
func Parse(patch string) ([]*File, error) {
	p := &parser{}
	for _, line := range splitLines(patch) {
		// A line is offered to at most two states: the body state hands one
		// back exactly once, when a hunk's body ends before its header said it
		// would. Bounding the hand-back here rather than by re-decrementing a
		// loop index is what makes "never loops forever" a property of the
		// shape of this loop instead of an argument about its cases.
		for pass := 0; pass < 2; pass++ {
			consumed, err := p.step(line)
			if err != nil {
				return nil, err
			}
			if consumed {
				break
			}
		}
	}
	p.closeFile()
	return p.files, nil
}

// step feeds one line to the current state. It reports false when the state
// declined the line and the next state should be offered it.
func (p *parser) step(line string) (bool, error) {
	switch p.state {
	case stHunk:
		return p.stepHunk(line), nil
	case stBinaryPayload:
		// The base85 payload of `GIT binary patch` is not diff syntax and is
		// never displayed, so it is skipped wholesale rather than classified.
		// Only the next file header ends it.
		if strings.HasPrefix(line, "diff ") {
			p.state = stHeader
			return false, nil
		}
		return true, nil
	default:
		return true, p.stepHeader(line)
	}
}

// stepHunk classifies by first byte and nothing else. See parseState.
func (p *parser) stepHunk(line string) bool {
	if line == "" {
		// A context line for an empty source line is a single space, but
		// patches that have been through an editor or a mail client arrive with
		// that space stripped. While the hunk still owes lines, an empty line is
		// that context line; treating it as the end of the hunk would drop every
		// line after the first blank one in the file.
		p.addLine(KindContext, "")
		return true
	}
	switch line[0] {
	case ' ':
		p.addLine(KindContext, line[1:])
	case '+':
		p.addLine(KindAdd, line[1:])
	case '-':
		p.addLine(KindDel, line[1:])
	case '\\':
		// "\ No newline at end of file" is a marker, not a line: it consumes no
		// line number and counts toward neither side. It annotates whichever
		// line it follows, which is why it is applied to the last one appended
		// rather than to a side chosen by the parser.
		p.markNoNewline()
	default:
		// The body ended before the header said it would. The line is handed
		// back to the header state, which is where it can be understood.
		p.flag("hunk body is shorter than its header declares")
		p.state = stHeader
		return false
	}
	if p.oldRem <= 0 && p.newRem <= 0 {
		p.state = stHeader
	}
	return true
}

func (p *parser) stepHeader(line string) error {
	switch {
	// Combined diffs are refused before anything else, because every later
	// case would find something plausible to do with them.
	case strings.HasPrefix(line, "@@@"),
		strings.HasPrefix(line, "diff --cc "),
		strings.HasPrefix(line, "diff --combined "):
		return ErrCombinedDiff

	case strings.HasPrefix(line, "diff --git "):
		p.closeFile()
		p.startFile()
		p.hdrOld, p.hdrNew = splitDiffGitPaths(line[len("diff --git "):])

	case strings.HasPrefix(line, "@@"):
		p.openHunk(line)

	case strings.HasPrefix(line, "\\"):
		// The marker for the *last* line of a hunk arrives after that hunk's
		// counts are already satisfied, so it is met here rather than in
		// stepHunk. Both positions have to work: one file's patch ends
		// "-old / \ No newline / +new / \ No newline".
		p.markNoNewline()

	case strings.HasPrefix(line, "--- "):
		// A `--- ` here is a file header, even though after a lying hunk header
		// it could also be a removed line reading `-- foo`. Resolved in favour
		// of the header because misfiling one line is recoverable on screen and
		// swallowing a whole file is not — that is the failure this package
		// exists to prevent.
		if p.cur == nil || p.sawOldHeader || p.hunk != nil {
			p.closeFile()
			p.startFile()
		}
		p.sawOldHeader = true
		if path := headerPath(line[4:]); path == devNull {
			p.sawOldNull = true
		} else {
			p.cur.OldPath = path
		}

	case strings.HasPrefix(line, "+++ "):
		if p.cur == nil {
			p.startFile()
		}
		if path := headerPath(line[4:]); path == devNull {
			p.sawNewNull = true
		} else {
			p.cur.NewPath = path
		}

	case strings.HasPrefix(line, "new file mode "):
		if p.cur != nil {
			p.cur.Status = StatusAdded
			p.cur.NewMode = strings.TrimSpace(line[len("new file mode "):])
		}

	case strings.HasPrefix(line, "deleted file mode "):
		if p.cur != nil {
			p.cur.Status = StatusDeleted
			p.cur.OldMode = strings.TrimSpace(line[len("deleted file mode "):])
		}

	case strings.HasPrefix(line, "old mode "):
		if p.cur != nil {
			p.cur.OldMode = strings.TrimSpace(line[len("old mode "):])
			p.sawModeLine = true
		}

	case strings.HasPrefix(line, "new mode "):
		if p.cur != nil {
			p.cur.NewMode = strings.TrimSpace(line[len("new mode "):])
			p.sawModeLine = true
		}

	case strings.HasPrefix(line, "rename from "):
		if p.cur != nil {
			p.cur.Status = StatusRenamed
			p.cur.OldPath = renamePath(line[len("rename from "):])
		}

	case strings.HasPrefix(line, "rename to "):
		if p.cur != nil {
			p.cur.Status = StatusRenamed
			p.cur.NewPath = renamePath(line[len("rename to "):])
		}

	case strings.HasPrefix(line, "copy from "):
		if p.cur != nil {
			p.cur.Status = StatusCopied
			p.cur.OldPath = renamePath(line[len("copy from "):])
		}

	case strings.HasPrefix(line, "copy to "):
		if p.cur != nil {
			p.cur.Status = StatusCopied
			p.cur.NewPath = renamePath(line[len("copy to "):])
		}

	case strings.HasPrefix(line, "index "):
		// `index abc..def 100644` carries the mode only when it did not change;
		// when it did, `old mode`/`new mode` said so already and must win.
		if f := p.cur; f != nil && f.OldMode == "" && f.NewMode == "" {
			if fields := strings.Fields(line); len(fields) == 3 {
				f.OldMode, f.NewMode = fields[2], fields[2]
			}
		}

	case strings.HasPrefix(line, "Binary files ") && strings.HasSuffix(line, " differ"),
		strings.HasPrefix(line, "Files ") && strings.HasSuffix(line, " differ"):
		if p.cur != nil {
			p.cur.Binary = true
		}

	case line == "GIT binary patch":
		if p.cur != nil {
			p.cur.Binary = true
		}
		p.state = stBinaryPayload

	case p.hunk != nil && line != "" && isBodyByte(line[0]):
		// The body outran its header. Keeping the line and flagging the file is
		// the whole of spec §8.3 case 9: a count is a claim about the patch, and
		// when the claim is wrong the lines are the evidence, not the error.
		p.flag("hunk body is longer than its header declares")
		p.state = stHunk
		p.stepHunk(line)

	default:
		// `similarity index`, `dissimilarity index`, commit-message prose ahead
		// of the first header, and anything else that is not diff syntax.
	}
	return nil
}

func (p *parser) openHunk(line string) {
	m := hunkRe.FindStringSubmatch(line)
	if m == nil {
		// Starts `@@` but is not a hunk header. Outside a hunk there is nothing
		// to classify it as, so it is recorded rather than guessed at.
		if p.cur == nil {
			p.startFile()
		}
		// The line is clipped: it is attacker-shaped text, and an untruncated
		// copy of every junk header is how Malformed grew without bound.
		p.flag("unparsable hunk header: " + clip(line, 60))
		return
	}
	if p.cur == nil {
		p.startFile()
	}
	p.closeHunk()
	// A number too big for an int used to fold to 0 silently, which left a
	// deleted line carrying OldNo == 0 — a value Line's own doc reserves for
	// "this line does not exist on that side". A count that does not fit is a
	// claim the patch makes and the parser cannot honour, so it is flagged
	// rather than quietly rewritten to something that reads as a fact.
	num := func(field, raw string, dflt int) int {
		if raw == "" {
			return dflt
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			p.flag(fmt.Sprintf("hunk header %s %q is not a usable number", field, clip(raw, 24)))
			return dflt
		}
		return n
	}
	h := Hunk{
		OldStart: num("old start", m[1], 0),
		OldLines: num("old count", m[2], 1),
		NewStart: num("new start", m[3], 0),
		NewLines: num("new count", m[4], 1),
		Section:  strings.TrimPrefix(m[5], " "),
	}
	p.hunk = &h
	p.oldRem, p.newRem = h.OldLines, h.NewLines
	p.oldNo, p.newNo = h.OldStart, h.NewStart
	if p.oldRem <= 0 && p.newRem <= 0 {
		p.state = stHeader
	} else {
		p.state = stHunk
	}
}

func (p *parser) addLine(k Kind, text string) {
	if p.hunk == nil {
		return
	}
	l := Line{Kind: k, Text: text}
	switch k {
	case KindContext:
		l.OldNo, l.NewNo = p.oldNo, p.newNo
		p.oldNo++
		p.newNo++
		p.oldRem--
		p.newRem--
	case KindAdd:
		l.NewNo = p.newNo
		p.newNo++
		p.newRem--
		p.cur.Adds++
	case KindDel:
		l.OldNo = p.oldNo
		p.oldNo++
		p.oldRem--
		p.cur.Dels++
	}
	p.hunk.Lines = append(p.hunk.Lines, l)
}

func (p *parser) markNoNewline() {
	if p.hunk == nil || len(p.hunk.Lines) == 0 {
		return
	}
	p.hunk.Lines[len(p.hunk.Lines)-1].NoNewline = true
}

func (p *parser) startFile() {
	p.cur = &File{}
	p.hunk = nil
	p.hdrOld, p.hdrNew = "", ""
	p.sawOldHeader, p.sawOldNull, p.sawNewNull, p.sawModeLine = false, false, false, false
	p.oldRem, p.newRem = 0, 0
}

func (p *parser) closeHunk() {
	if p.hunk == nil {
		return
	}
	// Only a shortfall is reported here. An over-long body was already flagged
	// where it happened, and reporting it again as a negative shortfall would
	// read as a second, different defect.
	if p.oldRem > 0 || p.newRem > 0 {
		p.flag(fmt.Sprintf("hunk @@ -%d,%d +%d,%d @@ is short by %d old and %d new lines",
			p.hunk.OldStart, p.hunk.OldLines, p.hunk.NewStart, p.hunk.NewLines,
			max(p.oldRem, 0), max(p.newRem, 0)))
	}
	p.cur.Hunks = append(p.cur.Hunks, *p.hunk)
	p.hunk = nil
	p.oldRem, p.newRem = 0, 0
}

func (p *parser) closeFile() {
	if p.cur == nil {
		return
	}
	p.closeHunk()
	f := p.cur
	f.sealMalformed()
	if f.OldPath == "" && !p.sawOldNull {
		f.OldPath = p.hdrOld
	}
	if f.NewPath == "" && !p.sawNewNull {
		f.NewPath = p.hdrNew
	}
	if f.Status == "" {
		if p.sawModeLine && len(f.Hunks) == 0 && !f.Binary {
			f.Status = StatusModeOnly
		} else {
			f.Status = StatusModified
		}
	}
	if f.OldPath == "" && f.NewPath == "" {
		f.NewPath = unknownPath
		f.flag("the file header names no path")
	}
	p.files = append(p.files, f)
	p.cur, p.hunk, p.state = nil, nil, stHeader
}

func (p *parser) flag(msg string) {
	if p.cur == nil {
		p.startFile()
	}
	p.cur.flag(msg)
}

// maxMalformed caps the accumulated reconciliation text. The dedup below only
// suppresses a message that repeats verbatim, and two callers build a message
// from the line or the numbers they read, so a patch full of distinct junk
// headers produces a distinct message every time and the Contains scan then
// walks everything already recorded: 268 KB of crafted input took 13s and
// produced 768 KB of Malformed, and the capture cap is 4 MiB. Neither the time
// nor the string is bounded by anything the parser controls, so both are
// bounded here. Real `git diff` output cannot reach this — every line between
// hunks is git's own — but the package ships a fuzzer that can.
const maxMalformed = 1 << 10

// flag records a reconciliation failure. A message that repeats verbatim —
// every line of an over-long hunk body reports the same thing — is recorded
// once, and past maxMalformed the rest are counted rather than kept: the first
// kilobyte says what went wrong, and a reader who needs more than that needs
// the patch, not a longer string.
func (f *File) flag(msg string) {
	if f.malformedOver {
		f.malformedDropped++
		return
	}
	switch {
	case f.Malformed == "":
		f.Malformed = msg
	case strings.Contains(f.Malformed, msg):
		return
	default:
		f.Malformed += "; " + msg
	}
	if len(f.Malformed) > maxMalformed {
		f.malformedOver = true
	}
}

// sealMalformed appends the count of messages dropped by the cap. It runs once
// per file at close, because appending on every drop would be the unbounded
// growth the cap exists to prevent.
func (f *File) sealMalformed() {
	if f.malformedDropped > 0 {
		f.Malformed += fmt.Sprintf("; and %d further problem(s) not recorded", f.malformedDropped)
	}
}

func isBodyByte(c byte) bool { return c == ' ' || c == '+' || c == '-' }

// clip bounds a piece of patch text before it goes into a message. Without it
// the message set is as large as the patch, which is what made Malformed
// unbounded.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// splitLines splits the patch and, only when the patch file itself is CRLF
// terminated, removes one trailing \r from every line.
//
// The test is deliberately whole-file rather than per-line. A line reading
// "+alpha\r" is either an added line from a CRLF file carried in an LF patch or
// an added line from an LF file carried in a CRLF patch, and nothing about that
// line distinguishes the two. The patch as a whole does: if the structural
// lines — `diff --git`, `index`, `@@` — also end in \r then the terminator is
// the patch's, and stripping it is right. If they do not, the \r belongs to the
// file's content and stripping it would render a CRLF-to-LF conversion as two
// identical lines, which reads as a parser bug on exactly the screen where
// nobody can check (spec §8.3 case 12).
func splitLines(patch string) []string {
	if patch == "" {
		return nil
	}
	lines := strings.Split(patch, "\n")
	if n := len(lines); lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if !patchIsCRLF(lines) {
		return lines
	}
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

func patchIsCRLF(lines []string) bool {
	seen := false
	for _, l := range lines {
		if l == "" {
			continue
		}
		if !strings.HasSuffix(l, "\r") {
			return false
		}
		seen = true
	}
	return seen
}

// headerPath turns the text after `--- `, `+++ ` or `diff --git `'s halves
// into a path. Those three carry git's a/ b/ prefix; the rename and copy lines
// do not, and use renamePath instead.
func headerPath(s string) string {
	s = rawHeaderPath(s)
	if s == devNull {
		return devNull
	}
	return stripSrcPrefix(s)
}

// renamePath turns the text after `rename from `, `rename to `, `copy from `
// or `copy to ` into a path. These four name the file directly, with no a/ b/
// prefix, so stripping one takes a real directory off the front: a rename of
// a/foo.txt to a/renamed.txt reported foo.txt, a path that does not exist in
// the repository. A rename with edits hid this, because the later `--- a/...`
// line overwrote OldPath with a correctly-prefixed one; a pure rename or copy
// has no such line, so the wrong path was final and silent.
func renamePath(s string) string {
	return rawHeaderPath(s)
}

// rawHeaderPath is the part every header path shares: the TAB delimiter and
// git's C quoting, with /dev/null mapped to the sentinel.
func rawHeaderPath(s string) string {
	// git appends a TAB after a path containing a space, so that a reader can
	// tell where the name stops. A quoted path has its tabs escaped, so an
	// unescaped one is always this separator and never part of the name.
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	// No TrimRight of spaces here. `foo ` is a legal POSIX filename that git
	// does not quote, and trimming turned it into `foo`. The TAB above already
	// ends a name that contains spaces, and splitDiffGitPaths cuts exactly at
	// the separator, so there is nothing left for a trim to do but damage.
	s = unquotePath(s)
	if s == "/dev/null" {
		return devNull
	}
	return s
}

// unquotePath undoes core.quotePath's C quoting. Go's string-literal syntax is
// a superset of it — \nnn octal escapes included, which is how a UTF-8 path
// arrives — so strconv does the work. A quoting scheme it cannot read is
// returned verbatim: a path shown with its escapes still in it is legible, and
// an error here would lose the file.
func unquotePath(s string) string {
	if len(s) < 2 || s[0] != '"' {
		return s
	}
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return s
}

func stripSrcPrefix(s string) string {
	if len(s) >= 2 && (s[0] == 'a' || s[0] == 'b') && s[1] == '/' {
		return s[2:]
	}
	return s
}

// splitDiffGitPaths recovers both paths from the text after `diff --git `.
//
// This is the ambiguous source and the last resort: `--- `/`+++ ` and
// `rename from`/`rename to` name their paths unambiguously and are preferred
// wherever they exist, which leaves only mode-only and pure-rename chunks
// coming through here. The separator is a space, and a path may contain
// spaces, so `a/sub b/script.sh b/sub b/script.sh` has three candidate split
// points and only one right answer.
//
// Resolution: prefer the split that makes the two paths equal, which is exactly
// what a mode-only chunk has and what the last-occurrence rule gets wrong; fall
// back to the last occurrence otherwise. The residual risk, recorded rather
// than pretended away: a chunk whose two paths genuinely differ, contains ` b/`
// inside a path, and carries no rename lines would still split wrong. git does
// not emit one — differing paths always come with `rename from`/`rename to` or
// `copy from`/`copy to`.
func splitDiffGitPaths(rest string) (oldPath, newPath string) {
	if strings.HasPrefix(rest, `"`) {
		if i := closingQuote(rest); i > 0 {
			return headerPath(rest[:i]), headerPath(strings.TrimSpace(rest[i:]))
		}
	}
	last := -1
	eq := -1
	for i := 0; i < len(rest); i++ {
		if !isSplitPoint(rest, i) {
			continue
		}
		last = i
		if o, n := headerPath(rest[:i]), headerPath(rest[i+1:]); o == n && o != "" {
			eq = i
		}
	}
	if eq >= 0 {
		return headerPath(rest[:eq]), headerPath(rest[eq+1:])
	}
	if last >= 0 {
		return headerPath(rest[:last]), headerPath(rest[last+1:])
	}
	// No separator at all. Naming both sides after the whole remainder beats
	// naming neither: Path() must not come back empty.
	s := headerPath(rest)
	return s, s
}

func isSplitPoint(s string, i int) bool {
	if s[i] != ' ' {
		return false
	}
	r := s[i+1:]
	return strings.HasPrefix(r, "b/") || strings.HasPrefix(r, `"b/`)
}

func closingQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return -1
}
