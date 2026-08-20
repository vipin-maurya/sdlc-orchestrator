# SDLC Orchestrator — Specification

Version: 1.0
Status: Approved for implementation
Date: 2026-08-20

---

## 1. Overview

A Go binary (`sdlc`) that drives autonomous software-engineering jobs end to end:
issue → spec → plan → review → implementation → code review → build → test →
fix loops → final review → human-gated merge → human-gated release (via the
existing `autoship` CLI) → published build.

**Core design rule (normative):**

> Go decides *what* happens next. Agents decide *how* to solve their assigned
> problem. Automated builds/tests decide *whether* the result is acceptable.
> Guardrails decide what the automation is *allowed* to do.

Agents never control workflow. The orchestrator is the only component that
transitions state, runs builds/tests, touches git history, or invokes the
release path.

### 1.1 Goals

- G1: Fully autonomous pipeline from submitted issue to shipped build, with
  exactly two human decisions: approve merge, approve release.
- G2: Repo-agnostic. Every target repository is a config entry; ExpenseTracker
  (`C:\Users\vm899\repos\ExpenseTracker`) is the reference target.
- G3: Mixed agent backends per state: `claude` CLI (Opus/Sonnet) and `agy`
  CLI (Gemini), selected per state via config; orchestrator does not care which
  model executes a task.
- G4: Crash-safe. The process can be killed at any point and resumed without
  corrupting a job; git is the ground truth for work state, SQLite for
  workflow state.
- G5: Everything that can vary is a self-explanatory config key with a sane
  default. No hard-coded models, limits, commands, paths, or prompts.

### 1.2 Non-goals (v1)

- No container/VM sandboxing. Isolation = per-job git worktree + CLI-level
  tool restrictions + orchestrator-enforced diff policies (host execution).
- No web dashboard, no Jira/GitHub integration. CLI is the only interface.
- No PR flow. Merges are local (rebase + merge to the target's default
  branch, push if a remote exists).
- No agent-run builds or tests. Gradle/adb are invoked by the orchestrator only.
- No multi-machine operation. Single host, single orchestrator process
  (enforced by a lock file).

### 1.3 Environment (reference)

| Tool | Location | Notes |
|---|---|---|
| Go 1.26 | `go` on PATH | build toolchain |
| git 2.45 | on PATH | worktrees, merges |
| claude CLI | `C:\Users\vm899\.local\bin\claude.exe` | logged-in Max subscription |
| agy CLI 1.1.12 | `C:\Users\vm899\AppData\Local\agy\bin\agy.exe` | logged-in cached auth |
| adb | `C:\Users\vm899\platform-tools\adb.exe` | device pool |
| autoship | built from `C:\Users\vm899\repos\autoship` | release path |
| Target | `C:\Users\vm899\repos\ExpenseTracker` | Android app, `gradlew.bat` |

OS is Windows; all subprocess handling, paths, and scripts must be
Windows-first but portable (use `os/exec`, `filepath`, no shell-isms).

---

## 2. Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  sdlc CLI (cobra-style subcommands)                         │
│  submit · run · status · approve · reject · cancel ·        │
│  resume · events · logs · validate · version                │
└──────────────────────────┬──────────────────────────────────┘
                           │ (same binary; `run` hosts the engine,
                           │  other commands write/read the DB)
