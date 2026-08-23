# Local server and web UI — Specification

Version: 1.0
Status: Proposed for implementation
Date: 2026-08-21
Amends: [`docs/SPEC.md`](../docs/SPEC.md) §1.2 (which currently says "No web
dashboard"), §11 (CLI), §12 (config), §14 (layout)

---

## 1. Summary

`sdlc serve` starts a local HTTP server, hosted in the same binary, that
manages everything the CLI manages: the job list with live state, job detail,
submitting new jobs, deciding every gate (spec, code, merge, release), clearing
holds with resume/cancel, the event timeline, the phase logs, the artifacts,
and a read-only view of the loaded config. It binds `127.0.0.1` and has no
authentication, because its trust boundary is the machine — exactly the CLI's.

The feature exists because of one asymmetry the CLI cannot fix. Everything the
orchestrator *does* is legible from a terminal — a transition is one line, a
counter is one number. The two things a human is asked to *judge* are not: a
spec is a page of prose, and a merge-gate patch is routinely three thousand
lines across forty files. `sdlc review` renders both faithfully and then hands
them to a pager, which is the wrong instrument for a diff. The UI is a better
instrument for those two objects and nothing more ambitious than that.

Three properties are load-bearing and everything below is arranged around them:

1. **The server is an additional caller of existing packages, not a rewrite.**
   It reads the same SQLite database, calls the same `review.Render`, and writes
   the same `store.Approval` rows the CLI writes. `internal/engine` is not
   modified.
2. **The terminal and the browser cannot drift.** `internal/review` exists so
   that what the operator reads in a terminal and what they open in an editor
   are the same words (read its package comment). A third reader must not be
   allowed to become a third truth. §7 specifies the mechanism.
3. **Nothing goes stale silently.** A job list that stops updating, or an
   approve button that still works after the job moved on, is the same class of
   defect the whole orchestrator is arranged to prevent: a stop nobody is told
   about. §9 and §10 specify what the UI does instead.

Zero new module dependencies. `net/http`, `html/template`, `embed`, and about
250 lines of hand-written vanilla JavaScript. The repo declined even
`golang.org/x/term` and hand-rolled TTY detection with `os.Stdin.Stat()`
(`cli.interactive`); a dashboard is a much weaker reason to take a dependency
than that was.

---

## 2. Goals and non-goals

### 2.1 Goals

- **G1 — Full management.** Everything §11 of the main spec exposes, exposed
  again over HTTP: list, detail, submit, approve, reject, resume, cancel,
  events, logs, artifacts, config.
- **G2 — The two judgement surfaces are first class.** The spec/plan at the
  spec gate and the patch at the code/merge gates are what the UI is for. The
  diff viewer offers both unified (file-by-file, collapsible, per-file
  add/remove counts) and side-by-side, with a toggle.
- **G3 — One binary, offline.** No node toolchain, no CDN, no external font.
  `go build ./cmd/sdlc` produces a binary that serves the whole UI from
  `embed.FS`.
- **G4 — Additive.** A config that sets nothing behaves exactly as it does
  today. Nothing binds a port unless `sdlc serve` is run.
- **G5 — Testable without a browser.** Every route, every guard and every
  redirect is exercised through `net/http/httptest`.

### 2.2 Non-goals (v1) — normative

These are refusals, not deferrals. Each is here because the obvious next
feature request is the one that would break the trust model.

- **No authentication, no users, no roles.** The bind address is the access
  control. An approval written from the UI is byte-identical to one written by
  `sdlc approve`; the `approvals` table has no actor column and does not gain
  one (§13.5 records why).
- **No remote access.** A non-loopback `server.listen` is a config error, not a
  warning (§11.2). Operators who need remote access use `ssh -L`, which puts
  authentication where it belongs.
- **No TLS.** A loopback listener does not benefit from it, and a self-signed
  certificate would train the operator to click through warnings.
- **No PR integration.** This system merges locally (main spec §1.2, §7); there
  is no pull request to integrate with. The user-facing phrase "PR changes to
  review" means the merge-gate patch, and `/jobs/{id}/diff` is that.
- **No config editing from the UI.** The config is the contract that decides
  which backend reviews which state and what the release command is. It is
  edited in a file, validated at load, and read-only in the browser.
- **No engine control.** The UI does not start, stop or restart `sdlc run`.
  The supervisor owns the engine's lifecycle (`docs/running.md` §5), and a web
  button that stops an engine mid-`IMPLEMENTING` is a way to lose forty
  minutes of work to a mis-click.
- **No live agent-output streaming**, with one bounded exception specified and
  justified in §9.4 (opt-in byte-offset follow on a single log file).
- **No language-level syntax highlighting in v1.** §8.6 argues this at length
  rather than promising something expensive.
- **No editing of artifacts, prompts or the worktree.**
- **No multi-machine or multi-engine dashboard.** One host, one data dir — the
  same constraint the engine lock already enforces.
- **No notifications, email, or webhooks.**
- **No WebSockets.** SSE covers the need with `http.Flusher` (§9).

---

## 3. CLI surface

```
sdlc serve [--addr host:port]
```

- `--addr` overrides `server.listen` and is validated identically (§11.2).
- Prints the URL it is listening on, then blocks until SIGINT/SIGTERM, at which
  point it calls `srv.Shutdown` with a 5s grace period so an in-flight approval
  POST is not truncated. This mirrors `cmdRun`'s `signal.NotifyContext` shape.
- Exits non-zero if the port is taken, naming the address — the operator's most
  likely mistake is a second `sdlc serve`.
- It takes the same global `--config` / `SDLC_CONFIG` resolution as everything
  else, and adds a line to the `usage` block in `internal/cli/cli.go`.

Add to the usage text, in the existing style:

```
  serve     [--addr 127.0.0.1:7777] local web UI (localhost only, no auth)
```

### 3.1 Always a separate process from `sdlc run` — normative

`sdlc serve` does **not** host the engine, and `sdlc run` does **not** host the
server. There is no `--serve` flag on `run`.

Reasons, in order of weight:

1. **The separate process is the existing, proven pattern.** `submit`,
   `approve`, `reject`, `cancel` and `resume` already mutate the same SQLite
   database from another process while the engine runs; WAL plus
   `database.busy_timeout` is exactly the arrangement that makes that safe
   (`store.Open`). The server is one more such writer. Nothing new has to be
   proven.
2. **The engine lock would make the combined form useless in the common case.**
   `engine.acquireLock` permits one engine per data dir, and the deployed
   engine is a supervised keep-alive unit (`docs/running.md` §5). A `serve`
   that also ran the engine would refuse to start whenever the supervisor was
   doing its job — i.e. always.
3. **Restarting the UI must not touch a running job.** Changing `--addr`,
   picking up a rebuilt binary, or recovering from a wedged handler are all
   routine for a UI and catastrophic for a 45-minute `IMPLEMENTING` state.
   Separate processes make "restart the UI" a nothing-event.
4. **Failure isolation.** A panic in a handler, an OOM from a pathological
   patch, or a file descriptor leak in the SSE hub cannot take the engine down
   if it is not in the engine.

The cost is that the server has no in-process signal when a job changes state,
so it discovers changes by polling the database. §9.2 measures that cost and
finds it small.

---

## 4. Package layout

New packages, sized to match the rest of the repo (`guard` is 101 lines,
`review` is 526, `handlers.go` is 760):

```
internal/server/
  server.go        Server struct, New, routes, middleware chain, Shutdown   ~260
  pages.go         GET handlers: list, detail, gate, events, logs, artifacts ~380
  actions.go       POST handlers: approve/reject/resume/cancel/submit        ~240
  render.go        review.Doc block model -> HTML; template funcs            ~200
  diffpage.go      diff page handler, view toggle, parsed-diff cache         ~180
  live.go          SSE hub, digest, snapshot JSON                            ~200
  safety.go        Host check, CSRF, name allowlist for served files         ~160
  assets.go        //go:embed templates/*.html static/*                       ~20
  templates/*.html base.html, jobs.html, job.html, gate.html, diff.html,
                   events.html, logs.html, log.html, artifacts.html,
                   artifact.html, submit.html, config.html, error.html
  static/          app.css (~450 lines), app.js (~250 lines)
  *_test.go

internal/diff/
  diff.go          unified-patch parser -> Files/Hunks/Lines                 ~340
  split.go         pairing for side-by-side + intra-line change spans        ~170
  diff_test.go, testdata/*.patch

internal/jobs/
  submit.go        the one job-creation path, shared by CLI and server        ~90
  submit_test.go
```

Modified:

- `internal/cli/cli.go` — `serve` case, usage line; `cmdSubmit` delegates to
  `internal/jobs`.
- `internal/cli/serve.go` — `cmdServe`, ~60 lines, same shape as `cmdRun`.
- `internal/review/review.go` (+ `blocks.go`) — the block model of §7.
- `internal/config/config.go` — the `server:` block of §11.
- `sdlc.example.yaml`, `README.md`, `docs/running.md`, `docs/SPEC.md`.

**Not modified: `internal/engine`.** If an implementer finds themselves editing
a state handler, the design has gone wrong; stop and re-read §7 and §12.4.

### 4.1 Why the split falls this way

- **`internal/diff` is its own package because it is the only part of this
  feature that can be wrong quietly.** A misrendered patch is a merge approved
  on a false picture of the change. It is a pure function from bytes to a
  struct — no HTTP, no config, no filesystem — so it can be tested to
  exhaustion (§12.2) and fuzzed. Bundling it into `server` would put a fuzz
  target behind an HTTP handler for no reason.
- **`internal/jobs` exists so there is exactly one submit validation path**
  (§10). It is small and that is fine; `guard` is smaller. Its package comment
  must say why it exists, or someone will helpfully inline it back into `cli`.
- **`render.go` is separate from `pages.go`** because the review-document
  renderer is the piece with the drift obligation; it should be readable
  without wading through routing.
- **`safety.go` is separate** for the same reason `guard` is: the checks that
  exist to stop something bad are easier to audit when they are in one file
  with the reasoning attached.

### 4.2 Comment style

The repo's comments explain *why*, at the point a reader would otherwise be
misled — see `cli.interactive` (why `/dev/null` is not a terminal),
`review.renderDiff` (why no patch path is printed there),
`engine.announceGate` (why the mutex is not held across the git call). New code
matches that. Specifically, every one of these gets a comment saying what goes
wrong without it, and none of them get a comment restating the code:

- the Host allowlist (what a rebinding attack actually does),
- the CSRF double-submit pair (why no server-side session exists),
- the served-name allowlist (why membership beats prefix comparison on Windows),
- the stale-gate 409 (why a left-open tab is a live approve button),
- the hunk-state-aware parse loop (why `----foo` is content, not a header),
- the SSE digest (why a snapshot is sent rather than a delta).

---

## 5. Route table

`http.ServeMux` with Go 1.22+ method-and-wildcard patterns
(`"GET /jobs/{id}"`). No router dependency, and unmatched methods produce 405
from the mux itself.

All HTML routes go through the middleware chain in order:
`recoverPanic → checkHost → withCSRFCookie → logRequest(only when --v)`.
All `POST` routes additionally go through `requireCSRF → requireFreshState`
(§10.3) before touching the store.

### 5.1 Pages (HTML)

| Method | Path | Does |
|---|---|---|
| GET | `/` | 303 to `/jobs` |
| GET | `/jobs` | Job list. "Waiting on you" block first (`review.GateFor(state) != ""`), then everything else, newest-updated first. Columns: id, target, state, since, title, gate. Live (§9). |
| GET | `/jobs/{id}` | Job detail: state, counters, hold reason, resume-after, branch/worktree paths, last `progress` event, links to gate/diff/events/logs/artifacts, and the decision form when a gate is open. |
| GET | `/jobs/{id}/gate` | The gate document for `review.GateFor(job.State)`, rendered from `review.Doc` (§7), with the decision form. 200 with an explanatory page when nothing is waiting — the same sentence `review.Render` produces. |
| GET | `/jobs/{id}/diff` | Diff viewer. `?view=unified\|split` (default from the `sdlc_diff_view` cookie, then `unified`); `?file=<index>` anchors and expands one file. |
| GET | `/jobs/{id}/diff/{index}` | One file's diff, full page. The escape hatch for a file too large to render inline (§8.5). |
| GET | `/jobs/{id}/diff.patch` | The raw patch, `text/plain; charset=utf-8`, `Content-Disposition: inline`. What a reviewer pipes into their own tools. |
| GET | `/jobs/{id}/events` | Full event timeline, newest last (matching `sdlc events`). `?kind=` filters to one kind; `?state=` to one state. Detail JSON is pretty-printed in a collapsible cell. |
| GET | `/jobs/{id}/logs` | The log directory listing, names + sizes + mtimes, newest last (matching `sdlc logs`). |
| GET | `/jobs/{id}/logs/{name}` | One log file. Last 2000 lines by default; `?all=1` for the whole file (bounded, §5.4); `?from=<byte>` returns only bytes after that offset (used by follow, §9.4). |
| GET | `/jobs/{id}/artifacts` | The artifact directory listing. |
| GET | `/jobs/{id}/artifacts/{name}` | One artifact, pretty-printed JSON. `review/1` artifacts additionally render as findings with severity and verification verdicts, because that is the shape a human reads them in. |
| GET | `/jobs/{id}/prompts/{name}` | One rendered prompt. The record of what was actually sent (main spec §5) and the first thing an operator wants after a bad state. |
| GET | `/submit` | New-job form: target (select, from `cfg.Targets`), title, body, and a "paste an issue file" textarea. |
| GET | `/config` | Read-only config: the loaded `*config.Config` re-marshalled to YAML, plus the resolved paths (data dir, DB, lock file) and the config file's own path. |
| GET | `/static/{path...}` | Embedded CSS/JS. Served with `Cache-Control: public, max-age=31536000, immutable`; the URL carries a build hash (`/static/app.a1b2c3d4.css`) so a redeploy invalidates it. |
| GET | `/healthz` | `text/plain` `ok`. For a supervisor, and for the acceptance test that the listener is up. |

### 5.2 Actions (POST, form-encoded, CSRF-checked, 303 back)

Every one of these writes exactly one `store.Approval` row through the same
call the CLI uses, and none of them do anything else.

| Method | Path | Form fields | Row written | Redirect |
|---|---|---|---|---|
| POST | `/jobs/{id}/approve` | `csrf`, `state`, `note?` | `{Gate: review.GateFor(state), Decision: "approve", Reason: note}` | `/jobs/{id}` |
| POST | `/jobs/{id}/reject` | `csrf`, `state`, `reason` (required, non-empty), `cancel?` | `{Gate: …, Decision: "reject", Reason: reason, Cancel: cancel}` | `/jobs/{id}` |
| POST | `/jobs/{id}/resume` | `csrf`, `state`, `to?`, `note?` | `{Gate: "resume", Decision: "resume", Reason: to, Note: note}` | `/jobs/{id}` |
| POST | `/jobs/{id}/cancel` | `csrf`, `state`, `confirm=yes` | `{Gate: "cancel", Decision: "cancel"}` | `/jobs/{id}` |
| POST | `/submit` | `csrf`, `target`, `title`, `body` | (no approval; creates a job via `jobs.Submit`) | `/jobs/{new-id}` |
| POST | `/prefs/diff-view` | `csrf`, `view` | (no row; sets the `sdlc_diff_view` cookie) | back to `Referer` if same-origin, else `/jobs` |

Normative details:

- `reject` with an empty `reason` is a 400 with the field flagged, never a row.
  The reason is handed to the next agent verbatim (`cli.rejectReason` says
  exactly why), and an empty rejection tells it only that somebody said no.
- `cancel` requires an explicit confirmation field, matching
  `reviewer.promptHold`'s `[y/N]`: cancel deletes the worktree and one click is
  the wrong affordance for that.
- `resume`'s `to` is validated with `engine.IsResumableState` before the row is
  written, and the form renders `engine.ResumableStates()` as a `<select>`, so
  an invalid value is not reachable from the UI and is still rejected when
  posted by hand.
- The `state` field carries the job state the page was rendered from. §10.3
  specifies what happens when it no longer matches.
- Every handler answers with 303 (POST/redirect/GET) so a browser reload never
  re-posts a decision.

### 5.3 Live and data routes

| Method | Path | Does |
|---|---|---|
| GET | `/events/stream` | `text/event-stream`. Events: `jobs` (full list snapshot), `ping` (server time, every 15s). `?job=<id>` narrows the `jobs` payload to one job but keeps the pings. §9. |
| GET | `/api/jobs.json` | The same snapshot as one `jobs` event. Used on first paint, on reconnect, and by anyone who wants to script against the UI. |
| GET | `/api/jobs/{id}.json` | One job's snapshot, including counters and the latest `progress` event. |

These are JSON, `Cache-Control: no-store`, and read-only. They are not a public
API and carry no stability promise; the spec says so here so nobody builds on
them and then reports a break.

### 5.4 Bounds — normative

| Thing | Bound | On exceeding |
|---|---|---|
| Request body (all POSTs) | 1 MiB via `http.MaxBytesReader` | 413 |
| Log page, `?all=1` | 8 MiB read from the tail | Banner naming the truncation and the full path on disk |
| Log page, default | last 2000 lines | Banner + link to `?all=1` |
| Patch | whatever `gitx.DiffPatchSince` returns (4 MiB cap, §13.3) | Truncation banner |
| Eagerly rendered diff rows | 20 000 across the page | Remaining files render as collapsed stubs linking to `/jobs/{id}/diff/{index}` |
| One file rendered inline | 500 lines before it defaults to collapsed | — |
| Concurrent SSE clients | 32 | 503 with a plain-text explanation |
| `ReadHeaderTimeout` | 10s | — |
| `WriteTimeout` | 0 (SSE would be killed by any finite value); slowloris is bounded by `ReadHeaderTimeout` and `MaxBytesReader` instead | — |

---

## 6. Data flow from artifacts to browser

```
SQLite (jobs/events/approvals) ─┐
                                ├─▶ review.Render(ctx, Options) ─▶ review.Doc
artifacts/*.json (spec, plan,  ─┤        (unchanged call site)      ├─ Blocks ─▶ html/template ─▶ browser
  review, implementation)       │                                  ├─ Body   ─▶ terminal / file
git diff vs Counters.BaseSHA ──┘                                   └─ Diff   ─▶ internal/diff ─▶ diff templates
```

Nothing is re-derived. The server reads jobs and events with `store.ListJobs` /
`store.GetJob` / `store.ListEvents`; it reads artifacts only through
`artifact.Load*`; it obtains gate content only through `review.Render`; it
obtains the patch only from `review.Doc.Diff`. There is no second copy of the
state→gate mapping (`review.GateFor` is it), no second severity ordering
(`artifact.SeverityRank` is it), and no second resumable-state list
(`engine.ResumableStates` is it).

### 6.1 Freshness and the parsed-diff cache

`review.Render` shells out to git twice (`DiffStatSince`, `DiffPatchSince`)
under a two-minute timeout. Doing that on every request — including every
toggle between unified and side-by-side — is both slow and a way for a browser
to spawn git processes in a loop.

- The gate and diff handlers call `review.Render` with a context derived from
  the request and capped at 20s, so a client that goes away stops the git call.
- The parsed result is cached in the `Server` behind a mutex: key
  `{jobID, job.HeadSHA, job.Counters.BaseSHA, len(patch)}`, value
  `[]*diff.File` plus the raw patch; at most 4 entries, evicted least-recently
  used. The key includes the head sha, so a cached diff can never outlive the
  commit it described — a stale diff at a merge gate is the one cache bug that
  would matter.
- Nothing else is cached. Job rows, events and log files are read per request;
  they are cheap and staleness there is the failure mode this feature exists to
  remove.

---

## 7. Gate content: how `Doc.Body` reaches the browser

### 7.1 The problem

`review.Render` already produces exactly the right content. It produces it as
markdown, because its two existing readers are a terminal and a text editor.
The browser needs HTML, and there is no markdown dependency and will not be
one. Two ways out were considered.

**Option A — a small hand-written markdown renderer**, limited to what
`internal/review` actually emits today: ATX headings, `**bold**`, `-` bullets
with one level of indent, `> ` blockquote, fenced code (` ``` ` and
` ```diff `), `` `inline code` ``, blank-line paragraphs.

**Option B — a structured block model.** `review` builds a typed document tree;
the existing markdown becomes one renderer over that tree, and the templates
become a second renderer over the same tree.

### 7.2 Recommendation: Option B — normative

The deciding argument is what each option makes *impossible* versus what it
makes *detectable*.

Under Option A, the contract between the two surfaces is implicit: "whatever
markdown `review.go` happens to emit today". The day somebody adds a table to
`renderPlan`, or a nested bullet under a finding's verification, or a
`<details>` for a long issue body, the terminal keeps working and the browser
silently renders a literal `|---|---|`. Nothing fails. A test can cover the
constructs it knows about, but the failure mode is precisely the construct
nobody thought to add a test for — and this is the document that decides
whether a change gets merged. That is the same shape of bug as an agent
claiming a test is flaky: plausible output, no mechanical check.

Under Option B the drift is not detected, it is unrepresentable. There is one
document tree with one set of block kinds; both renderers switch over the same
closed set; and an exhaustiveness test (§12.3) fails the build the moment a new
block kind exists without an HTML case. That is the mechanical enforcement this
repo applies everywhere else, applied here.

Option B also removes work rather than adding it: the JSON snapshot routes and
any future document consumer get a structured document for free, and
`renderReview`'s severity/verification formatting stops being duplicated as
string building.

The cost is honest and should be stated: it is an internal rewrite of a
526-line file that the terminal path depends on. §12.3 specifies the golden
test that makes it safe, and §13.1 records it as the largest risk in the
feature.

### 7.3 The block model

Added to `internal/review` (new file `blocks.go`); `Render`'s and `Write`'s
signatures do not change, and `Doc` gains one field.

```go
type BlockKind string

const (
    BlockHeading   BlockKind = "heading"    // Level 2 or 3
    BlockParagraph BlockKind = "paragraph"  // Inline spans
    BlockFacts     BlockKind = "facts"      // the state/branch/worktree/artifacts list
    BlockBullets   BlockKind = "bullets"    // Items, each a []Span, optional Sub
    BlockQuote     BlockKind = "quote"      // the issue body, possibly truncated
    BlockCode      BlockKind = "code"       // Lang: "" | "diff"; Text
    BlockFindings  BlockKind = "findings"   // []artifact.Finding, already ordered
    BlockSteps     BlockKind = "steps"      // []artifact.PlanStep
    BlockActions   BlockKind = "actions"    // what approve/reject do, + the commands
)
```

`Doc` gains `Blocks []Block`. `Doc.Body` stays, and is now produced by
`renderMarkdown(d.Blocks)` — so the terminal output is a *function of* the
tree rather than a sibling of it. `Write` appends its `Full patch:` line by
appending a block and re-rendering, not by string concatenation.

`BlockFindings` and `BlockSteps` carry the artifact structs rather than
pre-formatted strings. That is the point: the severity badge, the verification
verdict ("confirmed 2/3, was major") and the file location are rendered by each
surface in its own idiom, from one set of facts.

Inline spans are a closed set too — `SpanText`, `SpanStrong`, `SpanCode`,
`SpanPath` — so the markdown renderer emits `**x**` / `` `x` `` and the HTML
renderer emits `<strong>` / `<code>`, and neither has to parse the other's
output.

### 7.4 Escaping — normative

Every string in the tree originates from an agent, an issue body, or a config
file, and **nothing an agent says is trusted** (README). All of it is rendered
through `html/template`'s contextual escaping as plain interpolation.
`template.HTML`, `template.JS` and `template.URL` do not appear anywhere in
`internal/server` except for the build-hash asset paths, and a test greps for
that (§14, AC-17). A finding whose description is `<img src=x onerror=…>` must
render as text on the merge-gate page, because a reviewer's browser executing
an implementer's payload is a straight line from "agent output" to "approve
button".

---

## 8. Diff rendering

This is the hardest correctness surface in the feature. A patch rendered wrong
is a merge approved on a false picture of the change, and it fails silently —
the reviewer has no way to know the parser dropped a file.

### 8.1 Input

`review.Doc.Diff`, which is `gitx.DiffPatchSince` output, which is
`git diff <base>` run in the job worktree with `StderrSeparate: true`. So:
`a/`…`b/` prefixes present, rename detection on (git ≥ 2.9 default), binary
files summarised rather than encoded, no combined-diff (`@@@`) hunks possible
because this is never a merge diff.

### 8.2 Model

```go
type File struct {
    OldPath, NewPath string
    Status           Status // Added | Deleted | Modified | Renamed | Copied | ModeOnly
    OldMode, NewMode string
    Binary           bool
    Hunks            []Hunk
    Adds, Dels       int
    Malformed        string // non-empty ⇒ render a visible warning, never drop the file
}

type Hunk struct {
    OldStart, OldLines, NewStart, NewLines int
    Section string  // the text after the closing @@ (git's function-context hint)
    Lines   []Line
}

type Line struct {
    Kind        Kind // Context | Add | Del
    OldNo, NewNo int // 0 when the line does not exist on that side
    Text        string
    NoNewline   bool // the "\ No newline at end of file" marker applied to this line
}
```

`Parse(patch string) ([]*File, error)` is the whole API surface, plus
`Split(h Hunk) []Row` from §8.4. It touches no filesystem and spawns no
process.

### 8.3 Cases the parser must handle — each gets its own test

Listed because each one has a specific way of going wrong.

1. **New file.** `new file mode 100644`, `--- /dev/null`, `+++ b/x`. The path
   must come from `+++`/the `diff --git` header; a parser that takes the path
   from `---` names the file `/dev/null`.
2. **Deletion.** `+++ /dev/null`. Symmetric, and the file must still appear in
   the list with its removed lines — a deleted 400-line file is the change most
   worth seeing.
3. **Pure rename, no content change.** `similarity index 100%`,
   `rename from`/`rename to`, and **no `@@` at all**. A parser driven by hunk
   headers drops the file entirely. It must appear with a rename badge and
   0/0 counts.
4. **Rename with edits.** Both paths differ and hunks exist. Side-by-side shows
   the old path over the left gutter and the new path over the right.
5. **Binary.** `Binary files a/x and b/x differ` (default) or `GIT binary patch`
   (only with `--binary`, which we never pass, but the parser must not choke if
   a future caller does). `Binary: true`, no hunks, render "binary file
   changed" and never attempt to display bytes.
6. **Mode change only.** `old mode 100644` / `new mode 100755` with no `index`
   and no hunks. Renders as a file entry with a mode badge and zero line
   changes — a file becoming executable is a real review finding.
7. **`\ No newline at end of file`.** It is a marker, not a diff line: it must
   not consume a line number and must not count toward adds/dels. It attaches
   to the *preceding* line, can appear after a `-`, a `+` or a context line,
   and can appear twice in one hunk (once per side). Both occurrences must
   attach correctly.
8. **Hunk headers without counts.** `@@ -1 +1 @@` means one line;
   `@@ -0,0 +1,5 @@` means the old side is empty. Parse `-l[,s]` on both sides
   and default the count to 1.
9. **Hunks with no trailing context** (a change at EOF) and hunks whose parsed
   line counts disagree with the header. Never discard lines to match the
   header: keep them, set `Malformed`, and render a visible warning. A patch
   the parser half-understood must announce itself.
10. **Content that looks like a header.** Inside a hunk, every line carries a
    prefix character, so a removed line `---foo` arrives as `----foo` and a
    context line `diff --git …` arrives as ` diff --git …`. The parse loop is
    **state-aware**: while inside a hunk with lines remaining, a line is
    classified by its first byte only, and file-header patterns are matched
    only outside a hunk. This is the classic unified-diff parser bug and the
    reason the loop is written as an explicit state machine rather than a
    sequence of `strings.HasPrefix` checks.
11. **Quoted and space-containing paths.** With `core.quotePath` (on by
    default) git C-quotes paths containing non-ASCII or special characters:
    `diff --git "a/naïve file" "b/naïve file"`. Unquote with
    `strconv.Unquote`, falling back to the raw text when it fails. Prefer the
    `--- `/`+++ ` lines and the `rename from`/`rename to` lines for paths;
    only fall back to splitting the `diff --git` line when neither exists
    (mode-only and pure-rename chunks). That fallback is genuinely ambiguous
    for a path containing ` b/` — split on the last such occurrence and record
    the residual risk here rather than pretending it is solved.
12. **CRLF.** The repo is Windows-first and `.gitattributes` normalises to LF,
    but a target repo need not. Split on `\n`, strip one trailing `\r` for
    display, and mark a line whose only difference from its pair is trailing
    whitespace — otherwise a whitespace-only change renders as two identical
    lines and reads as a parser bug.
13. **Combined diffs (`@@@`).** Impossible from this call site. The parser
    rejects them with a clear error rather than misparsing them into
    plausible-looking nonsense.
14. **Empty patch** (a gate whose diff is empty) and **truncated patch** (§13.3):
    parse what is there, never error, and let the page say so.

### 8.4 Side-by-side pairing

Per hunk, one pass:

- A context line emits one `Row` with the same text on both sides and both line
  numbers set.
- A maximal run of consecutive `-` lines followed by a maximal run of
  consecutive `+` lines is a *change block*. Pair the i-th deletion with the
  i-th addition; whichever side runs out first gets filler rows (a greyed,
  numberless cell). This is positional pairing, not an LCS — it is what every
  side-by-side viewer does, it is O(n), and it is right for the overwhelmingly
  common case of an edited line.
- Deletions with no following additions, and additions with no preceding
  deletions, are pure removals/insertions and pair against filler.

**Intra-line marks.** For a paired row, compute the common rune prefix and
common rune suffix and mark the middle span on each side. Guard it: if the
marked span exceeds 70% of the longer line, drop the marks — two unrelated
lines paired by position would otherwise light up entirely and mislead. About
40 lines, a pure function, trivially tested, and the single biggest
readability win available without a dependency.

**Invariant, enforced by test:** the sequence of non-filler add/del lines in
the split view equals the sequence in the unified view, in the same order.
The two views are two renderings of one model and cannot disagree about what
changed (§12.2).

### 8.5 Page behaviour

- **File index** at the top: path, status badge (added/deleted/renamed/mode/
  binary), `+n −m` counts, a test-file badge when the path matches
  `policies.test_file_globs` via `guard.CompileGlobs` — the orchestrator
  already cares which changes touch tests (main spec §13.1) and so does the
  reviewer. Clicking a row jumps to the file.
- **Per-file `<details>`**, so collapse works with JavaScript off. Files over
  500 lines start collapsed; everything else starts open.
- **Toggle** between unified and split is a link to `?view=…`, and a small
  `POST /prefs/diff-view` sticks the choice in a cookie. Server-rendered both
  ways from the same `[]*diff.File`, so there is no client-side re-parse and no
  second parser.
- **Row budget** (§5.4): once 20 000 rendered lines are reached, the remaining
  files render as stubs linking to their own page. A 3000-line patch is
  comfortably inside the budget; a 40 000-line one degrades to navigable stubs
  instead of a wedged tab.
- **Banners** for: truncated patch, `Malformed` hunks, worktree gone (the
  message `review.renderDiff` already produces), and empty diff.

### 8.6 Syntax highlighting — the honest answer

**Mandatory and cheap: diff-level colouring.** Add/remove/context backgrounds,
two line-number gutters, intra-line change marks, a monospace stack of
installed fonts only (`ui-monospace, SFMono-Regular, Menlo, Consolas,
"DejaVu Sans Mono", monospace` — no webfont, the machine may be offline).
This is CSS plus the classification the parser already produces, and it is
most of the value.

**Not in v1: language-level highlighting.** The recommendation is none, for a
reason more specific than "it's a lot of code":

A hand-rolled tokenizer for the languages that matter here (Kotlin, Java, Go,
YAML, Gradle KTS) is 600–1000 lines of state machine with real ambiguities —
Kotlin string templates (`"$a ${b.c}"`) and raw strings (`"""`), Go raw strings
and generics-versus-shift, YAML block scalars. But the disqualifying problem is
not the languages, it is the input: **a hunk is a fragment.** A hunk that
starts on line 402 gives no way to know whether line 402 is inside a block
comment or a raw string. Highlighting it correctly requires the whole file,
which is frequently unavailable — the worktree may be cleaned up
(`git.cleanup_worktrees`), and a deleted file has no new content at all. The
failure mode is a hunk rendered entirely as "string" because a `"` was opened
above the window, which is worse than plain text: it looks confident and it is
wrong, on the page where a human decides whether to merge.

**If it is ever wanted**, the honest shape is: fetch full file content with
`git show <base>:<path>` and `git show HEAD:<path>`, tokenize whole files,
project the token spans onto the hunk lines. That is two extra process spawns
per file, a cache, and the tokenizer — a feature of its own size, not a corner
of this one. Recorded here so the tradeoff does not have to be rediscovered.

---

## 9. Live updates

### 9.1 Decision: Server-Sent Events

`net/http` handles SSE with `http.Flusher` and no dependency, and the
comparison is not really "SSE versus polling" — SQLite offers no cross-process
change notification, so *something* polls the database either way. The question
is where.

With browser polling, N open tabs make N database reads per interval, each
request is independently able to fail, and a tab whose fetches are all failing
looks exactly like a tab where nothing is happening. With SSE, one server
goroutine reads the database on a fixed cadence and fans out to all clients,
and — the deciding property — **the connection itself carries liveness**. A
heartbeat that stops arriving is an unambiguous signal the page can show. That
is what makes "the list is stale" a visible state rather than an invisible one,
which is the whole reason this feature exists.

### 9.2 Server side

- One hub goroutine started by `Server.Start`, stopped on `Shutdown`.
- Every **1s** it calls `st.ListJobs()` and computes a digest over
  `(id, state, updated_at, hold_reason, resume_after, counters)` for all jobs.
  On a change it marshals a snapshot and broadcasts. `ListJobs` on a realistic
  job table is a single indexed scan of tens of rows; at 1s that is noise next
  to the engine's own 3s tick.
- Every **15s** it broadcasts `ping` carrying the server's UTC time,
  unconditionally.
- Broadcast is a buffered channel per client (depth 4). A client that cannot
  keep up is dropped rather than backpressuring the hub — snapshots are
  idempotent, so a dropped client's next connection repaints correctly.
- Response headers: `Content-Type: text/event-stream`,
  `Cache-Control: no-store`, `Connection: keep-alive`,
  `X-Accel-Buffering: no`. Flush after every event. Return when
  `r.Context().Done()` fires.
- **A snapshot, not a delta.** A client that missed events must not have to
  reconstruct anything; a full list is a few kilobytes and is self-healing.

### 9.3 Client side and refresh semantics — normative

- First paint is server-rendered. `EventSource` then replaces rows in place;
  it is an enhancement, and every page is fully usable with JavaScript off (a
  plain "Refresh" link is rendered and only hidden once the stream is live).
- A status pill in the header has exactly three states:
  - **live** — an event arrived within the last 45s (3 heartbeats);
  - **stale — reconnecting** (amber) — nothing for 45s, or `EventSource`
    reported an error. The pill names the age of the last update.
  - **disconnected** (red) — reconnect has been failing for 5 minutes.
- In the amber and red states the page **stops presenting itself as current**:
  the pill states the last-update time, and the decision buttons on any open
  gate page are disabled with the reason shown. A stale page must not offer to
  approve something.
- `EventSource` reconnects on its own; the first `jobs` event after a
  reconnect repaints everything.
- Relative times ("4m ago") are computed in the browser from RFC3339 UTC
  timestamps carried in `datetime` attributes, with the absolute local time in
  the `title`. `humanSince` is not duplicated server-side for live values —
  a server-rendered "4m ago" is wrong the moment it is delivered, and the
  operator may be on the other end of an `ssh -L` in another timezone.

### 9.4 The one live-output exception: log follow

The log page (`GET /jobs/{id}/logs/{name}`) has a **Follow** checkbox, off by
default. When on, `app.js` polls `?from=<byte offset>` every 2s and appends
whatever is new; the handler seeks to the offset, reads to EOF (bounded at
1 MiB per response) and returns the bytes plus the new offset. If the file
shrank, it restarts from 0 and says so.

This is in scope, against the "no live agent output" non-goal, because the
justification is real and the cost is a few dozen lines: agent logs are written
*as output arrives* (main spec §6.1, item 1) precisely so a running state can
be watched, `sdlc logs --last` already exists for the terminal, and
distinguishing a working state from a hung one is the observability property
the main spec spends three mechanisms on. It requires no engine change, no
goroutine per client, and no new protocol — it is a byte offset and a `Seek`.

Everything beyond it stays out: no streaming of the agent's structured events,
no per-line parsing, no server-side tailing goroutines.

---

## 10. Submitting jobs from the UI

### 10.1 One validation path — normative

`cmdSubmit`'s body moves into a new leaf package:

```go
// package jobs

type SubmitRequest struct{ Target, Title, Body string }

// Submit validates the request against cfg, allocates the id, derives the
// branch and worktree paths, and creates the job row.
func Submit(cfg *config.Config, st *store.Store, req SubmitRequest) (*store.Job, error)

// TitleAndBody splits a pasted issue file the way `sdlc submit --file` does:
// the first line, stripped of a leading '#', becomes the title when none was
// given.
func TitleAndBody(title, content string) (string, string)
```

`cmdSubmit` becomes flag parsing, file reading and a call to `jobs.Submit`.
`POST /submit` becomes form parsing and a call to `jobs.Submit`. There is no
second set of rules about which targets exist, what makes a title valid, how a
branch name is derived, or what an empty body defaults to.

### 10.2 Why a new package rather than `engine.Submit`

Putting it in `internal/engine` would be the smaller diff, and it was rejected:
the constraint is that the engine is not modified, and "additive function in
the engine package" is exactly how that constraint erodes. `internal/jobs`
depends only on `config` and `store`, is importable by both `cli` and `server`
without an import cycle, and leaves the state machine untouched. The
alternative that was never on the table — re-implementing validation in the
server — is the specific failure this section exists to prevent.

### 10.3 Deciding a gate that has moved — normative

A browser tab left open on a gate page is a live approve button pointed at a
job that may have moved on hours ago. Three mechanisms, all required:

1. **The `state` field.** Every decision form carries the job state the page
   was rendered from. Before writing a row, the handler re-reads the job; if
   `job.State != form.state`, it answers **409** with a page saying what the
   job is doing now and a link to it, and **writes nothing**.
2. **The pending-approval check.** If `st.PendingApproval(id, gate)` already
   returns a row, the handler answers 409 with the same sentence
   `reviewer.promptGate` uses — a second row would race the first, the engine
   acts on the oldest, and the answer typed second would be silently discarded.
3. **The live stream.** On a gate page, a `jobs` event showing a different
   state disables the buttons in place and shows what happened, so the tab
   usually corrects itself before anyone clicks.

The same `state` check applies to resume and cancel: a resume posted against a
job that is no longer held is refused rather than left as a row for something
else to consume.

---

## 11. Config additions

### 11.1 The block

```yaml
server:
  listen: 127.0.0.1:7777        # sdlc serve binds here; loopback only
```

That is the entire addition: one key.

`CONTRIBUTING.md` says not to add a config knob for something with one
reasonable default, so the things that were *not* made configurable are listed
with their fixed values and the reason each has exactly one sensible answer:

| Fixed | Value | Why not a knob |
|---|---|---|
| SSE digest interval | 1s | Faster is pointless (the engine ticks at 3s); slower makes the UI feel broken. |
| SSE heartbeat | 15s | It exists to detect a dead connection; it has no user-visible tradeoff. |
| Stale threshold | 45s | Defined as 3 heartbeats. A knob would let an operator configure away the warning. |
| Log tail default | 2000 lines | `?all=1` is one click away. |
| Diff row budget | 20 000 | A browser limit, not a preference. |
| Max SSE clients | 32 | Nobody opens 33 tabs on purpose. |

`Config.Server` is added to the struct so strict decoding accepts the key.
Default: `Server{Listen: "127.0.0.1:7777"}`, present in `Default()` so a config
that omits the block still yields a valid address for `sdlc serve`.

**Behaviour with a config that sets nothing is unchanged.** Nothing binds, no
goroutine starts, and no file is written unless `sdlc serve` runs.

### 11.2 Validation — normative

In `Config.Validate`, in the existing `fail(...)` style:

- `server.listen` must parse with `net.SplitHostPort`.
- The host must be a loopback literal: `127.0.0.1`, `::1`, `localhost`, or
  empty-meaning-nothing is rejected outright. Concretely: parse the host with
  `net.ParseIP` and require `IsLoopback()`, or accept the exact string
  `localhost`. **`0.0.0.0`, `::`, and any routable address are configuration
  errors**, with the message naming the reason and the supported alternative:

  ```
  server.listen host "0.0.0.0" is not a loopback address: the server has no
  authentication, so binding it beyond localhost publishes an approve button
  on the network. Forward the port instead: ssh -L 7777:127.0.0.1:7777 host
  ```

  This is a refusal rather than a warning because a warning in a log nobody
  reads is how an unauthenticated approve button ends up on a LAN.
- The port must be 1–65535. Port `0` is rejected in config (an ephemeral port
  the operator cannot predict is not a useful configuration) but **is accepted
  from `--addr`**, which is how tests bind without racing for a fixed port.

### 11.3 Compatibility note

Unknown YAML keys are rejected (main spec §12), so a config that sets `server:`
fails to load on an older binary. `sdlc.example.yaml` therefore ships the block
**commented out**, matching how `database:` is already presented there, with
the loopback rule stated in the comment.

### 11.4 Docs to update in the same commit

- `docs/SPEC.md` §1.2 — "No web dashboard" becomes a pointer to this document
  and the localhost-only, no-auth boundary; §11 gains `serve`; §12 gains the
  `server:` block; §14 gains the three packages.
- `docs/running.md` — a §8 covering `sdlc serve`, what it does not protect
  against, and the `ssh -L` recipe for remote use.
- `README.md` — one line in the command list.

---

## 12. Testing strategy

`net/http/httptest` only. No browser automation, no new test dependencies, and
`go test ./... -race` stays green — which the SSE hub must be built for
(channels and a mutex-guarded client set; no shared maps).

### 12.1 `internal/server`

A fixture in the style of `newReviewEnv` (`internal/cli/review_test.go`): a
temp data dir, a `config.Default()` with one target, a real `store.Open`, and
jobs inserted directly. `httptest.NewServer(srv.Handler())`.

| Test | Asserts |
|---|---|
| `TestEveryRouteResolves` | Every pattern in the route table returns non-404 against the fixture; an unregistered path returns 404; a POST-only path returns 405 for GET. |
| `TestHostHeaderRejectsRebinding` | `Host: evil.example` → 421 and no body content from the app; `Host: localhost:<port>` and `127.0.0.1:<port>` → 200. |
| `TestCSRFRequiredOnEveryPost` | Table-driven over every POST route: missing token → 403, mismatched token → 403, no `approvals` row written in either case (asserted through the store, not the response). |
| `TestApprovalRowMatchesCLI` | A UI approve/reject/resume/cancel produces a row field-identical to the one `recordDecision` writes for the same input. |
| `TestStaleGateIsRefused` | Render the gate page, move the job in the DB, POST the form → 409 and zero rows. |
| `TestSecondDecisionIsRefused` | With a pending row present, a POST → 409 and still exactly one row. |
| `TestRejectRequiresReason` | Empty reason → 400, no row. |
| `TestCancelRequiresConfirmation` | Missing `confirm` → 400, no row. |
| `TestResumeValidatesTargetState` | `to=COMPLETED` → 400; `to=BUILDING` → row with `Reason: "BUILDING"`. |
| `TestServedFileNamesAreAllowlisted` | Table of hostile names — `../../sdlc.db`, `..%2f..%2fsdlc.db`, `....//`, `\..\..\sdlc.db`, `C:sdlc.db`, `log.txt:$DATA`, an absolute path, a name with a NUL — each 400 or 404, and the response never contains bytes from outside the job's logs dir. |
| `TestAgentTextIsEscaped` | A job title, a finding description and a log line each containing `<script>alert(1)</script>` appear escaped in the HTML and unescaped nowhere. |
| `TestTemplatesExecuteWithZeroValues` | Every template executes against a zero-value model without error — a missing field must fail in CI, not at 02:00 on a gate page. |
| `TestSSEDeliversSnapshotOnChange` | Connect, mutate a job's state in the DB, receive a `jobs` event naming the new state within 5s; assert `ping` frames arrive. |
| `TestSSEDropsSlowClient` | A client that never reads is dropped and the hub keeps serving others. |
| `TestShutdownIsClean` | `Shutdown` returns, the hub goroutine exits (checked via a `sync.WaitGroup`), and `-race` is clean. |
| `TestSubmitFromUICreatesSameJob` | `POST /submit` and `jobs.Submit` from the CLI path produce identical `store.Job` values apart from the id. |
| `TestNoEngineBanner` | With no lock file present, every page carries the "no engine appears to be running" banner (§13.8); with one present, it does not. |

### 12.2 `internal/diff`

- One golden test per case in §8.3, from `testdata/*.patch` fixtures generated
  once with real git and committed. Each asserts file count, paths, status,
  per-file `Adds`/`Dels`, hunk headers, and the exact `Line` sequence for at
  least one hunk.
- `TestSplitMatchesUnified` — the invariant of §8.4: for every fixture, the
  ordered non-filler add/del lines of the split view equal those of the
  unified view. This is what stops the two viewers from ever telling different
  stories about the same patch.
- `TestNoNewlineMarkerDoesNotCountAsALine` — line numbers and counts unaffected,
  in both the "after a deletion" and "after an addition" positions.
- `TestContentThatLooksLikeAHeader` — a patch whose hunk body contains
  `----foo`, ` diff --git a/x b/y` and `+@@ fake @@` parses as one file with
  the right line kinds.
- `TestMalformedHunkIsFlaggedNotDropped` — a hunk whose counts lie keeps its
  lines and sets `Malformed`.
- `FuzzParse` — stdlib fuzzing, seeded with the fixtures: never panics, never
  loops, and every returned `File` has a non-empty path. Kept in the default
  (seed-corpus) run so CI cost stays flat.

### 12.3 `internal/review` — the anti-drift tests

- **`TestMarkdownGoldenUnchanged`** — golden files for all five gates
  (spec/code/merge/release/hold), captured from the *current* implementation
  **before** the block-model refactor starts, asserted byte-for-byte after.
  This is what makes the refactor safe, and it must be the first commit.
- **`TestEveryBlockKindRenders`** — iterates a package-level list of all
  `BlockKind` values, executes the HTML renderer on one instance of each, and
  fails on an unhandled kind. Adding a block kind without an HTML case breaks
  the build; that is the mechanism that makes drift unrepresentable rather than
  merely testable.
- **`TestSpanKindsRoundTrip`** — the same for inline spans.
- **`TestBodyIsRenderedFromBlocks`** — `Doc.Body == renderMarkdown(Doc.Blocks)`
  for every fixture, so nobody reintroduces a string-building shortcut.

### 12.4 `internal/engine`

No new tests, because no changes. `go test ./internal/engine/... -race` passing
unmodified is itself an acceptance criterion (AC-2).

---

## 13. Open questions and risks

1. **The `internal/review` refactor is the biggest single risk.** It rewrites
   the internals of a file the terminal path depends on, to gain a property
   (§7.2) that is worth it. Mitigation: the golden test lands first, as its own
   commit, before a line of the refactor. If the golden test cannot be made to
   pass byte-for-byte, that is the signal to stop and reconsider Option A —
   not to update the golden file.
2. **Quoted-path ambiguity in `diff --git` headers** (§8.3 case 11) is not
   fully solvable from the header alone. The mitigation (prefer `---`/`+++`
   and rename lines) covers everything except mode-only and pure-rename chunks
   for paths containing ` b/`. Residual, documented, and vanishingly rare.
3. **`gitx.DiffPatchSince` truncates silently at 4 MiB.** `execx.RunCapture`'s
   `limitedWriter` stops writing at the cap and the caller cannot tell.
   Detection heuristic: `len(patch) >= 4<<20` ⇒ show the truncation banner.
   Fixing it properly means returning a truncated flag from `execx`/`gitx`,
   which is a change to two existing packages and is deliberately out of scope
   here. Flagged so the next person does not think the banner is guesswork.
4. **Two writers to SQLite.** Already the case for `sdlc approve`, but worth
   stating: the server opens the store the same way (`store.Open`, WAL,
   `MaxOpenConns=1`, `busy_timeout`) and must never hold a transaction across
   a request. A write that collides with the engine's waits out the busy
   timeout; that is the designed behaviour, not a bug to work around.
5. **Timezones under `ssh -L`.** The operator's browser may be in a different
   zone from the engine. All timestamps are emitted as RFC3339 UTC and
   localised in the browser (§9.3).
6. **Diff-page cost.** Rendering the gate page shells out to git. The 20s
   request cap and the 4-entry parsed cache (§6.1) bound it, but a repository
   whose `git diff` genuinely takes 30s will produce a slow page. Acceptable:
   the CLI has the same cost, under a 2-minute cap.
7. **Windows path handling for served files.** The allowlist-by-directory-
   membership rule (§14, AC-9) is chosen specifically because prefix
   comparison after `filepath.Clean` can be defeated on Windows by
   drive-relative names (`C:file`), alternate data streams (`name:$DATA`) and
   short names. Membership in `os.ReadDir` output cannot be.
8. **Engine liveness is inferred, not known.** The UI shows a banner when
   `orchestrator.lock_file` is absent: "no engine appears to be running —
   decisions are recorded and applied when one starts". The signal is weak
   (`engine.acquireLock` removes a stale lock at startup, so an absent lock
   after a crash is ambiguous) and the banner's wording must stay hedged.
   Cross-checking the newest event's age is a possible improvement; it is not
   in v1.
9. **Provenance of a decision.** A UI approval is indistinguishable from a CLI
   approval by design (§2.2). If provenance is ever wanted, `Approval.Note` is
   the wrong place — the note is handed to the agent verbatim — so it would
   mean a schema change. Recorded as a known limitation, not a to-do.
10. **Open question — does the gate page prefer the persisted document?**
    `review.Write` already wrote `<gate>.md` and `<gate>.diff` when the job
    parked. The spec chooses live `review.Render` for freshness, which costs
    the git calls. Reading the persisted patch instead would be faster and
    would work with the worktree deleted; it can also be older than the branch.
    Proposal: live render, fall back to the persisted files when the worktree
    is gone (which is exactly the case `renderDiff` already handles by saying
    so). Worth revisiting after the first real 3000-line patch.

---

## 14. Acceptance criteria

Each is checkable by a test or a grep. "Grep" criteria are worth a small
`TestHouseRules` in `internal/server` that walks the package source.

**Build and hygiene**

- **AC-1** — `go.mod` is byte-identical before and after the feature. `git diff
  --exit-code go.mod go.sum` on the feature branch shows no change.
- **AC-2** — `gofmt -l .` prints nothing; `go vet ./...` is clean;
  `go test ./... -race` is green on Windows, macOS and Linux.
- **AC-3** — `go build ./cmd/sdlc` produces one binary that serves the entire
  UI with the network disabled: no request in the rendered HTML resolves to a
  non-`/static/` external host. Grep the templates for `http://`, `https://`
  and `//` in `src`/`href` attributes; the only external URLs permitted are in
  human-readable text, never in a fetched resource.
- **AC-4** — `git diff --stat internal/engine/` is empty.

**Server behaviour**

- **AC-5** — With no `server:` block in the config, `sdlc validate` passes and
  `sdlc run`, `sdlc status` and `sdlc review` behave exactly as before
  (existing test suites unchanged and green).
- **AC-6** — `sdlc serve --addr 127.0.0.1:0` binds, prints the actual address,
  answers `GET /healthz` with `ok`, and shuts down within 5s of SIGINT with the
  hub goroutine exited.
- **AC-7** — A config with `server.listen: 0.0.0.0:7777` fails `Config.Validate`
  with an error naming both `loopback` and `ssh -L`.
- **AC-8** — Every route in §5 has a test; a path not in §5 returns 404.

**Safety**

- **AC-9** — For every hostile name in the §12.1 traversal table, the response
  is 400 or 404 and contains no byte from outside the job's logs/artifacts
  directory. Implementation is membership in `os.ReadDir`, not prefix
  comparison — greppable as the absence of `strings.HasPrefix` on a path in
  `safety.go`.
- **AC-10** — A request with `Host: evil.example` gets 421 and no application
  content, on every route including `/static/` and `/events/stream`.
- **AC-11** — Every state-changing route rejects a missing or mismatched CSRF
  token with 403 and writes no row. Grep: every `<form method="post">` in
  `templates/` contains the hidden `csrf` input.
- **AC-12** — The CSRF cookie is set `HttpOnly`, `SameSite=Lax`, `Path=/`, and
  is generated from `crypto/rand`.
- **AC-13** — POSTing a decision for a job whose state changed since render
  returns 409 and writes no row.
- **AC-14** — POSTing a decision when an unconsumed approval row already exists
  for that job and gate returns 409 and leaves exactly one row.
- **AC-15** — Every POST answers 303; no decision route ever renders a 200 body
  that a reload would re-submit.
- **AC-16** — `http.MaxBytesReader` is applied to every POST body.
- **AC-17** — `grep -rn "template.HTML\|template.JS\|template.URL"
  internal/server` matches only the asset-path helper, and that match is
  covered by a comment explaining why it is safe.

**Content fidelity**

- **AC-18** — `Doc.Body` is byte-identical to the pre-refactor output for all
  five gates (golden files).
- **AC-19** — Adding a `BlockKind` without an HTML case fails
  `TestEveryBlockKindRenders`.
- **AC-20** — The gate page for each of the five gates contains the document's
  title, every finding's severity and description, and the "what happens next"
  actions — asserted by substring against the same fixtures the markdown golden
  test uses.

**Diff**

- **AC-21** — All fourteen cases in §8.3 have a passing fixture test.
- **AC-22** — `TestSplitMatchesUnified` passes for every fixture.
- **AC-23** — `FuzzParse` runs its seed corpus with no panic and no timeout.
- **AC-24** — A 3000-line, 40-file patch renders in both views in under one
  second of server time (measured in a benchmark over a committed fixture,
  asserted as a ceiling, not a target).
- **AC-25** — Per-file `+n −m` counts equal `git diff --numstat` for every
  fixture — an independent oracle for the parser's counting.

**Live updates**

- **AC-26** — A state change in the database produces a `jobs` SSE event naming
  the new state within 5s.
- **AC-27** — `ping` frames arrive on an idle connection.
- **AC-28** — With the stream stopped, the page shows the stale state and the
  gate page's decision buttons are disabled — asserted by unit-testing the
  small pure staleness function in `app.js`'s Go counterpart, or, if the logic
  lives only in JS, by asserting the server never renders a page whose freshness
  claim is server-side. (The spec's position: no server-rendered "live" claim
  exists, so there is nothing to go stale on the server.)

**Submit**

- **AC-29** — `jobs.Submit` is the only place `store.CreateJob` is called
  outside tests. Grep: `grep -rn "CreateJob(" --include=*.go | grep -v _test`
  returns one non-test hit.
- **AC-30** — A UI submission and a CLI submission with the same inputs produce
  identical job rows apart from the id.
