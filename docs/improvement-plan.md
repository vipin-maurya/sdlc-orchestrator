# Improvement plan

Findings from JOB-1 ("Fix 1.0.6 issues", ExpenseTracker, 2026-08-21), the first
long unattended run. The job reached AWAIT_MERGE-adjacent states in ~90 minutes
across 11 agent invocations, three ESCALATED holds, and one destroyed set of
human commits. Everything below is grounded in that run's artifacts, logs, and
event stream.

Items are ordered by cost of leaving them unfixed, not by effort.

---

## P0 — data loss and dead ends

### 1. Strip the BOM before parsing agent JSON

**Symptom.** IMPLEMENTING ran 13m45s, wrote all 22 files correctly, and was
scored a total failure: `post-condition: invalid character 'ï' looking for
beginning of value`.

**Evidence.** `head -c 8 .sdlc/implementation.json` → `efbb bf7b` — a UTF-8 BOM.
`artifact.ReadJSON` strips markdown fences but not the BOM.

**Change.** In `internal/artifact/artifact.go`, `ReadJSON`: drop a leading
`﻿` (and the raw `EF BB BF` bytes) before `TrimSpace`. Agents on Windows
backends write BOMs routinely; this is not exotic input.

**Verify.** Add a case to `artifact_test.go` with a BOM-prefixed fixture for each
`Load*` function.

---

### 2. Do not attribute out-of-band commits to the agent

**Symptom.** Two human commits containing a verified regression fix and four
spec-mandated fixture updates were discarded by the orchestrator:

```
fix for code_bug touched test files [PerFieldExpenseParserTest.kt
SmsExpenseParserParameterizedTest.kt SmsExpenseParserTest.kt]
— change discarded, human review required
```

**Evidence.** `handleFixing` sets `entryHead := c.job.HeadSHA`, which the
orchestrator only updates inside its own `commit()`. A human who commits on the
job branch is invisible to it, so `DiffNamesSince(worktree, entryHead)` returns
*their* files, `guard.CheckFixDiff` charges them to the agent, and
`ResetHardClean(worktree, entryHead)` rolls the branch back. The agent's own
3m58s of work went with it.

**Change.** Two parts, both needed:
- Resync `job.HeadSHA` from the worktree at the top of every restartable state
  handler, so the orchestrator notices a branch that moved underneath it.
- Scope the guard to the agent's own commits — diff against the SHA captured
  *immediately before* the agent runs (`headBefore` in `runAgent` already exists),
  not against a counter that may be hours stale.

**Verify.** `engine_test.go`: a job whose branch advances between two states must
not trip `CheckFixDiff` on the pre-existing commits.

---

### 3. `resume` silently ignores flags after the job id

**Symptom.** `sdlc resume JOB-1 --to BUILDING` resumed into **FIXING** — the state
that had just escalated — instead of BUILDING. Event log: `{"gate":"resume",
"to":"FIXING"}`. This sent the job straight back into the guard that then
destroyed the commits in item 2.

**Evidence.** `cmdResume` calls `fs.Parse(args)` with the positional already in
`args`. Go's `flag` package stops at the first non-flag token, so `--to` is never
parsed, `Reason` is empty, and the engine falls back to `PrevState`. No error is
printed. The working form is `sdlc resume --to BUILDING JOB-1`.

**Change.** Parse flags after extracting the positional (`fs.Parse(args[1:])`),
or accept flags in either position. Reject unknown trailing arguments loudly
rather than ignoring them. Audit `cmdControl`, `cmdDecision`, `cmdLogs`,
`cmdEvents` for the same shape.

**Verify.** CLI test asserting both orderings produce the same approval record.

---

### 4. Surface `HeadSHA` drift instead of silently resetting

**Symptom.** After a human advances the branch, `reconcile` on engine restart
runs `ResetHardClean(worktree, j.HeadSHA)` and erases the work with no warning.

**Change.** Before resetting, compare the recorded `HeadSHA` against the actual
branch head. If the branch is *ahead*, fast-forward the record and log it; only
reset when the tree is genuinely dirty or the branch diverged. Emit a `note`
event either way so the reset is visible in `sdlc events`.

---

## P1 — feedback integrity

### 5. Verify pass for review findings