┌──────────────────────────▼──────────────────────────────────┐
│  ENGINE (only inside `sdlc run`)                            │
│  ├── Scheduler: picks runnable jobs, bounded worker pool    │
│  ├── StateMachine: transition table, per-state handlers     │
│  ├── Store: SQLite (WAL, single writer goroutine)           │
│  ├── GitManager: branch/worktree/commit/rebase/merge        │
│  ├── AgentRunner: backend adapters (claude, agy, exec)      │
│  ├── ExecRunner: gradle/adb/autoship with streaming logs    │
│  ├── ResourceManager: gradle-slot + device leases           │
│  ├── ArtifactStore: per-job artifact dir + hashes           │
│  └── Guardrails: config validation, diff policy, budgets    │
└─────────────────────────────────────────────────────────────┘
```

Trust boundaries: agents run only inside their job worktree with restricted
tools; the orchestrator process invokes gradle/autoship; release credentials
live in autoship's encrypted store and are never readable by agents.

---

## 3. State machine (normative)

### 3.1 States

Pipeline states (in nominal order):

`CREATED → PLANNING → DESIGN_REVIEW → IMPLEMENTING → CODE_REVIEW → BUILDING →
TESTING → FINAL_REVIEW → AWAITING_MERGE_APPROVAL → MERGING →
AWAITING_RELEASE_APPROVAL → RELEASING → COMPLETED`

Off-nominal states: `FLAKE_CHECK`, `ANALYZING`, `FIXING`,
`BLOCKED_ON_QUOTA`, `ESCALATED`, `CANCELLED`, `TIMED_OUT`, `FAILED`.

Terminal states: `COMPLETED`, `CANCELLED`, `FAILED`.
`ESCALATED` and `TIMED_OUT` are durable holds: a human can `sdlc resume`
(re-enter a configured state) or `sdlc cancel`.

### 3.2 Transition table

| From | Trigger | To |
|---|---|---|
| CREATED | scheduler picks up job, worktree provisioned | PLANNING |
| PLANNING | spec.json + plan.json valid | DESIGN_REVIEW |
| PLANNING | agent failure > `limits.max_agent_retries` | ESCALATED |
| DESIGN_REVIEW | review.json has zero `blocker` findings | IMPLEMENTING |
| DESIGN_REVIEW | blockers present, rounds < `limits.max_design_review_rounds` | PLANNING |
| DESIGN_REVIEW | blockers present, rounds exhausted | ESCALATED |
| IMPLEMENTING | non-empty diff + implementation.json valid; orchestrator commits | CODE_REVIEW |
| CODE_REVIEW | zero blockers | BUILDING |
| CODE_REVIEW | blockers, rounds < `limits.max_code_review_rounds` | FIXING |
| CODE_REVIEW | rounds exhausted | ESCALATED |
| BUILDING | all build commands exit 0 | TESTING |
| BUILDING | build fails | ANALYZING |
| TESTING | unit (and enabled UI) tests pass | FINAL_REVIEW |
| TESTING | tests fail | FLAKE_CHECK |
| FLAKE_CHECK | reruns inconsistent (flaky) & retries < `limits.max_flake_retries` | TESTING |
| FLAKE_CHECK | reruns inconsistent & retries exhausted | ESCALATED |
| FLAKE_CHECK | reruns consistently fail | ANALYZING |
| ANALYZING | classification `code_bug` or `test_bug`, fixes < `limits.max_fix_attempts` | FIXING |
| ANALYZING | classification `environment` | ESCALATED |
| ANALYZING | classification `unknown` or fix attempts exhausted | ESCALATED |
| FIXING | diff passes test-file policy; orchestrator commits | BUILDING |
| FIXING | diff violates test-file policy | ESCALATED |
| FINAL_REVIEW | zero blockers | AWAITING_MERGE_APPROVAL |
| FINAL_REVIEW | blockers, fix attempts remain | FIXING |
| FINAL_REVIEW | fix attempts exhausted | ESCALATED |
| AWAITING_MERGE_APPROVAL | `sdlc approve` | MERGING |
| AWAITING_MERGE_APPROVAL | `sdlc reject` | FIXING (reason attached) or CANCELLED (`--cancel`) |
| MERGING | rebase + merge + (optional push) succeed | AWAITING_RELEASE_APPROVAL |
| MERGING | rebase conflict | ESCALATED |
| AWAITING_RELEASE_APPROVAL | `sdlc approve` | RELEASING |
| AWAITING_RELEASE_APPROVAL | `sdlc reject` | COMPLETED (merged, not shipped) |
| RELEASING | ship command exits 0 | COMPLETED |
| RELEASING | non-zero, retries < `limits.max_release_retries` | RELEASING (retry) |
| RELEASING | retries exhausted | ESCALATED |
| any agent state | quota/rate-limit detected | BLOCKED_ON_QUOTA (auto-resumes to the same state after `resume_after`) |
| any state | `sdlc cancel` | CANCELLED |
| any state | job age > `limits.max_job_duration` | TIMED_OUT |

Notes (normative):

- **Fix loop always re-enters BUILDING**, never TESTING directly — build once,
  then fan out.
- **FLAKE_CHECK is deterministic, no agent.** The orchestrator re-runs the
  failing test tasks `limits.flake_rerun_count` times on the *unmodified*
  tree. Any pass among reruns ⇒ flaky. All fail ⇒ real failure ⇒ ANALYZING.
- **ANALYZING never gets to declare "flaky"** — its schema only allows
  `code_bug | test_bug | environment | unknown`.
- **Approval is computed, not declared:** a review state passes iff its
  `review.json` contains zero findings with `severity: blocker`. The agent
  emits findings; the orchestrator derives the verdict.
- **AWAITING_* states are durable.** The engine parks them; restart-safe;
  approval arrives via the DB from a separate `sdlc approve` invocation.
- BLOCKED_ON_QUOTA does **not** consume retry/fix budgets.

### 3.3 Per-state counters

Independent counters per job, persisted (not one shared `retry_count`):
`design_review_rounds`, `code_review_rounds`, `fix_attempts`,
`flake_retries`, `release_retries`, and per-state `agent_retries`
(reset on state change). Budgets are configured under `limits.*`.

### 3.4 UI testing & devices

If `targets.<t>.ui_test.enabled: true`, TESTING additionally runs the UI test
command while holding a **device lease** from the ResourceManager:

- Pool = `resources.devices.serials` (explicit) ∪ discovered via
  `adb devices` when `resources.devices.discover: true`.
- If pool is empty and `resources.devices.boot_emulator.enabled: true`, the
  orchestrator boots the configured AVD (`emulator -avd <name>`), waits for
  `sys.boot_completed`, uses it, and (config) keeps or kills it.
- If no device is obtainable within `resources.devices.acquire_timeout`,
  behavior follows `targets.<t>.ui_test.on_no_device`:
  `skip` (record, continue) | `fail` (→ ANALYZING) | `wait`.
- The leased serial is passed to the UI test command via env
  `ANDROID_SERIAL=<serial>`.

Gradle concurrency: all BUILDING/TESTING/FLAKE_CHECK executions hold one of
`resources.gradle_slots` leases. If `targets.<t>.gradle.isolated_user_home:
true`, each job runs with `GRADLE_USER_HOME=<data_dir>/gradle-homes/<job>`.

---

## 4. Data model (SQLite)

Driver: `modernc.org/sqlite` (pure Go, no cgo). `PRAGMA journal_mode=WAL`,
`busy_timeout` from config. All writes funneled through a single goroutine.

```sql
CREATE TABLE jobs (
  id            TEXT PRIMARY KEY,      -- e.g. JOB-007
  target        TEXT NOT NULL,         -- key into targets config
  issue_title   TEXT NOT NULL,
  issue_body    TEXT NOT NULL,
  branch        TEXT NOT NULL,         -- e.g. sdlc/JOB-007
  worktree_path TEXT NOT NULL,
  state         TEXT NOT NULL,
  prev_state    TEXT,                  -- state to return to from BLOCKED_ON_QUOTA / holds
  state_entered_at TEXT NOT NULL,
  head_sha      TEXT,                  -- worktree HEAD recorded at state entry
  resume_after  TEXT,                  -- for BLOCKED_ON_QUOTA
  hold_reason   TEXT,                  -- ESCALATED / TIMED_OUT / rejection reason
  counters      TEXT NOT NULL,         -- JSON {design_review_rounds:0,...}
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE TABLE events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id      TEXT NOT NULL,
  state       TEXT NOT NULL,
  kind        TEXT NOT NULL,   -- enter|agent_run|exec_run|transition|approval|error|note
  agent       TEXT,            -- agent key (opus/sonnet/gemini) if any
  backend     TEXT, model TEXT, effort TEXT,
  binary_version TEXT,         -- CLI --version output, recorded per run
  prompt_hash TEXT,            -- sha256 of fully rendered prompt
  input_ref   TEXT,            -- artifact-relative path
  output_ref  TEXT,            -- artifact-relative path (stdout envelope, logs)
  exit_code   INTEGER,
  duration_ms INTEGER,
  head_before TEXT, head_after TEXT,
  tokens_in   INTEGER, tokens_out INTEGER,  -- best-effort from CLI JSON
  detail      TEXT,            -- free-form JSON
  created_at  TEXT NOT NULL
);

