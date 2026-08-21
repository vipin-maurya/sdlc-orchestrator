# Implementation plan — local server and web UI

Branch: `main-anueyo`. Source spec: `scratchpad/server-spec.md` v1.0, as amended by A1/A2/A3.
Go 1.26, stdlib only, **zero new `go.mod` entries**.

## 0. How to read this plan

Work is partitioned by **exclusive file ownership**. Ownership means *exclusive write*; reading any file is always free. No file appears under two owners in the same wave. Everything one owner calls from another owner's code is pinned in §2 as verbatim Go — code against §2, never against a sibling's in-flight edit.

A wave closes when every owner in it has landed and `gofmt -l . && go vet ./... && go test ./... -race` is green.

### Amendments in force (override the spec)

- **A1** — patch truncation is fixed properly in `execx`/`gitx`, not inferred from `len(patch) >= 4<<20`. Spec §13.3 is void. W0.2.
- **A2** — the gate page renders live and falls back to the persisted `<gate>.md`/`<gate>.diff`, labelling on screen which one the reader has. Closes spec §13 open question 10.
- **A3** — both diff views ship, with a toggle. Not descopable.
- **A4** — `Server` owns its `*http.Server` and exposes `Serve(ln net.Listener) error` alongside `Start`/`Shutdown`. §2.7 pinned nothing that binds, and §5.4's `ReadHeaderTimeout: 10s` and `WriteTimeout: 0` are properties of the `http.Server` — leaving its construction to W2-M would put the timeout that would silently kill SSE in the file least likely to be reviewed for it. `Start()` still means "start the hub only", so `httptest` tests get stream events without binding. The caller must pass a listener opened on `config.NormalizeListen`'s **return value**, never the raw config string. W2-G.
- **A5** — `/api/jobs/{id}.json` is registered as `GET /api/jobs/{idjson}`, with the `.json` suffix required and stripped and `r.SetPathValue("id", …)` called so handlers read `{id}` as on every other job route. `ServeMux` wildcards must span a whole segment: registering `{id}.json` **panics** with `bad wildcard segment (must end with '}')`. The URL spec §5.3 pins is served exactly; only the pattern spelling differs. W2-G.
- **A6** — the root route is `GET /{$}`, not `GET /`. A bare `/` is a subtree match that would swallow every unregistered path and answer 303 where AC-8 requires 404. W2-G.

### Restated acceptance criteria

- **AC-4 (restated).** `git diff --stat internal/engine/` shows exactly one file, `internal/engine/jobctx.go`, and its change is confined to the `DiffPatchSince` call inside `stageDiffPatch`: one call line altered plus a three-line comment. No state handler, transition, counter, event, or timeout is touched, and `go test ./internal/engine/... -race` passes with `internal/engine/*_test.go` unmodified. The original "empty diffstat" form of AC-4 is unachievable under A1.
- **AC-17 (restated).** `grep -rn "template.HTML\|template.JS\|template.URL" internal/server` returns **zero** hits. `html/template` escapes a plain string correctly in a `href`/`src` URL context, so the asset-path helper needs no escape hatch. `TestHouseRules` asserts zero.
- All other AC-1…AC-30 stand as written.

---

## 1. Wave map

| Wave | Owner | Area | Blocked by | ~lines |
|---|---|---|---|---|
| W0 | **A** | `internal/review` golden test + fixtures | — | 330 |
| W0 | **B** | execx/gitx truncation fix (A1) | — | 110 |
| W1 | **C** | `internal/diff` parser | §2.3 | 800 |
| W1 | **D** | `internal/diff` side-by-side + intra-line | §2.3 | 380 |
| W1 | **E** | `internal/review` block-model refactor | W0-A, W0-B | 900 |
| W1 | **F** | `internal/jobs` + `internal/config` server block | §2.4, §2.5 | 300 |
| W2.0 | **G** | `internal/server` skeleton: struct, routes, middleware, safety, assets, fixtures, stubs | W1 all | 950 |
| W2.1 | **H** | pages.go + read-only page templates | W2.0 | 830 |
| W2.1 | **I** | actions.go + `_forms.html` | W2.0 | 620 |
| W2.1 | **J** | render.go + gate.html (review.Doc → HTML) | W2.0, W1-E | 620 |
| W2.1 | **K** | diffpage.go + diff templates + diff.css | W2.0, W1-C/D | 780 |
| W2.1 | **L** | live.go + app.js (SSE, snapshots, staleness, follow) | W2.0 | 760 |
| W2.1 | **M** | `internal/cli`: `serve` + `cmdSubmit` delegation | W2.0, W1-F | 260 |
| W3 | **N** | docs: SPEC, running, README | W2 all | 260 |

---

## 2. Contracts (pinned; implement and consume verbatim)

### 2.1 `internal/execx` — owner B

```go
type Result struct {
	ExitCode int
	Duration time.Duration
	TimedOut bool
	LogPath  string
	// Truncated reports that maxBytes stopped the returned string short of the
	// command's real output. limitedWriter reports a full write after it has
	// stopped copying — it has to, or os/exec would tear the command down with
	// a short-write error — so without this flag a caller cannot distinguish a
	// 4 MiB patch from the head of a 40 MiB one. A reviewer approving a merge
	// from a patch that was silently clipped is deciding on a change they have
	// not seen.
	Truncated bool
}

type limitedWriter struct {
	w    io.Writer
	n    int64
	over bool // set once bytes have been dropped
}

// dropped is nil-safe so RunCapture can ask about a limiter it never built
// (maxBytes == 0, or stderr not separated) without a nil check at each site.
func (l *limitedWriter) dropped() bool { return l != nil && l.over }
```

`limitedWriter.Write` sets `l.over = true` on both the `l.n <= 0` early return and the `len(p) > l.n` clamp. `RunCapture` keeps concrete `*limitedWriter` handles for the stdout limiter and (when `StderrSeparate`) the stderr limiter, and builds `res := Result{..., Truncated: lw.dropped() || elw.dropped()}`. Both count because both feed the string returned on the error path.