**Symptom.** The DESIGN_REVIEW agent produced exactly one finding, `major`,
against `ScanSummaryFragment`'s use of `previousBackStackEntry`. It was
articulate, named the nav graph shape correctly, and supplied a plausible
failure scenario:

> `previousBackStackEntry` from ScanSummaryFragment therefore resolves to the
> nav_approval_flow graph entry, not the nav_transform entry […] the Snackbar
> silently never shows.

**It was wrong.** Disassembling `androidx.navigation:navigation-runtime:2.6.0`
(the version this target actually resolves) shows `getPreviousBackStackEntry`
discarding the top entry and then applying `firstOrNull { entry ->
entry.destination !is NavGraph }`:

```
78: getDestination()
81: instanceof NavGraph
84: ifne 91          // predicate true when NOT a NavGraph
```

It skips graph entries. The write lands on `nav_transform` — exactly where the
message needs to go. The finding was a confident, well-reasoned false positive
derived from remembered library semantics rather than the resolved dependency.

**Why this matters more than the one finding.** A single reviewer stage produces
exactly this failure mode, and the pipeline has no way to tell a real finding
from a fluent wrong one. Today the blocker-only gate hides the problem by
discarding sub-blocker findings entirely (item 6) — which happened to be lucky
here, and would be actively harmful for a genuine `major`. Raising the gate
without a verify pass would have sent this job into a rework loop over a
non-existent bug.

**Design.**

Insert a verification step between loading a review artifact and gating on it.
It runs in `handleDesignReview`, `handleCodeReview`, and `handleFinalReview` —
one implementation, three call sites.

*Scope.* Only findings that can gate (`blocker`, and `major` once item 6 lands).
`minor`/`nit` are never verified; spending tokens on findings that cannot change
control flow is waste. When no gating findings exist — the common case — the pass
is skipped entirely and costs nothing.

*Fan-out.* `limits.verify_votes` (default 3) independent agents per gating
finding, run concurrently, each seeing only that one finding plus the same
context the reviewer had (`spec.json`, `plan.json`, `diff.patch` where staged).
Verifiers must not see each other's verdicts.

*Stance.* Each verifier is asked to **refute**, and must produce evidence for
whichever verdict it reaches. The prompt encodes the lesson this run taught:

> A claim about third-party library or framework behavior is not verified until
> you have checked the resolved dependency — read the source jar, or disassemble
> the artifact in the dependency cache. Do not confirm or refute such a claim
> from memory. State which artifact and version you inspected.

That single rule is what refuted F1, and it is the class of error a code-reading
reviewer is most prone to.

*Output.* New schema `verdict/1`:

```json
{
  "schema": "verdict/1",
  "finding_id": "F1",
  "verdict": "confirmed | refuted",
  "evidence": "what was inspected and what it showed",
  "reasoning": "why that settles it"
}
```

*Aggregation.* A finding survives at its stated severity when
`confirmed >= limits.verify_min_confirm` (default 2 of 3). Otherwise it is
downgraded to `nit`, annotated with the verdicts, and excluded from `Blockers()`.
Findings are never deleted — a refuted finding stays in the artifact with its
refutation, so the record shows what was considered and why it was dismissed.

*Persistence.* Write `design_review.r1.verified.json` beside the raw artifact and
emit a `verify_pass` event carrying `{gating: N, confirmed: M, refuted: K}`, so
`sdlc events` shows the pass happened and what it changed.

*Config.*

```yaml
limits:
  verify_votes: 3          # independent verifiers per gating finding
  verify_min_confirm: 2    # votes needed for the finding to survive

agents:
  verifying: { backend: claude, model: opus, effort: high }

states:
  verifying:
    timeout: 10m
    disallowed_tools: [Edit, Write, NotebookEdit]
```

Verifiers are reviewers: reuse `requireCleanTree` so they cannot modify the
worktree, and count their invocations against
`limits.max_agent_invocations_per_job`.

*Model choice.* This is the one stage where a strong model earns its cost —
refuting a plausible claim is harder than generating one. Default `verifying` to
the same tier as the reviewer or above, never below.

**Files.** `internal/artifact/artifact.go` (schema + aggregation),
`internal/engine/handlers.go` (three call sites, one shared helper),
`internal/engine/jobctx.go` (a `runVerifiers` fan-out),
`prompts/verify_finding.md` (new), `internal/config/config.go` (limits + agent +
state), `sdlc.example.yaml`.

