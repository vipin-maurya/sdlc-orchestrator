# Problem scoping — Specification

Version: 1.0
Status: Proposed for implementation
Date: 2026-08-23
Amends: [`docs/SPEC.md`](SPEC.md) §3.1 (states), §3.2 (transition table), §3.3
(counters), §5.1 (schemas), §6.3 (prompts), §11 (CLI), §12 (config), §14
(layout)

---

## 1. Summary

A new agent state, `SCOPING`, runs between `CREATED` and `PLANNING`. It reads
the issue and the repository and writes one artifact, `problem.json`: a problem
statement in the repository's own vocabulary, explicit in-scope and out-of-scope
lists, success criteria, the assumptions it had to make, and the questions it
could not answer. It writes no spec, no plan, and no code.

The state exists because of one conflation in the current pipeline. `PLANNING`
is handed `--title` and `--body` verbatim (`handleCreated` returns `SPlanning`)
and must do two different jobs in a single invocation: work out *what is being
asked*, and design *how to build it*. When the issue is precise those two jobs
are one. When it is not — "Fix 1.0.6 issues" was a real submitted title — the
interpretation is made silently inside `spec.approach`, and the first artifact
anybody can inspect is a full spec and plan built on top of it. `DESIGN_REVIEW`
then reviews that spec against the same ambiguous issue text, so it has no
independent position from which to notice that the interpretation was wrong.
The cost of finding out is a re-plan at best, and at worst an implemented,
built, tested, merged change that solves a problem nobody had.

Three properties are load-bearing:

1. **A vague ticket is an expected outcome, not a failure.** When the scoping
   agent cannot proceed it parks at an approval gate, not in the `ESCALATED`
   hold. The operator sees the questions in the same approve/reject surface they
   already use for the spec, code, merge and release gates, in the terminal and
   in the browser.
2. **`clarity` is enforced, not declared.** The field that decides the route is
   held to the rest of the artifact by cross-field validation, so an agent
   cannot raise a blocking question and still route itself past the human.
3. **The scoped problem is consumed, not filed.** `PLANNING` is bound by it and
   `DESIGN_REVIEW` checks the spec against it. A scoping artifact that nothing
   downstream reads would be an expensive comment.

No new module dependencies. No new decision verbs at the gate. No new reviewer
agent.

---

## 2. Goals and non-goals

### 2.1 Goals

- **G1 — Separate interpretation from design.** `PLANNING` receives a scoped
  problem, not raw issue text, and is bound by its scope boundaries.
- **G2 — Ask before building, when asking is warranted.** An issue the agent
  genuinely cannot resolve stops the job at a gate that shows the specific
  questions, before an agent invocation is spent on a spec.
- **G3 — Do not stall an unattended run without cause.** Ambiguity the agent can
  resolve from the repository is resolved and recorded as an assumption. Only a
  question with no defensible default blocks.
- **G4 — Keep scope drift observable.** `DESIGN_REVIEW` gains the ability to say
  "this spec is not solving the scoped problem", which today it cannot.
- **G5 — Cost one agent invocation.** For an already-precise issue the state
  reads, confirms, writes, and moves on.

### 2.2 Non-goals

- **No `SCOPE_REVIEW` state.** A second reviewer agent before any spec exists is
  not worth its invocation; `DESIGN_REVIEW` checking spec-against-problem covers
  the same failure at no extra cost. §6.2.
- **No change to `spec/1`.** The linkage between problem and spec is carried by
  prompts and by the design-review check, so no existing artifact schema
  changes and no existing job in the database becomes unreadable.
- **No requirements elicitation with the end user.** The scoping agent reads the
  issue and the repository. It does not consult tickets, chat, or history it was
  not given.
- **No retroactive scoping.** Jobs already past `CREATED` never enter the state.

---

## 3. State machine (amends SPEC §3.1, §3.2)

### 3.1 New states

| State | Kind | Meaning |
|---|---|---|
| `SCOPING` | agent | Produces `problem.json` from the issue and the repository. |
| `AWAITING_SCOPE_APPROVAL` | parked | Waiting on a human decision at the `scope` gate. |