`Run` never sets `Truncated` (it has no cap).

### 2.2 `internal/gitx` — owner B

```go
// DiffPatchSince returns the full unified diff between sha and the worktree,
// and whether the 4 MiB capture cap cut it short. The flag is returned rather
// than left for the caller to infer from the length: a patch that is exactly
// the cap and a patch that overran it are the same string.
func (r Repo) DiffPatchSince(ctx context.Context, dir, sha string) (patch string, truncated bool, err error)
```

Exactly two call sites change.

`internal/engine/jobctx.go` `stageDiffPatch` — the entire engine delta:

```go
	// The truncation flag is dropped here on purpose: the reviewing agent's
	// context window binds long before 4 MiB, and changing what the engine
	// stages for a giant patch is not this change. The gate document, which a
	// human reads and acts on, does say so.
	patch, _, err := c.repo.DiffPatchSince(ctx, c.job.WorktreePath, c.job.Counters.BaseSHA)
```

`internal/review/review.go` `renderDiff` — sets `d.Diff` and the new `d.DiffTruncated`, and emits one markdown line **before** the `FullDiff` branch so the warning precedes the content it warns about:

```go
	patch, truncated, err := repo.DiffPatchSince(dctx, o.Job.WorktreePath, base)
	if err == nil {
		d.Diff, d.DiffTruncated = patch, truncated
		if truncated {
			fmt.Fprintf(b, "_The patch exceeds the 4 MiB capture limit; what follows is its head. `git -C %s diff %s` for all of it._\n\n",
				o.Job.WorktreePath, short(base))
		}
		// ... existing FullDiff branch unchanged
	}
```

`review.Doc` gains:

```go
	// DiffTruncated reports that Diff is the head of a larger patch. Both
	// surfaces state it as a fact rather than guessing from len(Diff).
	DiffTruncated bool
```

### 2.3 `internal/diff` — owners C and D

Owner C defines every type below plus `Parse`. Owner D defines `Row` and `Split` only. Both files are `package diff`.

```go
// Package diff parses a unified patch into a structure two viewers render.
//
// It is its own package because it is the only part of the web UI that can be
// wrong quietly: a dropped file or a misattributed hunk produces a page that
// looks right and describes a change that did not happen, on the screen where
// a human decides whether to merge. Pure function from bytes to structs — no
// process, no filesystem, no config — so it can be tested to exhaustion and
// fuzzed.
package diff

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
func (f *File) Path() string

// Rows is the number of diff lines across every hunk. The page budget (20 000)
// and the collapse threshold (500) are both counted in these.
func (f *File) Rows() int

// ErrCombinedDiff rejects a merge diff rather than misparsing it. This call
// site never produces one; a parser that quietly turned @@@ into plausible
// nonsense would be worse than one that refuses.
var ErrCombinedDiff = errors.New("diff: combined diffs (@@@) are not supported")

// Parse is the whole parsing API.
func Parse(patch string) ([]*File, error)
```

Owner D:

```go
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

// Split pairs one hunk's lines for side-by-side display.
func Split(h Hunk) []Row
```

Intra-line guard, normative: if the marked span exceeds 70% of the longer of the two lines, drop both marks (set all four offsets to 0). Two unrelated lines paired by position would otherwise light up end to end and read as a rewrite.

### 2.4 `internal/review` block model — owner E

```go
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
	Findings []artifact.Finding  // already ordered; each surface formats its own badge
	Steps    []artifact.PlanStep
	Actions  []Action
	Commands []string // BlockActions: the CLI equivalents, one per line
}
```

`Doc` gains:

```go
	// Blocks is the document as a tree. Body is rendered from it rather than
	// built alongside it, so the browser and the terminal cannot come to
	// describe the same gate differently.
	Blocks []Block
```

The renderer, and the one rule that makes byte-fidelity reachable:

```go
// renderMarkdown concatenates each block's own markdown, trailing blank line
// included. Blocks are deliberately NOT joined with a separator: the document's
// spacing is irregular by construction — a facts list runs line-by-line with a
// single blank line after it, a fenced block ends with exactly one newline, a
// findings list has none between entries — and the golden test holds all of it
// to the byte. Each block therefore owns the whitespace that follows it.
func renderMarkdown(blocks []Block) string
```

`Render` builds `d.Blocks` and sets `d.Body = renderMarkdown(d.Blocks)`. `Write` appends a block and re-renders instead of concatenating onto `Body`; the appended block is

```go
Block{Kind: BlockParagraph, Spans: []Span{
	{Kind: SpanText, Text: "\nFull patch: "},
	{Kind: SpanPath, Text: diffPath},
}}
```

and must render to exactly "\nFull patch: `<path>`\n" — the bytes the current `doc.Body += fmt.Sprintf(...)` produces. `Render`'s and `Write`'s signatures do not change.

### 2.5 `internal/jobs` — owner F

```go
// Package jobs owns job creation. It exists so `sdlc submit` and the web UI
// cannot disagree about which targets exist, what makes a title valid, how a
// branch name is derived or what an empty body defaults to. It depends on
// config and store only, so both callers can import it and the engine's state
// machine stays untouched. If this looks small enough to inline back into cli,
// that is the inlining this package exists to prevent.
package jobs

type SubmitRequest struct{ Target, Title, Body string }

// ErrNoTitle distinguishes "you did not say what this is" from a configuration
// error, because the CLI answers the first with a usage exit (2) and the second
// with a failure exit (1). Callers test with errors.Is.
var ErrNoTitle = errors.New("a title is required")

// Submit validates the request against cfg, allocates the id, derives the
// branch and worktree paths, and creates the job row.
func Submit(cfg *config.Config, st *store.Store, req SubmitRequest) (*store.Job, error)

// TitleAndBody splits a pasted issue file the way `sdlc submit --file` does:
// when title is empty the first line, stripped of a leading '#', becomes the
// title and the remainder becomes the body.
func TitleAndBody(title, content string) (title2, body string)
```

