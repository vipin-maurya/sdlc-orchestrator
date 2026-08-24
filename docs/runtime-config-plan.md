# Runtime-configurable settings — implementation plan

## What landed already (`eef4f75`, `internal/config/live.go`)

- `config.Parse(raw []byte, path string) (*Config, error)` — the exact
  pipeline `Load` runs a file through (env expand, strict decode over
  `Default()`, computed defaults, `Validate`), callable on text that has not
  touched disk. `Load` is now a thin wrapper: read the file, call `Parse`.
- `config.RestartRequiredFields(old, new *Config) []string` — names exactly
  `orchestrator.data_dir`, `orchestrator.lock_file`, `database` (the whole
  block), `server.listen`. Nothing else is ever restart-required; every other
  field is read fresh at its point of use throughout the codebase, so a
  change to it is live the moment the pointer swaps.
- `config.Live` — `atomic.Pointer[Config]` wrapper.
  - `Get() *Config` — lock-free, safe from any goroutine.
  - `Apply(raw []byte) ReloadResult` — validates first; only on success, and
    only if `RestartRequiredFields` against the current live config is empty,
    does it back up the current file, write `raw` via temp-file-and-rename,
    and swap. An invalid or restart-only edit never reaches disk.
  - `Reload() ReloadResult` — the same check against whatever is already on
    disk, for edits Apply didn't make (a second process, a text editor).
  - `Watch(ctx, interval, *log.Logger)` — polls mtime+size, calls `Reload`
    only when they moved, logs what happened.
  - `NewLive(initial *Config) *Live` — wraps an already-`Load`ed config;
    does not re-validate.
- `writeWithBackup` — backs up the replaced file into
  `<dir of config>/.sdlc-config-history/<UTC timestamp>-<basename>` before
  overwriting, pruned to the newest 50 (`maxHistoryFiles`).

All of it is tested under `-race`, including a concurrent-writers-plus-a-
watcher-plus-readers test, and both refusal paths (restart-only, invalid) are
proven by reverting the check and watching the test that names it fail.

**Nothing outside `internal/config` uses `Live` yet.** That is this plan.

## Why two processes matter here

`sdlc run` (the engine) and `sdlc serve` are separate OS processes that only
ever communicate through the SQLite database and the filesystem — proven
architecture, unchanged by this feature. **The actual work — agent
invocations, builds, merges — happens in `sdlc run`, not in `sdlc serve`.**
A config edit made through the browser is saved to disk by the `serve`
process; the `run` process picks it up by noticing the file changed, on its
own `Watch` loop, exactly like a person editing the file by hand would cause
it to. There is no cross-process signaling beyond the file itself. Both
processes construct their own independent `*config.Live` and their own
`Watch` goroutine.

This is why the plan below touches three things independently: the engine's
config source, the server's config source, and the server's new write path —
the write path only ever writes a file; making the change *live* is Watch's
job in both processes, symmetrically.

## Owner A — `internal/engine`: mechanical rewrite + two real subtleties

Files: `internal/engine/*.go` (all of them) and their `_test.go` siblings.
Do not touch `internal/config`, `internal/server`, `internal/cli`.

**1. `Engine.cfg` changes type from `*config.Config` to `*config.Live`.**
Every read site (`e.cfg.X`, currently ~58 of them across engine.go,
handlers.go, gate.go, jobctx.go, progress.go, states.go) becomes
`e.cfg.Get().X`. This is mechanical but must be exhaustive — grep for
`e\.cfg\.` after the change and confirm zero remain that are not `.Get().`.
`engine.New(cfg *config.Live, st *store.Store, logger *log.Logger) *Engine`
— signature changes; every test constructing an `Engine` needs
`config.NewLive(cfg)` wrapped around whatever `*config.Config` it already
builds.

**2. The parallel-job semaphore is a fixed-capacity channel today**
(`sem: make(chan struct{}, cfg.Orchestrator.MaxParallelJobs)`, `engine.go`
line ~55), sized once at construction. A live change to
`orchestrator.max_parallel_jobs` must actually take effect, and a Go channel
cannot be resized. Replace it with a small mutex-guarded counting semaphore:

```go
// resizableSem is a counting semaphore whose capacity can change while
// acquires and releases are in flight — a fixed-size chan struct{} cannot be
// resized, and max_parallel_jobs is a runtime-configurable setting.
type resizableSem struct {
	mu       sync.Mutex
	capacity int
	inUse    int
}

func (s *resizableSem) SetCapacity(n int) {
	s.mu.Lock()
	s.capacity = n
	s.mu.Unlock()
}

func (s *resizableSem) TryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inUse >= s.capacity {
		return false
	}
	s.inUse++
	return true
}

func (s *resizableSem) Release() {
	s.mu.Lock()
	s.inUse--
	s.mu.Unlock()
}
```

Replace `select { case e.sem <- struct{}{}: default: continue }` with
`if !e.sem.TryAcquire() { continue }`, and the release defer's `<-e.sem`
with `e.sem.Release()`. Call `e.sem.SetCapacity(e.cfg.Get().Orchestrator.MaxParallelJobs)`
once per tick, before the dispatch loop, so a live edit is visible within one
tick. Add a test that shrinks capacity mid-flight (some jobs already holding
a slot) and confirms in-flight jobs are not evicted, only new dispatch is
capped, plus a `-race` test acquiring/releasing/resizing concurrently.

**3. The poll ticker is created once from `PollInterval`** (`ticker :=
time.NewTicker(e.cfg.Orchestrator.PollInterval.D())`, `engine.go` line ~98)
and never revisited. After each tick, compare the live config's
`PollInterval` to what the ticker is currently running at (track the last
value used) and call `ticker.Reset(d)` when it changed. Untested silently
stale intervals are exactly the class of bug this whole codebase's review
history has been about — write a test proving a mid-run interval change is
picked up (inject a fake clock or assert on `ticker`'s observable behavior
via a small seam; use your judgement on the cleanest testable shape, but do
not leave this unverified).

**4. Start the watch loop.** `Engine.Run` does not start `Watch` — that is
the CLI's job (Owner C), since `Engine.New` takes an already-constructed
`*config.Live`. Do not add a `Watch` call inside `engine`.

Run `go test ./internal/engine/... -race -count=1` (it's the slow suite,
budget for it) before reporting done.

## Owner B — `internal/server`: mechanical rewrite + the write path + UI

Files: `internal/server/*.go`, `internal/server/templates/*.html`,
`internal/server/static/*`. Do not touch `internal/config`, `internal/engine`,
`internal/cli`.

**1. Same mechanical change.** `Server.cfg` from `*config.Config` to
`*config.Live`; every `s.cfg.X` (~25 sites across pages.go, actions.go,
render.go, diffpage.go, live.go, server.go, safety.go) becomes
`s.cfg.Get().X`. `server.New(cfg *config.Live, st *store.Store, lg
*log.Logger, verbose bool) (*Server, error)` — signature changes.

**2. The existing read-only `/config` page stays** — it renders the
*decoded, redacted* effective config (`redactedConfigYAML`), which is a
different and still-useful view (resolved values, secrets hidden). Add an
edit surface alongside it:

- The editor operates on the **raw file bytes**, not the redacted/decoded
  struct. This is deliberate and safe: env-var expansion (`${VAR}`) happens
  on the raw text *before* parsing (`config.expandEnv`, called inside
  `Parse`), so the file on disk is meant to hold `${VAR}` placeholders, never
  literal secret values — that is the existing, documented convention
  (`sdlc.example.yaml`'s own comment). The editor therefore never needs to
  redact anything it round-trips; state this explicitly in the page's copy
  ("this shows exactly what's on disk — keep secrets in environment
  variables, not literal values, or they will be visible here").
  Read the raw bytes with `os.ReadFile(s.cfg.Get().Path)` for the GET.
- `GET /config` (extend the existing handler): also render a `<textarea>`
  pre-filled with the raw file text, a Save button, and — when the last save
  attempt failed — the error and the text the user submitted (not what's on
  disk), so a rejected save never loses the user's edit.
- `POST /config`: read the form body (`s.cfg.Get().Path`'s new content),
  call `s.cfg.Apply([]byte(body))`. On `Applied`: 303 back to `/config` with
  a success banner (query param or flash — match whatever pattern
  `actions.go` already uses for post-decision banners, if any; otherwise a
  `?saved=1` query flag the template checks). On refusal (`Err != nil` or
  `RestartFields` non-empty): **do not 303** — re-render `/config` (200) with
  the specific error or the exact restart-required field names, and the
  submitted text still in the textarea. This is a deliberate, one-off
  exception to "every POST answers 303" (spec AC-15): AC-15 is about not
  re-posting a *decision* on reload, and a multi-hundred-line document
  failing validation is a form re-render, not a decision — losing the user's
  edit on a typo would be actively hostile. Say so in a comment at the
  handler, so it does not read as an overlooked deviation on the next pass.
  Must go through `requireCSRF` like every other POST here.
- History: `GET /config/history` lists
  `<dir of s.cfg.Get().Path>/.sdlc-config-history/` — reuse `safeName`'s
  membership-based resolution for any per-entry file access (never a
  hand-rolled path join, that pattern is exactly what this codebase's own
  review caught once already). `GET /config/history/{name}` shows one
  version's raw text read-only, with a "load into editor" link that
  pre-fills the `/config` textarea with that version's content via a query
  param or session-scoped stash — it must NOT save on its own; restoring an
  old version still requires an explicit second Save, matching the existing
  "no one-click destructive action" pattern (`cancel` requires
  `confirm=yes`).

**3. Test it against real concurrency, not just the handler in isolation:**
a test that POSTs an edit, then asserts `s.cfg.Get()` reflects it
immediately (no restart, no second request); a test that a restart-only edit
is refused and the file on disk is unchanged; a test that an invalid edit
re-renders with the submitted text intact; a test that CSRF is required on
`POST /config` exactly like every other POST (extend
`TestCSRFRequiredOnEveryPost`'s table rather than writing a parallel check).
Update `TestHouseRules`/`TestEveryRouteResolves`/`TestNoStubsRemain`-style
package-wide tests if the new routes need entries.

Run `go test ./internal/server/... -race -count=1` before reporting done.

## Owner C — `internal/cli` + docs

Files: `internal/cli/cli.go`, `internal/cli/serve.go`, `docs/SPEC.md`,
`docs/running.md`, `README.md`. Do not touch `internal/engine`,
`internal/server`, `internal/config`.

**1. `cmdRun`** (the `sdlc run` entry point) and **`cmdServe`** construct
`live := config.NewLive(cfg)` from the already-`Load`ed config, then start
`go live.Watch(ctx, configWatchInterval, logger)` before constructing the
`Engine`/`Server` with `live` instead of the bare `*config.Config`. Every
other command (`submit`, `approve`, `reject`, `resume`, `cancel`, `status`,
`validate`, ...) is one-shot and keeps using the plain `*config.Config` from
`Load` unchanged — do not touch those command functions' signatures.

**2. `configWatchInterval` is a small constant (2s), not itself configured**
by anything in `sdlc.yaml` — the poll cadence for noticing a file change is
infrastructure for the config system, not a setting the config system
governs; making it configurable would be a needless regress (what governs
the interval that governs the interval...). Say this in a one-line comment
where the constant is defined so nobody "fixes" it into a new config key
later.

**3. Docs.** `docs/SPEC.md` needs: a note wherever `server.listen` / config
loading is described that `sdlc run` and `sdlc serve` each independently
watch the config file and reload it (with the restart-required exceptions
named), and that `sdlc serve`'s `/config` page can write to it. `docs/
running.md` needs an operational section: what "runtime configurable" means
in practice, the four restart-required settings and why, that a rejected
save never touches disk, that history backups live in
`.sdlc-config-history/` next to the config file (worth a `.gitignore`
mention if the config file itself is typically checked in — check whether
`.gitignore` already excludes it or needs an entry), and — importantly —
that the raw editor shows literally what's on disk, so the `${VAR}`
convention for secrets is not optional once the config is browser-editable.
Do not claim a test exists unless you have checked it does; this document's
own history has three prior instances of a comment or doc citing a
nonexistent test.

Run `gofmt -l .`, `go build ./...` after the docs+CLI change (docs changes
don't need `go test`, but the CLI wiring does — run
`go test ./internal/cli/... -race -count=1`).

## Rules for all three owners

- Zero new `go.mod` entries.
- `gofmt -l <your files>` and `go vet ./...` clean before reporting done.
- Do not run the full `go test ./...` — the engine suite alone takes
  minutes under `-race`; run only your own package(s).
- Do not commit or push. Leave work in the working tree.
- Comments explain *why*; never claim a test or behavior exists without
  having checked it does, in this codebase above all others.
- Report exactly what you changed, your own verification output, and
  anything you could not do with the reason — the standard this whole
  session has held every prior piece of work to.