`SCOPING` joins `config.AgentStates` and therefore requires an entry under
`states:` and takes part in the existing agent machinery unchanged: prompt
rendering and hashing, retry budget, quota suspension, timeout, streaming,
heartbeat, and crash-resume via `needsWorktreeReconcile` (true for every agent
state — a partial scoping run is discarded and re-run).

`AWAITING_SCOPE_APPROVAL` joins `isParked`. `SCOPING` joins `resumable`, in
pipeline order, ahead of `PLANNING`.

`producesCode` remains false for `SCOPING`: like the planner and the reviewers,
it must leave the tree clean, and `handleScoping` calls `discardTreeChanges`
before it transitions, exactly as `handlePlanning` does today.

### 3.2 Transitions

```
CREATED ──────────────────────────────────► SCOPING          (policies.scoping: on)
CREATED ──────────────────────────────────► PLANNING         (policies.scoping: off)

SCOPING  ─ clarity == "blocked" ──────────► AWAITING_SCOPE_APPROVAL
SCOPING  ─ human_gates has "scope" ───────► AWAITING_SCOPE_APPROVAL
SCOPING  ─ otherwise ─────────────────────► PLANNING

AWAITING_SCOPE_APPROVAL ─ approve ────────► PLANNING
AWAITING_SCOPE_APPROVAL ─ reject --reason ► SCOPING
AWAITING_SCOPE_APPROVAL ─ cancel ─────────► CANCELLED       (existing control path)
```

The two routes into the parked state are distinct in cause and identical in
mechanism. The gate document names which one applied (§7.2), and both are
recorded as `note` events with a `reason` of `agent_blocked` or `policy_gate`.

Re-scoping is bounded by `limits.max_scope_rounds`. On entering `SCOPING` with
`Counters.ScopeRounds >= max_scope_rounds`, the state escalates rather than
running the agent again:

```
scope still unresolved after N round(s); see problem.json and the gate history
```

This is the same shape as the design-review and code-review round caps, and it
exists for the same reason: a loop between a human and an agent that neither
side ends is worse than a stop.

### 3.3 Counters (amends SPEC §3.3)

One field on `store.Counters`:

```go
// ScopeRounds counts completed SCOPING attempts that were sent back by a
// human. Like DesignReviewRounds it increments only on rejection, so it is 0
// for a scoping that was accepted first time.
ScopeRounds int `json:"scope_rounds,omitempty"`
```

`HumanRejectReason` is reused verbatim to carry the operator's answers into the
next scoping prompt, and is cleared by `handleScoping` after one attempt — the
same scoping `handlePlanning` and `handleFixing` already apply, and for the same
reason: a stale instruction re-delivered to a later round is an instruction the
agent has already answered.

---

## 4. Artifact `problem/1` (amends SPEC §5.1)

### 4.1 Schema

Written by the agent to `.sdlc/problem.json`, harvested to
`artifacts/problem.json`.

```json
{
  "schema": "problem/1",
  "problem_statement": "what is wrong or wanted, in the repository's own vocabulary, grounded in current behaviour",
  "in_scope": ["concrete piece of work this job covers"],
  "out_of_scope": ["work a reader might reasonably assume is included, and is not"],
  "success_criteria": ["observable outcome that shows the problem is solved"],
  "assumptions": [
    {"assumption": "what was taken as true", "basis": "what in the repository supports it"}
  ],
  "open_questions": [
    {"id": "Q1", "question": "...", "why_it_matters": "what changes depending on the answer", "blocking": true}
  ],
  "clarity": "clear | assumed | blocked"
}
```

### 4.2 Validation (`artifact.LoadProblem`)

Structural rules, in the style of the existing `Load*` functions — a transition
never happens on an artifact that does not validate:

- `schema` must be `"problem/1"`.
- `problem_statement` non-empty.
- `success_criteria` non-empty.
- `clarity` ∈ {`clear`, `assumed`, `blocked`}.
- Every `open_questions[i].id` and `.question` non-empty; ids unique.
- Non-blocking questions are permitted at any `clarity`. They are informational
  — carried into the gate document and the planning prompt, never routing.
- **`clarity == "blocked"` ⟺ at least one question has `blocking: true`.** Both
  directions are checked. Without the forward direction an agent can claim to be
  blocked with nothing to ask, and the job parks forever on a question the
  operator cannot answer because it was never written down. Without the reverse
  an agent can raise a blocking question and route itself straight past the
  human, which defeats the state.