`Submit` semantics, pinned to preserve today's CLI behaviour exactly: empty or unknown target → error wrapping `cfg.Target`'s (exit 1); empty title after `TitleAndBody` → `ErrNoTitle` (exit 2); empty body → set to the title. After W2-M, `jobs.Submit` is the only non-test caller of `store.CreateJob` (AC-29).

### 2.6 `internal/config` — owner F

```go
type Server struct {
	Listen string `yaml:"listen"`
}
```
added to `Config` as a field `Server` with the yaml tag `server`, defaulted in `Default()` to `Server{Listen: "127.0.0.1:7777"}`.

```go
// ValidateListen reports why addr must not be bound. It is exported because
// `sdlc serve --addr` takes the same rule as the config key, and a second copy
// of "which hosts count as loopback" is how the flag and the file would drift.
//
// allowPortZero is true only for --addr: an ephemeral port the operator cannot
// predict is not a useful thing to write in a config file, but it is exactly
// how a test binds without racing for a fixed one.
func ValidateListen(addr string, allowPortZero bool) error
```

Rule: `net.SplitHostPort` must succeed; the host must be `localhost` verbatim or parse via `net.ParseIP` with `IsLoopback()`; an empty host is rejected; the port must be 1–65535 (0 allowed only when `allowPortZero`). The non-loopback message must contain the substrings **`loopback`** and **`ssh -L`** — `TestListenMustBeLoopback` greps for both (AC-7). `Config.Validate` calls `ValidateListen(c.Server.Listen, false)` in the existing `fail(...)` style.

`sdlc.example.yaml` ships the block **commented out**, matching how `database:` is presented, because strict decoding means a config carrying `server:` will not load on an older binary:

```yaml
# server:
#   listen: 127.0.0.1:7777      # sdlc serve binds here. Loopback only: there
#                               # is no authentication, so a routable address
#                               # would publish an approve button on the
#                               # network. Remote use goes through
#                               # ssh -L 7777:127.0.0.1:7777 host
```

### 2.7 `internal/server` shared surface — owner G defines, everyone consumes

```go
type Server struct {
	cfg     *config.Config
	st      *store.Store
	log     *log.Logger // never nil
	verbose bool

	// tpl holds one parsed set per page. html/template keeps one namespace per
	// set, so several pages each defining "content" cannot share a set —
	// parsing them together silently leaves whichever file parsed last.
	tpl map[string]*template.Template

	assets map[string]string // "app.css" -> "/static/app.<hash>.css"

	hub *hub // live.go

	mu    sync.Mutex // guards diffs
	diffs []diffEntry
}

func New(cfg *config.Config, st *store.Store, lg *log.Logger, verbose bool) (*Server, error)
func (s *Server) Handler() http.Handler
func (s *Server) Start()
func (s *Server) Shutdown(ctx context.Context) error
```

Shared helpers, all defined in `server.go`/`safety.go` by G, all called by H/I/J/K/L:

```go
// render executes a page into a buffer before writing a byte of it: a template
// that fails halfway would otherwise leave a 200 with half a page on it, which
// looks like a rendering quirk rather than the error it is.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, body any)

// fail renders error.html with a status. Never used for a decision route:
// those answer 303 (AC-15).
func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, msg string)

// job resolves {id} or answers 404 and reports false.
func (s *Server) job(w http.ResponseWriter, r *http.Request) (*store.Job, bool)

// target resolves the job's target or answers 500 and reports false.
func (s *Server) target(w http.ResponseWriter, r *http.Request, j *store.Job) (config.Target, bool)

func (s *Server) page(r *http.Request, title, nav string, body any) pageData

type pageData struct {
	Title    string
	Nav      string // "jobs" | "submit" | "config"
	CSRF     string
	EngineUp bool // orchestrator.lock_file exists
	Assets   map[string]string
	Now      time.Time
	Body     any
}

// safeName resolves name to a file inside dir by MEMBERSHIP in os.ReadDir(dir),
// never by cleaning and prefix-comparing. Prefix comparison after
// filepath.Clean is defeatable on Windows by drive-relative names (C:file),
// alternate data streams (name:$DATA) and 8.3 short names; a name that is not
// in the directory listing is not in the directory, on every platform.
func safeName(dir, name string) (string, error)

func (s *Server) requireCSRF(next http.HandlerFunc) http.HandlerFunc
func (s *Server) requireFreshState(next freshHandler) http.HandlerFunc

// freshHandler runs only after the job has been re-read and its state matched
// against the form's `state` field.
type freshHandler func(w http.ResponseWriter, r *http.Request, j *store.Job)
```

Template functions registered by G, usable from every template:

```go
"asset":   func(name string) string          // "app.css" -> "/static/app.<hash>.css"
"rfc3339": func(t time.Time) string          // UTC, "" for zero
"short":   func(sha string) string           // first 12
"gateFor": review.GateFor
"sevRank": func(s string) int                // artifact.SeverityRank[s]
"base":    func(p string) string             // path.Base for display
"add":     func(a, b int) int
```

DOM contract that L's `app.js` depends on and H/I/J/K's templates must emit:

| Hook | Where | Meaning |
|---|---|---|
| `<span id="live-pill" class="pill">` | base.html (G) | staleness indicator |
| `<tr data-job="JOB-1">` with `<td data-field="state\|since\|gate\|title">` | jobs.html (H) | in-place row replacement |
| `<time datetime="RFC3339">` | everywhere | browser-side relative time |
| `<form data-gate-state="AWAITING_MERGE_APPROVAL">` | `_forms.html` (I) | disable buttons when state moves or the stream goes stale |
| `<pre id="logtail" data-url="…" data-offset="1234">` + `<input id="follow" type="checkbox">` | log.html (H) | follow polling |
| `<a class="diff-view" href="?view=split">` + `<form method="post" action="/prefs/diff-view">` | diff.html (K) | view toggle |