**Verify.** Unit-test aggregation in `artifact_test.go` across the vote matrix
(3/3, 2/3, 1/3, 0/3 confirmations). Engine test with a fake runner returning
scripted verdicts, asserting that a majority-refuted blocker does not gate and a
majority-confirmed one does. Replay JOB-1's F1 as a fixture: it must come back
refuted.

---

### 6. Make the gate threshold configurable

**Symptom.** `Review.Blockers()` hardcodes `severity == "blocker"`. A `major`
finding cannot gate anything, and nothing else consumes it either, so it is
discarded on the floor.

**Change.** `limits.review_gate_severity: blocker | major` (default `blocker`
until item 5 ships, then `major`). Generalise `Blockers()` to
`GatingFindings(threshold)`.

**Depends on item 5.** Raising the threshold without verification would have sent
JOB-1 into a rework loop over a false positive.

---

### 7. Propagate surviving findings to the implementer

**Symptom.** `PrevFindings` is populated only on rework rounds
(`DesignReviewRounds > 0`). On the first pass — the common case — every
non-gating finding is dropped before IMPLEMENTING ever sees it.

**Change.** Always stage the verified review artifact into `.sdlc/context/` for
the next state, and render surviving non-gating findings in the implementer
prompt as advisory notes. Cheap: the artifact already exists on disk.

---

### 8. Give the human a channel to instruct the next agent

**Symptom.** JOB-1 escalated four times on `test_change_requested`. The
correct human answer was "yes, those four fixtures are stale, update them" — and
there was no way to say it. `sdlc resume` carries only a target state, so
resuming returns to FIXING with `LastAnalysis` still `code_bug`, the prompt
renders the forbidden branch again, and it escalates identically. The operator's
only options were to hand-edit the repo or cancel.

**Evidence.** The machinery already exists and is unreachable: the merge-gate
`reject --reason` path sets `FixSource = "human"` and `HumanRejectReason`, which
makes `handleFixing` render `pctx.RejectReason` with an empty classification —
the prompt's permissive branch, and a `CheckFixDiff` that permits test edits.
That is exactly the mode needed. It is wired only to `handleGate`.

**Change.** `sdlc resume --to FIXING --note "..." JOB-1` carrying the note into
`HumanRejectReason` and clearing `LastAnalysis`, so the grant is scoped to one
attempt and recorded in the event log. Pairs with item 3 — the flag must actually
parse.

---

## P2 — durability

### 9. Commit at every step, not once per state

**Symptom.** One commit per state means a 13-minute, 22-file IMPLEMENTING run is
all-or-nothing. When its post-condition failed on the BOM, everything was
discarded and re-run from scratch. Separately, fix attempt 2's regression fix sat
uncommitted in the worktree for ~40 minutes, one `reconcile` away from deletion.

**Change.**
- Commit on **every** state that produces work, including states that then
  escalate — an escalated state's output is exactly what the human needs to
  inspect.
- Within IMPLEMENTING and FIXING, commit per completed plan step. The plan
  already carries step ids (`S1`…`S12`); have the agent emit
  `steps_completed` incrementally and checkpoint each one, so a failed
  post-condition costs one step rather than the whole state.
- Never leave the worktree dirty across a state boundary.

---

### 10. Give retry prompts real context

**Symptom.** `007_IMPLEMENTING.md` is `006` plus six lines: *"The previous attempt
failed its post-condition check: … Re-read the output requirements above and
comply exactly."* It does not say the worktree already contains every edit, nor
that the sole fault was file encoding. The retry re-did work that was already
complete and correct.

**Change.** The retry block should state what the previous attempt accomplished
(files touched, steps completed), the current worktree state, and the specific
post-condition that failed — not a generic instruction to comply.

---

## P3 — observability

### 11. Stream agent output

**Symptom.** *"The run is very silent, doesn't show anything for many minutes."*
PLANNING ran 12m05s with no output. An in-flight state is indistinguishable from
a hung one: artifacts and logs appear only after the agent exits.

**Evidence.** The engine logs only at transitions
(`log.New(os.Stdout, …)` in `engine.go`); agent output goes to `LogPath` only.
The claude backend is already invoked with `--output-format stream-json`, so the
event stream exists and is being discarded.