- **`clarity == "assumed"` ⟹ `assumptions` non-empty.** "I made assumptions" and
  a blank list is the same failure as silence.

Validation errors follow the existing convention: name every missing or invalid
field in one message and list the keys the file actually contained
(`presentKeys`), so a retry can see the mismatch rather than discovering one
field at a time.

### 4.3 Why `assumptions` is a list of pairs

An assumption without its basis is indistinguishable from a guess, and the whole
value of the field to a downstream reader — the planner, the design reviewer,
the operator at the gate — is being able to check the reasoning cheaply. The
`basis` is what makes an assumption falsifiable in one glance.

---

## 5. Prompts (amends SPEC §6.3)

### 5.1 `prompts/scoping.md` (new)

Registered in `prompt.defaults` under `config.StScoping`. It renders from the
existing `prompt.Ctx` — `JobID`, `Branch`, `IssueTitle`, `IssueBody`, `Round`,
`MaxRounds`, `RejectReason` — with no new context fields.

Content, normatively:

- The agent is scoping, not designing. It must not propose an approach, name an
  implementation, write a spec or a plan, or edit any file other than
  `.sdlc/problem.json`.
- It must ground the problem statement in the repository: read the code that the
  issue is about and describe the *current* behaviour, so that the statement is
  checkable rather than a restatement of the title.
- It must resolve what it can. An ambiguity with a defensible default, given the
  code, is an entry in `assumptions` — not a question. Blocking is reserved for
  a choice where the answers lead to materially different work and nothing in
  the repository decides between them.
- `out_of_scope` must name the work a reasonable reader would *assume* is
  included. An empty out-of-scope list on a non-trivial issue is a scoping
  failure, not a clean bill.
- On a re-scope (`{{if .RejectReason}}`), the previous `problem.json` is
  available at `.sdlc/context/problem.json`, and the operator's text is an
  instruction that takes precedence — the same wording the planning prompt
  already uses for a rejected spec. If the text answers an open question, the
  question moves into `assumptions` with the operator's answer as its basis and
  is removed from `open_questions`.

### 5.2 `prompts/planning.md` (amended)

A new **Scoped problem** section, rendered from the staged
`.sdlc/context/problem.json`, with two binding rules:

- The spec's `out_of_scope` must include every entry of the problem's
  `out_of_scope`.
- The spec must not widen `in_scope`. Work the planner believes is necessary and
  the problem excludes is stated in `approach` as an exception, and named.

### 5.3 `prompts/design_review.md` (amended)

One added instruction: `problem.json` is staged into `.sdlc/context/`; raise a
finding when the spec does not solve the scoped problem, silently widens scope
beyond `in_scope`, or drops a `success_criteria` entry.

Severity is left to the reviewer and gated by the existing
`policies.design_review_blocks_at` threshold — this adds a class of finding, not
a new gate.

---

## 6. Engine

### 6.1 `handleScoping`

```
resetExchange
if ScopeRounds >= limits.max_scope_rounds → escalate
if ScopeRounds > 0 or HumanRejectReason != "" → stage problem.json into context/
pctx.Round      = ScopeRounds + 1
pctx.MaxRounds  = limits.max_scope_rounds
pctx.RejectReason = HumanRejectReason
runAgent(SCOPING, post-condition: LoadProblem succeeds)
harvest problem.json
clear HumanRejectReason
discardTreeChanges                       // scoping must not leave edits behind
route per §3.2
```

The staging condition mirrors `handlePlanning`'s: a human rejection does not
increment `ScopeRounds`, so keying the staging on the counter alone would send
the agent back in with nothing to revise and it would write a new problem
statement from scratch instead of answering the objection.

### 6.2 Why no separate scope reviewer

The failure this state exists to catch is a wrong interpretation. An agent
review of a problem statement, before any spec exists, has nothing to check the
statement against except the same issue text the scoping agent read — it is the
same conflation one level up. The check that has real information is
spec-against-problem, and `DESIGN_REVIEW` is already positioned to make it
(§5.3), already has a verify pass behind its findings, and already has a
rework loop. Adding the check there costs one paragraph of prompt.

### 6.3 Gate handling

`handleGate` gains two cases, alongside the existing four gates:

- `scope` + `approve` → `PLANNING`. When `problem.json` had blocking questions,
  approval is a *waiver*: the questions go unanswered and the planner proceeds
  on the recorded assumptions. This is recorded as a `note` event listing the
  waived question ids, because a waiver that leaves no trace is the same class
  of silent decision the pipeline exists to prevent.
- `scope` + `reject` with `Reason` → `SCOPING`, `ScopeRounds++`,
  `HumanRejectReason = Reason`.

No new decision verbs. `approve` and `reject` already exist in
`store.Approval.Decision`, the CLI already parses `--reason`, the pending-
approval uniqueness and consume-once semantics already hold per job+gate, and a
gate that is re-entered after a rejection is already supported (the spec gate
does exactly this).

A rejection at this gate **requires** a non-empty `--reason`: unlike a merge
rejection, where "no" is itself information, sending a problem statement back
without saying what is wrong gives the next round nothing to act on. The CLI and
the server both refuse it with that explanation.

---

## 7. Operator surfaces

### 7.1 Gate registration

`review.GateScope = "scope"`, returned by `review.GateFor` for
`AWAITING_SCOPE_APPROVAL`. Because both the engine and every renderer read the
state→gate mapping from that one function, no second switch is introduced.

### 7.2 Gate document

A `renderProblem` section built from the existing `Block` kinds — no new
`BlockKind`, so the exhaustiveness tests
(`TestEveryBlockKindIsRegistered`, `TestEveryBlockKindHasAMarkdownCase`) need no
new cases and the terminal and browser cannot drift:

- Heading and facts (state, branch, worktree, artifacts) — as every document.
- The issue, as submitted.
- The problem statement, in scope, out of scope, success criteria.
- Assumptions, each with its basis.
- Open questions, blocking ones first, each with `why_it_matters`.
- A one-line statement of why the job is parked: the agent is blocked on N
  question(s), or `policies.human_gates` lists `scope`.
- Actions.

### 7.3 Action labels

`renderActions` already switches per gate, so the scope gate's two actions read
in its own vocabulary over the same underlying decisions:

| Decision | Label | Effect |
|---|---|---|
| `approve` | Proceed as scoped | Planning starts from this problem statement; any open questions are waived. |
| `reject` | Answer & re-scope | Your text is delivered to the scoping agent, which revises the problem statement. |

CLI equivalents printed alongside, as today:

```
sdlc approve JOB-7
sdlc reject JOB-7 --reason "Q1: only the SMS parser. Q2: keep the old format readable."
```

`sdlc status` gains its `ACTION NEEDED` line for the new state, matching the
existing spec and code gate lines.

---

## 8. Configuration (amends SPEC §12)

```yaml
states:
  SCOPING:
    agent: opus
    timeout: 15m
    disallowed_tools: [Edit, NotebookEdit]

policies:
  scoping: on          # on | off. off restores CREATED → PLANNING exactly.
  human_gates: [scope] # optional; empty by default

limits:
  max_scope_rounds: 2
```

Defaults: `states.SCOPING` = agent `opus`, timeout 15m; `policies.scoping` =
`on`; `limits.max_scope_rounds` = 2.

`opus` because this is a single invocation whose output every later state
inherits, and the failure it prevents is expensive. It is the wrong place to
save money.

`disallowed_tools` denies `Edit` for the reason the reviewers do — the agent's
only write is its own artifact, and immutability of the tree is additionally
enforced mechanically by `discardTreeChanges`. `Write` is retained because the
agent must emit `.sdlc/problem.json`. `Bash` is retained for the reason the
verifiers keep it: scoping "Fix 1.0.6 issues" means reading what 1.0.6 actually
changed, and `git log` is how that question is answered. Read-only exploration
is the whole job of this state, and a state that cannot explore would produce
exactly the ungrounded restatement it exists to prevent.

`policies.scoping` is validated as `on|off`. `policies.human_gates` accepts
`scope` alongside `spec|code|merge|release`.

---

## 9. Compatibility

- **In-flight jobs.** Only `handleCreated`'s return value changes. A job in any
  state past `CREATED` never enters `SCOPING` and needs no migration.