Snapshot JSON, produced by L, consumed by `app.js` — field names are the contract:

```go
type jobSnapshot struct {
	ID          string         `json:"id"`
	Target      string         `json:"target"`
	State       string         `json:"state"`
	Gate        string         `json:"gate"`
	Title       string         `json:"title"`
	Since       string         `json:"since"`        // RFC3339 UTC, StateEnteredAt
	UpdatedAt   string         `json:"updated_at"`   // RFC3339 UTC
	HoldReason  string         `json:"hold_reason,omitempty"`
	ResumeAfter string         `json:"resume_after,omitempty"`
	Counters    store.Counters `json:"counters"`
	Progress    string         `json:"progress,omitempty"`
	ProgressAt  string         `json:"progress_at,omitempty"`
}

type snapshot struct {
	Jobs []jobSnapshot `json:"jobs"`
	Now  string        `json:"now"`
}
```

Gate-document source, the A2 contract:

```go
type docSource string

const (
	docLive      docSource = "live"
	docPersisted docSource = "persisted"
)

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
func (s *Server) gateDoc(ctx context.Context, j *store.Job, gate string) (*review.Doc, docSource, error)
```

Parsed-diff cache (G defines the type and the mutex; K is its only user):

```go
type diffEntry struct {
	key   string // jobID + "\x00" + HeadSHA + "\x00" + BaseSHA + "\x00" + strconv.Itoa(len(patch))
	patch string
	files []*diff.File
}

// diffFor returns the parsed patch, parsing at most once per (job, head, base,
// length). The head sha is in the key so a cached diff can never outlive the
// commit it described: a stale diff at a merge gate is the one cache bug that
// would actually cost something. At most 4 entries, least-recently-used first
// out. Nothing else in this server is cached.
func (s *Server) diffFor(j *store.Job, patch string) ([]*diff.File, error)
```

---

## 3. Steps

### W0 — foundations

Both owners run in parallel; their files are disjoint. The wave closes only when both have landed and the goldens pass with B's change in the tree.

#### W0-A — capture the `internal/review` golden test

**Owns exclusively:** `internal/review/golden_test.go`, `internal/review/testdata/golden/**`.
**Must not touch:** `internal/review/review.go` (B holds it this wave, E holds it next), any other package.
**Blocked by:** nothing. **Size:** ~180 lines of Go + ~150 lines of JSON fixtures + 5 golden documents.

This is spec §12.3's `TestMarkdownGoldenUnchanged` and it must land **before** a line of the refactor. It is the only thing that makes E's rewrite safe.

`Render`'s output is time-, path- and timezone-dependent, and every one of those has to be pinned or the golden is flaky rather than protective. The resolutions are normative:

- **Time since state entry.** `humanSince` reads `time.Since`. The fixture sets `StateEnteredAt: time.Now().Add(-90 * time.Minute)`, which renders `1h` with an hour of slack either side.
- **Event timestamps** (hold gate only). `renderHold` formats `e.CreatedAt.Local()`. The fixture builds each event's `CreatedAt` with `time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)` so the wall-clock rendering is `03:04:05` in every zone.
- **Paths.** `DataDir` is the literal relative path `testdata/golden`; `WorktreePath` is `testdata/golden/worktree-gone`, which does not exist, so `renderDiff` takes its "worktree is gone" branch deterministically without needing git. Because `filepath.Join` uses `\` on Windows, the comparison applies exactly one substitution to both sides — `strings.ReplaceAll(s, "\\", "/")` — with a comment saying why. That is the only normalization permitted.
- **Artifact fixtures** live at `testdata/golden/jobs/JOB-1/artifacts/` so `artifact.ArtifactsDir("testdata/golden", "JOB-1")` finds them: `spec.json`, `plan.json`, `design_review.r1.json`, `code_review.r1.json`, `code_review.r2.json`, `code_review.r2.verified.json`, `implementation.json`, `final_review.json`, `final_review.verified.json`. The extra rounds are not decoration — they make the golden also pin `latestReview`'s and `preferVerified`'s choices.
- At least one finding carries a `Verification`, one carries no `File`, one carries no `Recommendation`, and one description contains a `<script>` tag and a backtick. That last one is reused verbatim by W2-J's escaping test.
- Golden documents: `testdata/golden/expected/{spec,code,merge,release,hold}.md`.

```go
// -update-golden rewrites the expected documents. Reach for it only when a
// deliberate change to the document's wording is what you are landing. If it
// is the block-model refactor that made the goldens differ, the difference IS
// the bug: the whole point of Option B (spec §7.2) is that the terminal's words
// do not move, and a regenerated golden hides exactly the drift this file is
// here to catch.
var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden/expected")
```

On mismatch the failure message repeats that sentence, so the next reader meets it before they meet the flag.

Also in this file: `TestFullPatchLineIsExactBytes`, asserting that `Write`'s appended patch line is exactly "\nFull patch: `<path>`\n" given a `Doc` with a non-empty `Diff`. E cannot satisfy §2.4's `Write` contract without it, and it needs no git.

**Verified by:** `go test ./internal/review/ -run 'TestMarkdownGoldenUnchanged|TestFullPatchLineIsExactBytes' -race`, then the same command run twice ten minutes apart (the `humanSince` slack), and under `TZ=Asia/Kolkata` and `TZ=UTC`.

#### W0-B — the truncation fix (A1)

**Owns exclusively:** `internal/execx/execx.go`, `internal/execx/execx_test.go`, `internal/gitx/gitx.go`, `internal/gitx/gitx_test.go`, `internal/engine/jobctx.go`, `internal/review/review.go`.
**Must not touch:** anything else. In particular no other file under `internal/engine`, and none of `review.go`'s rendering functions beyond the five lines §2.2 specifies inside `renderDiff` and the one field on `Doc`.
**Blocked by:** nothing. **Size:** ~40 lines of production change, ~70 of test.

Implement §2.1 and §2.2 exactly. This is its own commit: *"Return the truncation flag from execx and gitx"*.

**Verified by:**
- `TestRunCaptureReportsTruncation` — a command emitting more than `maxBytes` gives `Truncated == true` and a string of exactly `maxBytes`; the same command under a generous cap gives `false`; `maxBytes == 0` gives `false`.
- `TestDiffPatchSinceReportsTruncation` — in a temp git repo, commit a file larger than 4 MiB and assert `truncated == true`; a small diff gives `false`.
- `go test ./internal/engine/... -race` with the engine's own test files unmodified — this is AC-4's live half.
- `go build ./...` proves the only two call sites were the two named.

> **CHECKPOINT 1 — end of W0.** Do not start W1 until: goldens pass byte-for-byte with B's change in the tree, run twice under two timezones; `git diff --stat internal/engine/` names only `jobctx.go`; `git diff --exit-code go.mod go.sum` is clean. If the goldens cannot be made deterministic, stop.

### W1 — parallel package builds

#### W1-C — `internal/diff` parser

**Owns exclusively:** `internal/diff/diff.go`, `internal/diff/diff_test.go`, `internal/diff/fuzz_test.go`, `internal/diff/testdata/**`.
**Must not touch:** `internal/diff/split.go` (D owns it), anything outside the package.
**Blocked by:** §2.3. **Size:** ~340 production, ~450 test, ~15 fixtures.

The loop is an **explicit state machine**, not a chain of `strings.HasPrefix` checks:

```go
// The loop tracks whether it is inside a hunk because inside one, every line
// carries a prefix byte: a removed line `---foo` arrives as `----foo` and a
// context line `diff --git a/x b/y` arrives with a leading space. Classifying
// by first byte only while lines remain in the hunk, and matching file-header
// patterns only outside one, is the difference between parsing a patch and
// parsing a patch that happens not to contain its own syntax. This is the
// classic unified-diff bug and it fails silently: the file after the false
// header simply disappears from the review.
```

All fourteen cases in spec §8.3 get their own fixture and their own test. Each asserts file count, paths, `Status`, per-file `Adds`/`Dels`, hunk headers, and the exact `[]Line` sequence for at least one hunk. Case 11's residual risk (a `diff --git` header for a path containing ` b/`, reachable only for mode-only and pure-rename chunks) is handled by splitting on the **last** occurrence, with the limitation stated in a comment at that line.

**Fixture generation, pinned.** Fixtures are `.patch` files generated once with real git and committed. Beside each, commit `<name>.numstat` — the output of `git diff --numstat` for the same change, captured at the same moment. AC-25's independent oracle then needs no git at test time. The exact `git` commands that produced each fixture go in a comment block at the top of `diff_test.go`. One fixture, `big.patch` (~3000 lines across 40 files, under 200 KB), backs the AC-24 benchmark.

Named tests: `TestParseNewFile`, `TestParseDeletion`, `TestParsePureRenameHasNoHunks`, `TestParseRenameWithEdits`, `TestParseBinary`, `TestParseModeOnly`, `TestNoNewlineMarkerDoesNotCountAsALine` (both positions, and twice in one hunk), `TestHunkHeaderWithoutCounts`, `TestMalformedHunkIsFlaggedNotDropped`, `TestContentThatLooksLikeAHeader`, `TestQuotedPaths`, `TestCRLF`, `TestCombinedDiffIsRejected`, `TestEmptyAndTruncatedPatch`, `TestCountsMatchNumstat`, `FuzzParse`.

`FuzzParse` stays in the default seed-corpus run: never panics, never loops, every returned `File` has a non-empty `Path()`.

#### W1-D — side-by-side pairing and intra-line marks

**Owns exclusively:** `internal/diff/split.go`, `internal/diff/split_test.go`, `internal/diff/testdata/split/**`.
**Must not touch:** `diff.go` or C's testdata.
**Blocked by:** §2.3 only — D codes against the pinned types, not against C's file. **Size:** ~170 production, ~210 test.

Implement `Split` per spec §8.4: context lines emit a paired row; a maximal run of `-` followed by a maximal run of `+` is one change block paired index-by-index with filler for the shorter side; unmatched deletions and additions pair against filler. Positional pairing, not LCS — say so in a comment and say why.

Intra-line marks: common rune prefix and common rune suffix, reported as byte offsets per §2.3, with the 70% guard.

`TestSplitMatchesUnified` is the invariant that stops the two views from ever telling different stories: for every fixture in **both** `testdata/` and `testdata/split/`, the ordered sequence of non-filler add/del lines in the split view equals the sequence in the unified view. D writes its own minimal fixtures under `testdata/split/` so it can develop before C's corpus exists.

Named tests: `TestSplitPairsChangeBlocksPositionally`, `TestSplitFillsTheShorterSide`, `TestIntraLineMarksTheChangedMiddle`, `TestIntraLineMarksAreDroppedWhenTheLinesAreUnrelated`, `TestIntraLineOffsetsNeverSplitARune`, `TestSplitMatchesUnified`.

#### W1-E — the `internal/review` block-model refactor

**Owns exclusively:** `internal/review/review.go`, `internal/review/blocks.go`, `internal/review/blocks_test.go`.
**Must not touch:** `internal/review/golden_test.go` or `testdata/golden/**` — those are the contract being verified and E may not edit them, including with `-update-golden`.
**Blocked by:** W0-A and W0-B both landed. **Size:** `blocks.go` ~230, `review.go` rewritten in place, tests ~150.

Mapping from today's output to blocks, pinned:

| Today | Block |
|---|---|
| `# JOB-1 — title` | `BlockHeading{Level: 1}` |
| the bold gate title | `BlockParagraph` with one `SpanStrong` |
| the state/branch/worktree/artifacts list | one `BlockFacts` with four `Fact`s; state's `Note` is `waiting 1h` |
| `## Issue` + blockquote | `BlockHeading{Level:2}` + `BlockQuote{Truncated: …}` |
| `bullets(...)` | `BlockParagraph` (the bold heading line) + `BlockBullets` |
| plan steps | `BlockSteps` |
| findings | `BlockFindings` |
| the diff-stat and inline-patch fences | `BlockCode{Lang:""}` / `BlockCode{Lang:"diff"}` |
| every italic aside (`_unavailable_`, `_(no body)_`, `_worktree … is gone_`, `_source: …_`, `_Re-run with --diff_`, W0-B's truncation line) | `BlockParagraph` with `SpanEmphasis` |
| `## What happens next` + arrows + the command fence | `BlockActions` (`Actions` + `Commands`) |

Tests E owns: `TestBodyIsRenderedFromBlocks`, `TestEveryBlockKindHasAMarkdownCase`, `TestEverySpanKindHasAMarkdownCase`.

**Verified by:** `go test ./internal/review/ -race` — specifically the untouched `TestMarkdownGoldenUnchanged` passing byte-for-byte. Then `go test ./internal/cli/ -race`.

> **CHECKPOINT 2 — after W1-E.** If the goldens cannot be made to pass byte-for-byte, **stop and reconsider Option A**. Do not update the golden files. A wording change that slipped in here is a change to the document a human uses to decide a merge.

#### W1-F — `internal/jobs` and the config `server:` block

**Owns exclusively:** `internal/jobs/submit.go`, `internal/jobs/submit_test.go`, `internal/config/config.go`, `internal/config/config_test.go`, `internal/config/example_config_test.go`, `sdlc.example.yaml`.
**Must not touch:** `internal/cli/**` — the `cmdSubmit` delegation is W2-M's.
**Blocked by:** §2.5, §2.6. **Size:** ~140 production, ~150 test, ~8 lines of YAML.

`internal/jobs` lands unused this wave; that is expected.

Named tests: `TestSubmitDerivesBranchAndWorktree`, `TestSubmitRejectsUnknownTarget`, `TestSubmitWithoutATitleIsErrNoTitle` (`errors.Is`), `TestSubmitDefaultsBodyToTitle`, `TestTitleAndBodySplitsAPastedIssue`, `TestListenMustBeLoopback` (table: `127.0.0.1:7777` ok, `localhost:7777` ok, `[::1]:7777` ok, `0.0.0.0:7777` rejected naming both `loopback` and `ssh -L`, bare `:7777` rejected, `example.com:7777` rejected, port 0 rejected in config and accepted from `--addr`, port 99999 rejected), `TestDefaultServerListen`. `TestExampleConfigDecodes` must keep passing unchanged.

### W2.0 — the server skeleton (single owner, gate for all of W2.1)

#### W2-G — foundation, safety, assets, fixtures

**Owns exclusively:** `internal/server/server.go`, `safety.go`, `assets.go`, `templates/base.html`, `templates/error.html`, `static/app.css`, `fixture_test.go`, `server_test.go`, `safety_test.go`, `houserules_test.go`. Plus, **in the skeleton commit only**, the initial creation of every file the W2.1 owners will fill.
**Blocked by:** all of W1. **Size:** ~950.

Go's compiler serializes work inside a package: `server.go`'s route table cannot reference a handler that does not exist yet. G resolves that once, with a **skeleton commit**:

1. `server.go` complete: the `Server` struct, `New`, `Handler()` with **every** route from spec §5 registered, the middleware chain `recoverPanic -> checkHost -> withCSRFCookie -> logRequest`, `Start`, `Shutdown`, all shared helpers from §2.7, and the template func map.
2. `safety.go` complete: `checkHost`, the CSRF cookie/double-submit pair, `requireCSRF`, `requireFreshState`, `safeName`.
3. `assets.go` complete: the embed directives, the content-hash map, the `/static/{path...}` handler.
4. Every W2.1-owned `.go` file created containing only its method stubs calling `stub(w, "W2-H")`.
5. Every W2.1-owned `.html` file created containing an empty `content` definition, so `New`'s template map parses.
6. `TestNoStubsRemain` — walks the package source for the `stub(` marker and fails if any survives.

**After that commit G edits none of those files again.** No W2.1 owner edits `server.go`. That single rule is what keeps six agents out of each other's way.

Comments that must be present, each stating what goes wrong without the check: the Host allowlist (DNS rebinding against a server whose entire access control is "you are on this machine"); the CSRF double-submit (why there is no session to compare against); `safeName` (why membership beats prefix comparison on Windows); `WriteTimeout: 0` (any finite value kills an SSE stream).

Named tests: `TestEveryRouteResolves`, `TestHostHeaderRejectsRebinding` (421 on every route including `/static/` and `/events/stream`), `TestServedFileNamesAreAllowlisted` (the hostile table from spec §12.1 — each 400 or 404, and the body carries no byte from outside the directory), `TestTemplatesExecuteWithZeroValues`, `TestCSRFCookieAttributes`, `TestShutdownIsClean`, `TestNoStubsRemain`, `TestHouseRules`.

`fixture_test.go` follows `internal/cli/review_test.go`'s shape and pins `newEnv`, `job`, `get`, `post` (sends the CSRF cookie+field), `postRaw` (sends neither), and `approvals()`.

> **CHECKPOINT 3 — after W2-G, before W2.1 starts.** The security surface must not be retrofitted after six owners have built on top of it. Confirm: host check, CSRF, `safeName`, `MaxBytesReader`, and the four house-rule greps all pass; the route table matches spec §5 line for line; no W2.1 owner needs to add a route or a helper.

### W2.1 — six parallel owners inside `internal/server`

None edits `server.go`, `safety.go`, `assets.go`, `base.html`, or `app.css`.

#### W2-H — read-only pages
**Owns:** `pages.go`, `pages_test.go`, `templates/{jobs,job,events,logs,log,artifacts,artifact,prompt,submit,config}.html`.
**Routes:** the ten GET pages. **Size:** ~380 production, ~450 templates.
Job list: "Waiting on you" block first, then everything else newest-updated first. Log page: last 2000 lines by default with a banner and a link to `?all=1`; `?all=1` reads at most 8 MiB from the tail with a banner naming the truncation and the full path; `?from=<byte>` seeks and returns at most 1 MiB plus the new offset, restarting from 0 and saying so if the file shrank. `review/1` artifacts render as findings with severity and verification verdicts. `/config` re-marshals the loaded config to YAML alongside the resolved paths.
Named tests: `TestJobListPutsWaitingJobsFirst`, `TestLogTailDefaultsTo2000LinesWithABanner`, `TestLogAllIsBoundedAndSaysSo`, `TestLogFromOffsetReturnsOnlyNewBytes`, `TestLogFromOffsetRestartsWhenTheFileShrank`, `TestReviewArtifactRendersFindings`, `TestAgentTextIsEscaped`, `TestConfigPageIsReadOnly`.

#### W2-I — decisions and submit
**Owns:** `actions.go`, `actions_test.go`, `templates/_forms.html`.
**Routes:** the six POSTs. **Size:** ~240 production, ~180 templates, ~200 test.
Publishes named templates `decisionForm`, `holdForm`, `diffViewToggle` that H and J invoke. Every handler writes exactly one `store.Approval` through `st.AddApproval` and does nothing else. `POST /submit` calls `jobs.Submit` and nothing else. Every handler answers 303.
Normative: an empty reject reason is 400 with no row; `cancel` requires `confirm=yes`; `resume`'s `to` is checked with `engine.IsResumableState` before the row is written; the `state` field drives `requireFreshState` (409 + zero rows); a pending approval is 409 with the same sentence `reviewer.promptGate` uses.
Named tests: `TestCSRFRequiredOnEveryPost` (zero rows asserted through the store, not the response), `TestApprovalRowMatchesCLI`, `TestStaleGateIsRefused`, `TestSecondDecisionIsRefused`, `TestRejectRequiresReason`, `TestCancelRequiresConfirmation`, `TestResumeValidatesTargetState`, `TestEveryPostAnswers303`, `TestSubmitFromUICreatesSameJob`.

#### W2-J — the review document as HTML
**Owns:** `render.go`, `render_test.go`, `templates/gate.html`. **Route:** `GET /jobs/{id}/gate`. **Size:** ~200 production, ~200 template, ~220 test.
Renders `review.Doc.Blocks` to HTML, one case per `BlockKind`, and implements A2's `gateDoc` per §2.7 with the rationale comment reproduced verbatim. The page states, in the document header, whether the reader has the live render or the copy written at park time. All strings go through `html/template` as plain interpolation; no `template.HTML` anywhere.
Named tests: `TestEveryBlockKindRenders`, `TestSpanKindsRoundTrip`, `TestGatePageCarriesTitleFindingsAndActions`, `TestFindingDescriptionIsEscaped`, `TestGatePageFallsBackToPersistedDocument`, `TestGatePageWithNothingWaiting`.

#### W2-K — the diff viewer
**Owns:** `diffpage.go`, `diffpage_test.go`, `templates/{diff,difffile}.html`, `static/diff.css`, `bench_test.go`.
**Routes:** `/jobs/{id}/diff`, `/diff/{index}`, `/diff.patch`. **Size:** ~180 production, ~250 templates, ~250 CSS, ~180 test.
**A3 is not descopable:** unified (file-by-file, collapsible, per-file counts) *and* side-by-side, both server-rendered from the same `[]*diff.File`, with the toggle. No client-side re-parse, no second parser.
File index at the top: path, status badge, counts, and a test-file badge when the path matches `policies.test_file_globs` via `guard.CompileGlobs`. Files over 500 rendered lines start collapsed; past 20 000 rendered rows the remaining files become stubs linking to the per-file page. Banners for: truncated patch (from `review.Doc.DiffTruncated` — never a length heuristic), `Malformed` hunks, worktree gone, empty diff. Monospace stack of installed fonts only; no webfont.
Named tests: `TestDiffPageRendersBothViews`, `TestViewDefaultsFromCookieThenUnified`, `TestRowBudgetDegradesToStubs`, `TestTruncatedPatchShowsTheBanner`, `TestMalformedHunkShowsTheBanner`, `TestDiffCacheKeyIncludesHeadSHA`, `TestRawPatchIsPlainText`, `BenchmarkRenderBigPatch`.

#### W2-L — live updates
**Owns:** `live.go`, `live_test.go`, `static/app.js`. **Routes:** `/events/stream`, `/api/jobs.json`, `/api/jobs/{id}.json`. **Size:** ~200 production, ~250 JS, ~200 test.
Hub: one goroutine started by `Start`, stopped by `Shutdown`. Every 1s it calls `st.ListJobs()` and digests `(id, state, updated_at, hold_reason, resume_after, counters)`; on a change it broadcasts a snapshot. Every 15s it broadcasts `ping` unconditionally. One buffered channel per client, depth 4; a client that cannot keep up is dropped rather than backpressuring the hub. Max 32 clients; the 33rd gets 503.
The required comment: **why a snapshot rather than a delta** — a client that missed events must not have to reconstruct anything; a full list is a few kilobytes and repaints correctly after any gap, which is what makes a dropped client harmless.
`app.js` (vanilla, no build step): `EventSource`, in-place row replacement, the three-state pill (live / amber stale-reconnecting naming the age of the last update / red disconnected after 5 minutes), relative times computed in the browser from `datetime` attributes with absolute local time in `title`, the log-follow poller, and — normatively — **disabling the decision buttons on any open gate page** when the stream is stale or the snapshot shows a different state, with the reason shown. First paint is server-rendered and every page works with JavaScript off.
`humanSince` is not duplicated server-side for live values — a server-rendered "4m ago" is wrong the moment it is delivered, and the operator may be on the far end of an `ssh -L` in another timezone.
Named tests: `TestSSEDeliversSnapshotOnChange`, `TestSSEPingsOnAnIdleConnection`, `TestSSEDropsSlowClient`, `TestSSERejectsThe33rdClient`, `TestSnapshotJSONFieldNames`, `TestHubStopsOnShutdown`.

#### W2-M — `sdlc serve` and the submit delegation
**Owns:** `internal/cli/serve.go`, `internal/cli/cli.go`, `internal/cli/serve_test.go`.
**Must not touch:** `internal/cli/review.go`, `review_test.go`, `cli_test.go`, or anything under `internal/server`. **Size:** ~70 + ~50 net + ~140 test.
`cli.go` has exactly one owner in this plan, and this is it — both edits land together: `cmdSubmit` becomes flag parsing, file reading and a call to `jobs.Submit`, mapping `jobs.ErrNoTitle` to exit 2 and everything else to exit 1, preserving today's messages and exit codes exactly; and the `serve` case plus its usage line.
`cmdServe` mirrors `cmdRun`'s shape: `--addr` (validated through `config.ValidateListen(addr, true)`) and `--v`; `signal.NotifyContext`; prints the actual listening address so port 0 is usable; `Shutdown` with a 5s grace so an in-flight approval POST is not truncated; exits non-zero naming the address when the port is taken.
Named tests: `TestServeBindsAndAnswersHealthz`, `TestServeRejectsNonLoopbackAddr`, `TestSubmitStillPrintsTheSameLines`, `TestCreateJobHasOneNonTestCaller`.

> **CHECKPOINT 4 — end of W2, before docs.** `gofmt -l .` empty, `go vet ./...` clean, `go test ./... -race` green, `git diff --exit-code go.mod go.sum`, `git diff --stat internal/engine/` showing only `jobctx.go`, `TestNoStubsRemain` passing, all four `TestHouseRules` greps. Build and run once with the network disabled to confirm AC-3 by observation as well as by grep.

### W3 — documentation

#### W3-N — docs
**Owns:** `docs/SPEC.md`, `docs/running.md`, `README.md`. **Must not touch:** any code, `sdlc.example.yaml` (W1-F's). **Size:** ~260 lines of prose.
- `docs/SPEC.md` §1.2: "No web dashboard" becomes a pointer to this feature and a statement of the localhost-only, no-auth boundary. §11 gains `serve`. §12 gains the `server:` block. §14 gains `diff/`, `jobs/`, `server/`.
- `docs/running.md` gains a §8: what `sdlc serve` is, what it explicitly does not protect against, and the `ssh -L 7777:127.0.0.1:7777 host` recipe.
- `README.md`: one line in the command list.
- A1's truncation flag documented in `docs/SPEC.md` §8; A2's live-with-fallback rule in `docs/running.md` §8.

---

## 4. Ambiguities resolved

1. **Golden determinism** — fixed relative paths, `StateEnteredAt = now-90m`, `time.Date(..., time.Local)` events, exactly one permitted normalization (`\` to `/`, both sides, commented).
2. **`SpanEmphasis` is missing from the spec's closed span set.** The existing markdown uses italics in at least six places. Without it, byte-for-byte goldens are unreachable. Added.
3. **`Block` as one fat struct, not an interface** — `html/template` cannot type-switch.
4. **`renderMarkdown` must not join blocks with a separator** — each block emits its own trailing whitespace.
5. **The `Full patch:` line's exact bytes** — pinned, with the test landing in W0 so E has the target before starting.
6. **Where truncation appears in the markdown** — inside `renderDiff`'s success branch, before the fence, so the warning precedes the content. Only emitted when truncated, so W0-A's goldens are unaffected by W0-B.
7. **Whether the engine surfaces truncation** — no. Discarded with a comment. Surfacing it would be an engine behaviour change.
8. **Whether `Result.Truncated` counts the stderr limiter** — yes, both.
9. **Intra-line marks: rune or byte offsets** — byte offsets, computed on rune boundaries.
10. **AC-25's numstat oracle** — committed beside each fixture, not run at test time; keeps the pure package free of process spawns.
11. **A2's fallback has no block tree** — it becomes a single `BlockCode`, and it fires on a target-resolution or `Render` error, *not* on a missing worktree (which `renderDiff` already handles inside a successful render).
12. **One `*template.Template` or many** — a map, one clone of base per page, with the reason in a comment.
13. **`template.HTML` for asset paths** — not needed; pinned to zero occurrences.
14. **AC-28's either/or** — the staleness rule lives only in JS; the server-side criterion is that no page renders a server-side freshness claim at all.
15. **`jobs.Submit` error mapping** — `ErrNoTitle` + `errors.Is`, because today unknown target exits 1 and missing title exits 2.
16. **Who owns `internal/cli/cli.go`** — one owner, W2-M, doing both edits.
17. **Who owns `sdlc.example.yaml`** — W1-F, with the validation it must match.
18. **Shared CSS and shared forms** — `app.css` (G) and `diff.css` (K) separate; decision forms live in I's `_forms.html` as named templates.
19. **How six agents share one Go package** — the skeleton commit with `stub(...)` bodies, file-level ownership, and `TestNoStubsRemain` proving the wave finished.
20. **Whether the golden test gets an `-update` flag** — yes, with a comment and a failure message that both say a golden failing after the refactor is the bug, not the file.

## 5. Risk register

- **W1-E, the review refactor** — rewrites the internals of a file the terminal path depends on. Mitigated entirely by W0-A landing first; that is the only mitigation and it is why W0 exists.
- **W1-C, the diff parser** — the one component that can be wrong quietly. Mitigated by fourteen fixture tests, the numstat oracle, the split/unified invariant, and the fuzzer.
- **W2-G, `safety.go`** — a mistake here is not a bug, it is an unauthenticated approve button. Its own checkpoint.
- **W2-L, the SSE hub** — the only concurrent code in the feature. Built to shape rather than fixed after the race detector complains.
- **W2 package-sharing discipline** — six agents in one package works only if nobody edits `server.go` after the skeleton. An owner who needs a new route or shared helper has found a contract gap: raise it, amend §2.7, let G land it.
