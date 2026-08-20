# Contributing

sdlc is an orchestrator, not an agent framework. The design rule it exists to
enforce — Go decides *what* happens next, agents decide *how*, builds and tests
decide *whether*, guardrails decide what the automation is *allowed* to do —
is the thing to keep in mind before adding to it.

## Before you start

For anything beyond a small fix, open an issue first. It's cheap to agree on an
approach before writing code and expensive to unwind a large PR that took the
wrong one.

## Working in the repo

```bash
go build -o C:\tools\sdlc.exe .\cmd\sdlc   # /usr/local/bin (macOS), ~/.local/bin (Linux)
go vet ./...
go test ./... -race
```

Install into a directory on `PATH`, not the repo root — `/sdlc` and `/sdlc.exe`
are gitignored, and every script and doc here takes the tools path. See
[`docs/running.md`](docs/running.md) §1.

Tests need `git` on `PATH` (`internal/engine`'s end-to-end test drives a real
temp repo). They do not need a JVM, an Android SDK, a device, or an agent CLI —
the agent backends and gradle invocations are faked, and the SQLite driver is
pure Go, so there is no cgo toolchain to set up either. CI
(`.github/workflows/ci.yml`) runs the full suite on Windows, macOS, and Linux.

## What a good PR looks like

- **Doesn't move a decision from Go into a prompt.** Anything a prompt could
  lie about — whether a test is flaky, whether a diff touched test files,
  whether a reviewer modified the tree — is checked mechanically or it isn't
  checked. `TestFixTouchingTestsEscalates` and
  `TestIndependenceViolationFailsLoad` are two of those properties in
  executable form and should stay green.
- **Keeps every stop reachable and explained.** A new budget, cap or timeout
  lands the job in `ESCALATED`/`TIMED_OUT` with a reason and a way out
  (`sdlc resume`), never in a silent loop and never in an unrecoverable state.
- **Survives a kill -9.** Every state entry records the worktree HEAD, and a
  restart reconciles the worktree before re-running the state
  (`TestCrashResumeReconcilesWorktree`). New state must be recoverable the same
  way or it doesn't belong in a state handler.
- **Comes with a test that would have failed without the fix.**
- **Doesn't add a config knob for something with one reasonable default.**
  [`docs/SPEC.md`](docs/SPEC.md) is normative and explains most of the "why"
  behind a decision; check there before assuming something is an oversight. If
  a change alters behaviour the spec describes, the spec changes in the same PR.
- **Keeps release credentials out of this repo.** The release path runs through
  autoship, which holds them in the platform's native encrypted store. No code
  path here should read, log, or accept a credential.

## Reporting a security issue

Please don't open a public issue for a sandbox-escape or credential-handling
vulnerability — the agents run with permissions relaxed on the assumption that
they are confined to a job worktree. Email the maintainer directly (see the
GitHub profile on the commits) instead.