- **Existing artifacts.** No existing schema changes; `spec/1`, `plan/1`,
  `review/1`, `implementation/1`, `analysis/1`, `verdict/1` are untouched. An
  old job's artifacts remain readable.
- **Existing configs.** A config with no `SCOPING` entry under `states:` gets
  the default, via the same `applyComputedDefaults` loop over `AgentStates` that
  backfills every other state. A config that sets `policies.scoping: off`
  reproduces current behaviour exactly.
- **`PLANNING` without a problem.** `handlePlanning` stages `problem.json` when
  it exists and renders the Scoped problem section only then, so a job created
  with `scoping: off` — and any resumed pre-upgrade job — plans as it does
  today.
- **Database.** `Counters` is stored as JSON; a new optional field with
  `omitempty` reads back as zero on existing rows. No schema migration.

---

## 10. Testing & acceptance

### 10.1 Artifact

- Each cross-field rule rejected with a message naming the field: `blocked` with
  no blocking question; a blocking question with `clarity: clear`; `assumed`
  with an empty `assumptions`; an unknown `clarity`; duplicate question ids;
  empty `success_criteria`.
- A valid artifact of each `clarity` value loads.
- BOM- and fence-wrapped input loads, as for every other artifact.

### 10.2 Engine

- **Clear path.** `clarity: clear`, no gate configured → `CREATED → SCOPING →
  PLANNING`, one scoping invocation, `problem.json` harvested, tree clean.
- **Blocked path.** `clarity: blocked` → parks at `AWAITING_SCOPE_APPROVAL`
  regardless of `human_gates`; `reject --reason` → re-enters `SCOPING` with the
  reason in the rendered prompt and `ScopeRounds == 1`; a second, clear scoping
  → `PLANNING`.
- **Waiver.** `clarity: blocked` + `approve` → `PLANNING`, and the waived
  question ids appear in the event log.
- **Policy gate.** `clarity: clear` + `human_gates: [scope]` → parks; `approve`
  → `PLANNING`.
- **Round cap.** `ScopeRounds` at `max_scope_rounds` → escalates instead of
  dispatching the agent.
- **Rejection without a reason** is refused, at the CLI and at the server.
- **`scoping: off`** reproduces `CREATED → PLANNING` with no scoping invocation.
- **Planning binding.** With a `problem.json` present, the rendered planning
  prompt contains the problem statement and the out-of-scope list; without one,
  it renders as it does today.
- **Tree cleanliness.** A scoping agent that edits a source file has the edit
  discarded and does not commit.

### 10.3 Review

- Golden document for the scope gate, in both the blocked and policy-gate
  cases, covering the terminal and browser renderers through the existing
  shared-document tests.

---

## 11. Repository layout (amends SPEC §14)

| File | Change |
|---|---|
| `internal/config/config.go` | `StScoping`; `AgentStates`; default `States` entry; `Limits.MaxScopeRounds`; `Policies.Scoping`; `scope` accepted in `human_gates`; validation of both |
| `internal/engine/states.go` | `SScoping`, `SAwaitScope`; `isParked`; `resumable` |
| `internal/engine/handlers.go` | `handleScoping`; `handleCreated` routing; `handlePlanning` stages `problem.json` |
| `internal/engine/engine.go` | dispatch case; `handleGate` scope cases |
| `internal/artifact/artifact.go` | `Problem`, `Assumption`, `OpenQuestion`; `LoadProblem` |
| `internal/review/review.go` | `GateScope`; `GateFor`; `gateTitle`; `renderProblem`; `renderActions`; golden testdata |
| `internal/store/store.go` | `Counters.ScopeRounds` |
| `internal/prompt/prompt.go` | `defaults[StScoping]` |
| `prompts/scoping.md` | new |
| `prompts/planning.md` | Scoped problem section |
| `prompts/design_review.md` | scope-drift finding |
| `internal/cli/cli.go` | `ACTION NEEDED` line; reject-requires-reason at the scope gate |
| `sdlc.example.yaml` | `SCOPING` state, `policies.scoping`, `limits.max_scope_rounds` |
| `docs/SPEC.md` | §3.1, §3.2, §3.3, §5.1, §6.3, §12, §14 |
| `docs/running.md` | the new gate and what to do at it |

---

## 12. Open items

None. Every decision above is settled; §2.2 records what was deliberately left
out and why.