**Change.** Parse the backend's stream and emit a condensed live line per
tool-use / message (state, elapsed, what the agent is doing). Add
`orchestrator.stream_output: true|false` for headless runs. `sdlc logs JOB --last`
should tail a running state, not just a finished one.

---

### 12. Heartbeat long deterministic phases

**Symptom.** BUILDING/TESTING/FLAKE_CHECK ran 22s + 2m14s + 3×~2m20s in silence.
FLAKE_CHECK is the worst offender: three full suite reruns with no indication of
which rerun is in flight.

**Change.** Emit a progress event per phase start and per flake rerun, and show
elapsed time for the current state in `sdlc status`.

---

## P4 — contract between planner, prompt, and engine

### 13. Tell the planner that the orchestrator owns build and test

**Symptom.** The plan's first and last steps (S0 "capture a green baseline", S12
"re-run the full suite and compare") instruct the implementer to run
`./gradlew … --rerun-tasks`. The implementer prompt forbids exactly that:
*"Do NOT run builds or full test suites; the orchestrator does that after you
finish."* Two of the plan's twelve steps were unexecutable as written.

**Change.** State the division of labour in `planning.md`: BUILDING and TESTING
are orchestrator-owned states with configured commands; a plan step must never be
"run the suite". Have the planner express verification as *what must hold*, not
*what command to run*.

---

### 14. Require the plan to account for its own blast radius

**Symptom.** Changing `cleanMerchantName` broke fixtures in three test files that
`affected_files` never listed. That omission surfaced an hour later as the
escalation loop in item 8.

**Change.** Add a planning requirement: for every production symbol the plan
modifies, enumerate existing tests that assert its current behavior and state
whether each is expected to change. Cheap to check, and it converts a mid-run
escalation into a planned step.

---

### 15. Reconcile the agent's self-report against the worktree

**Symptom.** `implementation.json` listed `MonthStepperLayoutTest.kt` as changed;
git showed it untouched (the agent wrote `TransactionsLayoutTest.kt` instead).
Post-conditions check `len(changed) != 0`, not whether the claim is true.

**Change.** Diff `files_changed` against `DiffNamesSince`. A mismatch is a
post-condition failure with a specific message naming the discrepancy — far more
actionable than "comply exactly".

Related: the gemini backend wrote `{"status", "notes", "steps_completed"}`
against a schema requiring `schema` and `summary`. It would have failed even
without the BOM, and `impl.Summary` is the commit message, so a missing summary
also breaks the commit. The validation error should name the missing fields and
show the keys actually present.

---

## P5 — routing

### 16. Match model tier to state difficulty

**Observed.** Opus planned (12m05s, $5.43 — well spent). Sonnet reviewed (2m38s —
produced one finding, which was wrong; see item 5).
`gemini-3.7-flash-medium` handled IMPLEMENTING and FIXING across 22 files and
produced: a BOM, a wrong schema, a file-list mismatch, a self-inflicted
`TXN_ID_EXPLICIT` regression that violated the spec's own acceptance criterion,
and four escalations. Two of eleven invocations were retries of its own output
errors.

**Change.** Default the implementation and fix states to the same tier as
planning. Treat a flash tier as opt-in for mechanical states only.

---

### 17. Let a valid artifact outweigh a nonzero exit

**Symptom.** The gemini backend exits 1 on its own harness errors (`… is not a
valid artifact path`) even when the work is complete and correct.
`runAgent` comments that "Exit code is a hint; the post-condition decides" — but
a valid artifact plus a nonzero exit still scored as a total failure, because the
artifact was unparseable for an unrelated reason (item 1).

**Change.** Once item 1 lands this largely resolves itself, but make the
precedence explicit and logged: when the post-condition passes, a nonzero exit is
recorded as a `note` and the state proceeds.

---

## Suggested sequencing

1. **Items 1, 3** — one-line fixes, each cost a full state in this run.
2. **Items 2, 4** — stop destroying human work; unblocks safe manual intervention.
3. **Item 8** — makes escalations resolvable without hand-editing the repo.
4. **Item 5, then 6, then 7** — the feedback loop, in dependency order. Item 6 is
   unsafe before item 5.
5. **Items 9, 10** — durability.
6. **Items 11, 12** — observability.
7. **Items 13–17** — contract and routing; mostly prompt and config edits.