CREATE TABLE approvals (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id    TEXT NOT NULL,
  gate      TEXT NOT NULL,     -- merge|release
  decision  TEXT NOT NULL,     -- approve|reject
  reason    TEXT,
  cancel    INTEGER NOT NULL DEFAULT 0,
  consumed  INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
```

Agent transcripts/stdout are **never** stored inline — they are written to the
artifact store; events hold paths + hashes.

---

## 5. Artifacts

Per-job directory: `<orchestrator.data_dir>/jobs/<job-id>/`

```
issue.md            the submitted issue (input)
artifacts/
  spec.json         PLANNING output
  plan.json         PLANNING output
  design_review.r1.json ...      DESIGN_REVIEW rounds
  implementation.json            IMPLEMENTING output
  code_review.r1.json ...
  analysis.a1.json ...
  fix.f1.json ...
  final_review.json
logs/
  <seq>_<state>_<agent-or-exec>.log   raw stdout/stderr per run
prompts/
  <seq>_<state>.md                    fully rendered prompt actually sent
```

### 5.0 Exchange directory

Agents read inputs and write outputs through `.sdlc/` inside their worktree
(portable across backends — no extra directory-access flags needed):

- Before each agent run the orchestrator recreates `.sdlc/` and stages inputs
  under `.sdlc/context/` (spec.json, plan.json, implementation.json,
  analysis.json — whatever the state needs; for CODE_REVIEW/FINAL_REVIEW also
  `diff.patch`, the full diff vs the job base, so reviewers never need shell
  access to see the change).
- The agent writes its output artifact(s) to `.sdlc/<name>.json`; the
  orchestrator harvests them into the artifact store after validation.
- `.sdlc/` is added to the target repo's `.git/info/exclude`, so it never
  appears in status, diffs, or commits.

### 5.1 Schemas (validated by the orchestrator before any transition)

All artifacts share an envelope: `{"schema": "<name>/1", ...}`. Validation is
structural (required fields, enum values) in Go — no external JSON-schema lib
needed, but each schema has one validator function + tests.

**spec/1** (`spec.json`)
```json
{ "schema": "spec/1", "issue_summary": "...", "approach": "...",
  "affected_files": ["app/src/..."], "acceptance_criteria": ["..."],
  "error_paths": ["..."], "out_of_scope": ["..."],
  "compatibility_concerns": ["..."] }
```
Required: every key; `affected_files` and `acceptance_criteria` non-empty.
This is what forces Opus to be explicit enough for a Gemini implementer.

**plan/1** (`plan.json`)
```json
{ "schema": "plan/1",
  "steps": [ { "id": "S1", "description": "...", "files": ["..."],
               "verification": "..." } ],
  "risks": ["..."] }
```

**review/1** (`design_review.*.json`, `code_review.*.json`, `final_review.json`)
```json
{ "schema": "review/1", "reviewed": "spec|diff|final",
  "findings": [ { "id": "F1", "severity": "blocker|major|minor|nit",
                  "file": "optional", "description": "...",
                  "recommendation": "..." } ],
  "summary": "..." }
```
Verdict = derived: pass ⇔ zero `blocker`.

**implementation/1** (`implementation.json`, `fix.*.json`)
```json
{ "schema": "implementation/1", "summary": "...",
  "files_changed": ["..."], "tests_added_or_changed": ["..."],
  "test_change_requested": null }
```
`test_change_requested` (string reason) is the *only* legitimate way for a
FIXING agent to ask for a test modification when the classification was
`code_bug`; it routes to ESCALATED, never silently applied.

**analysis/1** (`analysis.*.json`)
```json
{ "schema": "analysis/1",
  "classification": "code_bug|test_bug|environment|unknown",
  "reasoning": "...", "failing_targets": ["..."], "fix_hint": "..." }
```

---

## 6. Agent execution

### 6.1 Contract

```go
type AgentResult struct {
    ExitCode   int
    Stdout     string        // raw; also persisted to logs/
    Model      string        // as reported by the CLI envelope, if available
    TokensIn, TokensOut int  // best-effort
    QuotaHit   bool          // backend adapter classified a rate/quota error
}
Run(ctx, AgentSpec{Backend, Model, Effort, Prompt, Cwd, Timeout,
                   AllowedTools, DisallowedTools}) (AgentResult, error)
```

**Exit code is a hint; the filesystem is truth.** After every agent run the
state handler checks post-conditions itself:

| State | Post-condition |
|---|---|
| PLANNING | `spec.json` + `plan.json` exist and validate |
| DESIGN_/CODE_/FINAL_REVIEW | round's `review.json` validates; **worktree diff must be empty** (reviewer wrote files ⇒ hard failure) |
| IMPLEMENTING | `implementation.json` validates; `git status` shows a non-empty diff |
| ANALYZING | `analysis.json` validates; worktree diff empty |
| FIXING | `fix.json` validates; diff non-empty; diff passes test-file policy |

Post-condition failure ⇒ retry the agent (up to `limits.max_agent_retries`,
with the failure appended to the prompt), then ESCALATED. Empty stdout with
satisfied post-conditions is a success (tolerates the known agy stdout bug).

After IMPLEMENTING/FIXING post-conditions pass, the **orchestrator** runs
`git add -A && git commit` in the worktree with message
`[sdlc <job>] <state>: <summary from artifact>`. Agents are instructed not to
run git; any commits they do make are tolerated (HEAD movement is recorded).

### 6.2 Backend adapters

**claude** (headless):
```
claude -p --output-format json --model <model>
       --permission-mode bypassPermissions [--allowedTools ...]
       [--disallowedTools ...]        (prompt delivered via stdin)
```
- The prompt is ALWAYS piped via stdin: `--allowedTools`/`--disallowedTools`
  are variadic and would swallow a trailing positional prompt argument.
- cwd = job worktree. `--output-format json` envelope is parsed for
  `is_error`, `result`, `usage` (tokens), `modelUsage`.
- Review/analyze states default to `--disallowedTools Edit,NotebookEdit,Bash`
  (config-overridable per state) — Write stays allowed because the reviewer
  must emit its `.sdlc` output file; source-tree immutability is enforced by
  the mechanical clean-tree check (§13.2), and the diff is pre-staged so Bash
  is unnecessary.
- Quota classification: envelope error text / nonzero exit matching
  `backends.claude.quota_error_patterns` (regex list, default includes
  `rate.?limit|usage.?limit|overloaded|quota`).

**agy** (headless):
```
agy -p <prompt> --output-format json --model <model> --effort <effort>
    --print-timeout <dur> [--dangerously-skip-permissions]
```
- cwd = job worktree. Known constraints handled:
  - stdout-empty bug ⇒ rely on post-conditions (6.1).
  - shell soft-deny ⇒ `backends.agy.permission_mode: skip` (the default)
    passes `--dangerously-skip-permissions` — agents only run inside job
    worktrees, which is the isolation model v1 accepts. Optionally
    `backends.agy.ensure_permissions` grants are checked by `sdlc validate`
    for setups that prefer explicit allow-lists (`permission_mode: default`).
    `sdlc validate --smoke` runs a prompt through a pipe and fails loudly if
    stdout is empty.
  - envelope `{"status":"ERROR",...}` with exit 0 is treated as a failure.
  - model fallback bug ⇒ if the JSON envelope reports a model, assert it
    matches the requested one; mismatch = state failure
    (`backends.agy.assert_model: true`).
- Version pinned: `sdlc validate` records `agy --version` and warns if it
  differs from `backends.agy.expected_version` (empty = no check).

**exec** (generic escape hatch): any command template; prompt passed via
stdin or file; for future CLIs. Config-only, no code change to add one.

### 6.3 Prompts

Each state has a prompt template (Go `text/template`) resolved from
`states.<STATE>.prompt` (path, relative to the orchestrator config file).
Defaults ship in `prompts/` and are embedded in the binary (`embed.FS`);
a config path overrides the embedded default.

Template context: issue title/body, job id, branch, relative artifact paths
(spec/plan/reviews/analysis as applicable), review findings from the previous
rejected round, failure logs excerpt (tail, `limits.log_excerpt_lines`),
classification, fix constraints. Every rendered prompt instructs the agent to
write its output artifact to an explicit relative path and echoes the exact
JSON schema required.

Prompt content rules (normative):
- The FIXING template must instruct: *diagnose root cause; if the correct fix
  requires changing an existing test, emit `test_change_requested` and stop* —
  never "make the build green".
- Review templates require checklist-driven findings with severities; nothing
  instructs a reviewer to "find problems" unconditionally.

`prompt_hash` (sha256 of the rendered prompt) is recorded on every event.

### 6.4 Independence guardrail

`states.<S>.must_differ_backend_from: <OTHER_STATE>` — at config load,
resolve both states' agents; if they share a backend, **fail startup** with a
clear error. Defaults: CODE_REVIEW vs IMPLEMENTING, DESIGN_REVIEW vs PLANNING.

---

## 7. Git management

- Job branch: `<targets.<t>.branch_prefix><job-id>` from
  `targets.<t>.default_branch`.
- Worktree: `<targets.<t>.worktrees_dir>/<job-id>` (default
  `<repo>/.worktrees/<job-id>`; the orchestrator ensures `.worktrees/` is
  ignored via `.git/info/exclude`).
- On every state entry the engine records `head_sha`. **Resume reconciliation:**
  on startup, for each non-terminal job, compare actual worktree HEAD/status
  to the recorded sha. All agent states are *restartable*: uncommitted work is
  discarded (`git reset --hard <recorded sha>` + `git clean -fd`) and the
  state re-runs. BUILDING/TESTING/FLAKE_CHECK re-run idempotently.
  AWAITING_*/BLOCKED_*/holds resume as parked.
- MERGING procedure: `git fetch` (if remote) → rebase job branch onto
  `default_branch` → if `merge.verify_after_rebase: build`, re-run the build
  command → `git checkout <default>` (in main repo) → `git merge --no-ff` →
  push when `merge.push: auto` and a remote exists. Rebase conflict ⇒ abort
  rebase, ESCALATED.
- Cleanup: on terminal states, worktree removed per `git.cleanup_worktrees:
  on_success|always|never` (branch is kept unless `git.delete_branch_on_success:
  true`).
- Guardrail: the orchestrator refuses to run agents in, or force-push to, any
  branch in `policies.protected_branches`.

---

## 8. Build & test execution (orchestrator-run)

`ExecRunner` runs command vectors (no shell) with: streaming capture to
`logs/`, timeout, env overlay, and exit-code capture. Per target:

- BUILDING: `targets.<t>.build.commands` (list of argv vectors) sequentially.
- TESTING: `targets.<t>.unit_test.command`, then (if enabled + lease)
  `targets.<t>.ui_test.command`, then optional `targets.<t>.lint.command`
  (position configurable via `targets.<t>.lint.run_in: building|testing|off`).
- FLAKE_CHECK: re-runs `unit_test`/`ui_test` command (whichever failed)
  `limits.flake_rerun_count` times.
- Failure log excerpts (last `limits.log_excerpt_lines` lines, plus any lines
  matching `limits.log_error_patterns`) are extracted for the ANALYZING prompt.

Working dir = job worktree (MERGING verify runs there too; RELEASING runs in
the main repo checkout). On Windows, `.bat`/`.cmd` commands are invoked via
`cmd /c` automatically.

---

## 9. Release path

RELEASING runs `targets.<t>.ship.command` (default:
`autoship run --config <repo>/autoship.yaml`) in the **main repo**, after
MERGING pushed/updated `main`. Notes:

- Default config ships with `ship.command` set to `autoship dry-run ...` —
  flipping to real publishing is a deliberate one-line config change.
- autoship exit 0 ⇒ COMPLETED. Non-zero ⇒ retry up to
  `limits.max_release_retries` with `limits.release_retry_backoff` between
  attempts, then ESCALATED (autoship's own halt latch is respected: retries
  first run `autoship resume` when `ship.auto_resume_halt: true`).
- Agents can never invoke ship: the ship command is not part of any prompt,
  and RELEASING is orchestrator-exec only. Play credentials remain inside
  autoship's DPAPI store.

---

## 10. Quota / rate-limit handling

Any agent run classified as quota-hit (per-backend regex on envelope/stderr):
- Does **not** increment agent retries or fix attempts.
- Job → `BLOCKED_ON_QUOTA`, `prev_state` = the interrupted state,
  `resume_after = now + backends.<b>.quota_backoff` (default 30m).
- Scheduler auto-reactivates the job at `resume_after` (re-enters
  `prev_state` from scratch — restartable semantics).
- `sdlc resume JOB-x` re-activates immediately.

---

## 11. CLI

```
sdlc submit  --target <key> [--title "..."] (--body "..." | --file issue.md)
             → prints job id (JOB-<n>, prefix configurable)
sdlc run     [--once]         start the engine (foreground; lock-file guarded).
                              --once drains runnable work then exits; default
                              runs until Ctrl-C.
sdlc status  [job]            table of jobs / detail incl. counters, waits
sdlc approve <job> [--note]   consume current AWAITING_* gate
sdlc reject  <job> --reason "..." [--cancel]
sdlc cancel  <job>
sdlc resume  <job> [--to STATE]   clear ESCALATED/TIMED_OUT/BLOCKED hold
sdlc events  <job> [--json]   event log
sdlc logs    <job> [--last]   print artifact/log paths (and tail last log)
sdlc validate                 config + environment doctor: binaries exist &
                              versions recorded, backends smoke-tested
                              headlessly, independence constraints, target
                              repos clean, adb reachable. Exits non-zero on
                              any hard failure.
sdlc version
```

`submit`, `approve`, `reject`, `cancel`, `resume` only write the DB; the
running engine picks changes up on its next tick (`orchestrator.poll_interval`).
Approvals print a diff-stat + artifact paths so the human can review before
approving.

---

## 12. Configuration (complete reference)

Single YAML file; resolution order: `--config` flag → `SDLC_CONFIG` env →
`./sdlc.yaml`. Every key below exists with exactly this name; defaults shown.
Env-var expansion (`${VAR}`) is applied to string values. `sdlc validate`
rejects unknown keys (typo protection).

```yaml
orchestrator:
  data_dir: ./data              # DB, per-job artifacts, logs, gradle homes
  max_parallel_jobs: 2          # jobs progressing concurrently
  poll_interval: 3s             # engine tick for DB-driven changes
  job_id_prefix: JOB            # job ids look like JOB-12
  lock_file: ${data_dir}/engine.lock   # single-engine enforcement

database:
  path: ${data_dir}/sdlc.db
  busy_timeout: 5s

limits:
  max_job_duration: 12h         # wall clock from submit; then TIMED_OUT
  max_design_review_rounds: 2
  max_code_review_rounds: 3
  max_fix_attempts: 3           # shared across CODE_REVIEW/ANALYZING/FINAL_REVIEW-driven fixes
  max_flake_retries: 2
  flake_rerun_count: 3          # deterministic reruns in FLAKE_CHECK
  max_agent_retries: 2          # per state, post-condition failures
  max_release_retries: 3
  release_retry_backoff: 10m
  max_agent_invocations_per_job: 40   # budget guardrail; exceed ⇒ ESCALATED
  log_excerpt_lines: 200        # failure-log tail fed to ANALYZING
  log_error_patterns: ["(?i)error", "(?i)exception", "FAILED"]

resources:
  gradle_slots: 1               # concurrent gradle invocations, all jobs
  devices:
    serials: []                 # explicit adb serials for the UI-test pool
    discover: true              # add `adb devices` results to the pool
    adb_binary: adb             # absolute path or PATH lookup
    acquire_timeout: 10m
    boot_emulator:
      enabled: false
      avd_name: ""
      emulator_binary: emulator # resolved via PATH if bare
      boot_timeout: 6m
      headless: true            # -no-window
      kill_after_job: true

backends:
  claude:
    binary: claude              # absolute path or PATH lookup
    default_timeout: 30m
    permission_mode: bypassPermissions
    quota_error_patterns: ["(?i)rate.?limit", "(?i)usage.?limit", "(?i)overloaded", "(?i)quota"]
    quota_backoff: 30m
    expected_version: ""        # non-empty ⇒ sdlc validate warns on mismatch
    extra_args: []
  agy:
    binary: agy
    default_timeout: 30m
    print_timeout: 45m          # agy --print-timeout; must exceed state timeout
    settings_file: ${USERPROFILE}/.gemini/antigravity-cli/settings.json
    ensure_permissions: []      # permission grants validate checks/writes
    assert_model: true          # fail state if envelope model ≠ requested
    quota_error_patterns: ["(?i)rate.?limit", "(?i)quota", "(?i)resource.?exhausted"]
    quota_backoff: 30m
    expected_version: ""
    extra_args: []

agents:
  opus:   { backend: claude, model: opus,   effort: high }
  sonnet: { backend: claude, model: sonnet, effort: "" }
  gemini: { backend: agy,    model: gemini-3-pro, effort: medium }
  # add more freely; `backend` must be a key under backends

states:                          # every agent state must appear here
  PLANNING:
    agent: opus
    prompt: ""                   # empty ⇒ embedded default prompts/planning.md
    timeout: 30m
    allowed_tools: []            # passed through when backend supports it
    disallowed_tools: []
  DESIGN_REVIEW:
    agent: sonnet
    prompt: ""
    timeout: 15m
    disallowed_tools: [Edit, NotebookEdit, Bash]  # Write stays: must emit .sdlc/review.json
    must_differ_backend_from: "" # optional; see CODE_REVIEW
  IMPLEMENTING:
    agent: gemini
    prompt: ""
    timeout: 45m
  CODE_REVIEW:
    agent: opus
    prompt: ""
    timeout: 20m
    disallowed_tools: [Edit, NotebookEdit, Bash]
    must_differ_backend_from: IMPLEMENTING
  ANALYZING:
    agent: sonnet
    prompt: ""
    timeout: 10m
    disallowed_tools: [Edit, NotebookEdit, Bash]
  FIXING:
    agent: gemini
    prompt: ""
    timeout: 45m
  FINAL_REVIEW:
    agent: opus
    prompt: ""
    timeout: 20m
    disallowed_tools: [Edit, NotebookEdit, Bash]
    must_differ_backend_from: IMPLEMENTING

policies:
  protected_branches: [main, master]
  protect_tests_on_code_bug_fix: true   # FIXING diff may not touch test files when classification=code_bug
  test_file_globs:
    - "**/src/test/**"
    - "**/src/androidTest/**"
    - "**/*Test.kt"
    - "**/*Test.java"
  reviewer_diff_must_be_empty: true     # review states may not modify the tree

git:
  cleanup_worktrees: on_success  # on_success | always | never
  delete_branch_on_success: false

targets:
  expensetracker:
    repo_path: C:\Users\vm899\repos\ExpenseTracker
    default_branch: main
    branch_prefix: sdlc/
    worktrees_dir: ""            # empty ⇒ <repo_path>/.worktrees
    gradle:
      isolated_user_home: false  # true ⇒ GRADLE_USER_HOME per job (first build is slow)
    build:
      commands:
        - ["gradlew.bat", ":app:assembleDebug"]
      timeout: 30m
    unit_test:
      command: ["gradlew.bat", ":app:testDebugUnitTest"]
      timeout: 30m
    lint:
      command: ["gradlew.bat", ":app:lintRelease"]
      run_in: "off"              # building | testing | off
      timeout: 20m
    ui_test:
      enabled: true
      command: ["gradlew.bat", ":app:connectedDebugAndroidTest"]
      timeout: 40m
      on_no_device: skip         # skip | fail | wait
    merge:
      rebase_before_merge: true
      verify_after_rebase: build # build | none
      push: auto                 # auto (push if remote exists) | never
    ship:
      command: ["autoship", "dry-run", "--config", "autoship.yaml"]  # argv; cwd=repo_path
      resume_command: ["autoship", "resume", "--config", "autoship.yaml"]
      timeout: 60m
      auto_resume_halt: true     # run resume_command before a retry
```

---

## 13. Guardrails summary (all orchestrator-enforced)

1. **Test-file policy:** when the active fix classification is `code_bug`,
   the FIXING diff is mechanically checked against `policies.test_file_globs`;
   any touched test file ⇒ commit rejected, ESCALATED with the diff attached.
2. **Reviewer immutability:** review/analyze states must leave the tree
   untouched; violation is a state failure.
3. **Backend independence:** `must_differ_backend_from` asserted at startup.
4. **Protected branches:** agents never run outside their worktree; the
   orchestrator never force-pushes; merges only via the MERGING handler.
5. **Budgets:** per-state timeouts, `max_job_duration`, retry caps per loop,
   `max_agent_invocations_per_job` — all halts land in ESCALATED/TIMED_OUT
   with reasons, never silent.
6. **Human gates:** two separate approvals (merge vs release); rejection with
   reason feeds back into FIXING.
7. **Credential separation:** release runs only through autoship in the main
   repo; ship commands never appear in agent prompts or worktrees.
8. **Config hygiene:** unknown YAML keys rejected; env/binary/versions checked
   by `sdlc validate`; every event records backend, model, binary version,
   prompt hash.

---

## 14. Repository layout (this repo)

```
cmd/sdlc/main.go
internal/
  cli/          subcommand dispatch (stdlib flag; no cobra dependency)
  config/       schema, defaults, strict YAML decode, validation, expansion
  store/        SQLite open/migrate, single-writer, job/event/approval DAOs
  engine/       scheduler, worker pool, state machine, handlers, resume
  states/       one handler per state (planning.go, building.go, ...)
  agent/        AgentRunner, claude/agy/exec adapters, envelope parsing
  prompt/       embedded templates, rendering, hashing
  artifact/     paths, schema validation (spec/plan/review/impl/analysis)
  gitx/         worktree, branch, commit, rebase, merge, diff, policy checks
  execx/        streaming command runner (timeouts, env, cmd /c handling)
  resource/     gradle-slot semaphore, device pool, emulator boot
  guard/        policy checks (test-file globs, budgets, independence)
prompts/        default templates (embedded via embed.FS)
docs/SPEC.md    this document
sdlc.example.yaml
```

Dependencies: standard library + `modernc.org/sqlite` + `gopkg.in/yaml.v3`.
Nothing else without a reason recorded in this spec.

## 15. Testing & acceptance

- Unit tests: config strict-decode + defaults; every transition in §3.2 via a
  table test; artifact validators (valid + each missing-field case);
  test-file-glob policy; flake decision logic; quota classification;
  counters; resume reconciliation (fake git dir).
- Integration tests (no network, no real agents): a `fakeagent` test binary
  (built during tests) acts as both backends — scripted per scenario to write
  artifacts/edit files/return quota errors/return empty stdout. A temp git
  repo with a trivial Go-free "build" (a script that exits per fixture)
  exercises: happy path to AWAITING_MERGE_APPROVAL, approve→merge→
  (dry ship stub)→COMPLETED; review-reject loop; fix loop with test-file
  violation; flake path; crash-kill mid-IMPLEMENTING then resume.
- `go vet ./...` and `go test ./...` must pass on Windows.
- Manual acceptance (documented in README, run by the operator): `sdlc
  validate` on the real machine, then one real job against ExpenseTracker
  with `ship.command` in dry-run mode.

## 16. Build order

1. config + store + CLI skeleton (`submit/status/validate` minimal)
2. execx + gitx (worktrees, commit, diff, merge) with tests
3. engine core: scheduler, transitions, counters, resume, locks
4. artifact schemas + prompt rendering (embedded defaults)
5. agent runner + claude/agy/exec adapters + quota classification
6. build/test/flake handlers + resource manager
7. review loops, guardrails, human gates, merging
8. releasing via autoship; cleanup
9. fakeagent integration suite; `sdlc.example.yaml`; README
