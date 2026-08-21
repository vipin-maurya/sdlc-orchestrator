# sdlc-orchestrator

[![CI](https://github.com/vipinm/sdlc-orchestrator/actions/workflows/ci.yml/badge.svg)](https://github.com/vipinm/sdlc-orchestrator/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/vipinm/sdlc-orchestrator.svg)](https://pkg.go.dev/github.com/vipinm/sdlc-orchestrator)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**An issue goes in, a reviewed and tested merge comes out.** A Go engine that
drives coding agents through a real SDLC — plan, review, implement, review,
build, test, fix, merge, release — with the decisions that matter kept out of
the agents' hands.

```
issue → PLANNING → DESIGN_REVIEW → IMPLEMENTING → CODE_REVIEW → BUILDING →
TESTING (unit / UI on a leased device) → [FLAKE_CHECK → ANALYZING → FIXING]* →
FINAL_REVIEW → human merge approval → MERGING → human release approval →
RELEASING (autoship) → COMPLETED
```

Runs on **Windows, macOS, and Linux**, against any repo you can describe in
`sdlc.yaml`.

- **Spec:** [`docs/SPEC.md`](docs/SPEC.md) — normative; this repo implements it
- **Operating it:** [`docs/running.md`](docs/running.md)
- **Known gaps:** [`docs/review-findings.md`](docs/review-findings.md) — what an audit of the current build found, fixed and still open
- **Shipping the result:** [autoship](https://github.com/vipinm/autoship), which
  the `RELEASING` state invokes

## Why

An agent that can write the change can't be trusted to also decide whether the
change is good, whether a failing test is flaky, or whether it may edit the test
that caught it. Those are exactly the judgements that decide whether unattended
development is safe — so they don't live in a prompt.

**Design rule:** Go decides *what* happens next; agents (claude CLI, agy CLI)
decide *how* to solve their assigned problem; builds and tests decide *whether*
the result is acceptable; guardrails decide what the automation is *allowed* to
do. Agents never run git, never run Gradle, and never see release credentials.

## Shape

A single binary hosting one engine, plus a CLI that talks to it through SQLite.

```
sdlc run  ──▶  engine  ──▶  state handler  ──▶  agent CLI in a job worktree
                 │                          └▶  gradle / adb / autoship
                 ▼
              SQLite  ◀── submit / approve / reject / resume (any terminal)
```

`submit`/`approve`/`reject`/`cancel`/`resume` only write the database; a running
engine picks the change up on its next tick, so you can drive everything from a
second terminal while the first streams logs.

The load-bearing property is that no stop is silent: every round cap, retry cap,
timeout and policy violation lands in `ESCALATED`/`TIMED_OUT` with a reason and
a way out — never in a loop, never in a state a restart can't recover.

## Cross-platform, natively — not just "it happens to compile"

The engine is supervised by whatever the platform already has, always as a
user-scoped unit, because the agent CLIs read per-user credentials and the
builds need the user's Gradle and Android SDK caches.

| | Windows | macOS | Linux |
|---|---|---|---|
| Supervisor | Task Scheduler (at logon) | launchd (LaunchAgent, `KeepAlive`) | systemd (`--user` service) |
| Install script | [`scripts/register-task.ps1`](scripts/register-task.ps1) | [`scripts/install-launchd.sh`](scripts/install-launchd.sh) | [`scripts/install-systemd-service.sh`](scripts/install-systemd-service.sh) |

The scripts take the same flags as autoship's, and register a keep-alive
service rather than a timer — autoship is a one-shot, this is an engine. Details
in [`docs/running.md`](docs/running.md) §5.

## Quick start

```bash
git clone https://github.com/vipinm/sdlc-orchestrator.git && cd sdlc-orchestrator
go build -o C:\tools\sdlc.exe .\cmd\sdlc   # /usr/local/bin (macOS), ~/.local/bin (Linux)

cp sdlc.example.yaml sdlc.yaml               # edit targets/agents to taste
sdlc validate                                # config + environment doctor
sdlc validate --smoke                        # + one real headless prompt per backend (costs quota)

sdlc submit --target expensetracker --title "Fix transaction sync" --file issue.md
sdlc run --once                              # drain the runnable work, then exit
```

`run --once` is the important one: it does everything a persistent engine does
but stops when there is nothing runnable left, so you can watch a first job
transition by transition before handing it to a supervisor. Registering it to
run unattended, and promoting the release step from a dry run to a real one, is
covered end to end in [`docs/running.md`](docs/running.md).

## Commands

```
sdlc submit    --target <key> --title "..." (--body "..." | --file issue.md)
sdlc run       [--once]                 start the engine (foreground)
sdlc status    [JOB-ID]                 list jobs / show one job in detail
sdlc approve   <JOB-ID> [--note "..."]  approve the pending merge/release gate
sdlc reject    <JOB-ID> --reason "..." [--cancel]
sdlc cancel    <JOB-ID>
sdlc resume    <JOB-ID> [--to STATE]    clear an ESCALATED/TIMED_OUT/quota hold
sdlc events    <JOB-ID>                 event log (agent, model, tokens, exit codes)
sdlc logs      <JOB-ID> [--last]        artifact & log paths (tail the last log)
sdlc validate  [--smoke]                config + environment doctor
sdlc version
```

`--config` is global and precedes the subcommand; `SDLC_CONFIG` sets it once.

## What the orchestrator enforces (not the prompts)

- **Test-file policy** — a fix for a failure classified `code_bug` may not touch
  test files; the diff is checked mechanically and violations escalate with the
  change rolled back.
- **Flake detection is deterministic** — failing tests are re-run N times on the
  unmodified tree; the LLM never gets to call something "flaky".
- **Reviewers cannot write** — review states run with write tools disallowed at
  the CLI level *and* any tree modification is discarded and retried.
- **Backend independence** — `CODE_REVIEW`/`FINAL_REVIEW` must resolve to a
  different backend than `IMPLEMENTING` or the engine refuses to start.
- **Budgets everywhere** — per-loop round caps, per-state timeouts, a per-job
  agent-invocation budget, and a wall-clock job budget; every exhaustion lands
  in `ESCALATED`/`TIMED_OUT` with a reason, never a silent loop.
- **Quota ≠ failure** — rate-limited agent runs suspend the job
  (`BLOCKED_ON_QUOTA`) with automatic resume; they don't burn retry budgets.
- **Crash resume** — every state entry records the worktree HEAD; on restart the
  worktree is reconciled (`reset --hard` + `clean`) before the state re-runs.
  Kill the engine at any point; nothing corrupts.

## Layout

```
cmd/sdlc              entrypoint
internal/
  cli/                subcommands
  engine/             scheduler, state machine, handlers
  agent/              claude / agy / exec backend adapters
  config/             sdlc.yaml schema + validation
  store/              SQLite (jobs, events, approvals)
  gitx/               worktrees, commits, rebase/merge
  execx/              streaming command runner
  resource/           gradle slots + device pool + emulator boot
  guard/              test-file globs, policy checks
  artifact/           spec/plan/review/analysis JSON schemas
  prompt/             template rendering + hashing
prompts/              embedded default per-state prompts
scripts/              supervisor install scripts (Task Scheduler / launchd / systemd)
docs/SPEC.md          the specification this repo implements
docs/running.md       installing, validating, and running it unattended
docs/review-findings.md  audit of the current build: fixed, open, and verified
```

## Three things worth knowing before changing it

1. **The spec is normative.** [`docs/SPEC.md`](docs/SPEC.md) defines the state
   machine, the artifact schemas and the config surface. Behaviour that
   contradicts it is a bug in the code, not in the spec — and a deliberate
   change updates both in the same commit.
2. **Nothing an agent says is trusted.** Flakiness, policy compliance and review
   independence are all decided by the orchestrator; the agent's own claim about
   any of them is discarded.
3. **The release path is a black box on purpose.** `RELEASING` shells out to
   autoship and reads an exit code. Credentials, Play API access and the halt
   latch live over there, so nothing in this repo can leak them.

## Build and test

```bash
go vet ./...
go test ./... -race
go build -o C:\tools\sdlc.exe .\cmd\sdlc   # /usr/local/bin (macOS), ~/.local/bin (Linux)
```

Tests need `git` on `PATH`; they do not need a JVM, an Android SDK, a device, or
an agent CLI. The SQLite driver is pure Go, so no cgo toolchain either. CI runs
the full suite on all three OSes — see the badge above.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) before sending a PR.

## License

[MIT](LICENSE)
