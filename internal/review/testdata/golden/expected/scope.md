# JOB-1 — Redact tokens before they reach the job log

**Approve the scoped problem before a spec is written.**

- state: `AWAITING_SCOPE_APPROVAL` (waiting 1h)
- branch: `sdlc/JOB-1`
- worktree: `testdata/golden/worktree-gone`
- artifacts: `testdata/golden/jobs/JOB-1/artifacts`

## Issue

> A token pasted into an issue body is written to the job log verbatim.
> From the run on 2026-01-02, every one of these lines reached logs/agent.log:
> 
>   line 01 — captured agent output, copied through unchanged
>   line 02 — captured agent output, copied through unchanged
>   line 03 — captured agent output, copied through unchanged
>   line 04 — captured agent output, copied through unchanged
>   line 05 — captured agent output, copied through unchanged
>   line 06 — captured agent output, copied through unchanged
>   line 07 — captured agent output, copied through unchanged
>   line 08 — captured agent output, copied through unchanged
>   line 09 — captured agent output, copied through unchanged
>   line 10 — captured agent output, copied through unchanged
>   line 11 — captured agent output, copied through unchanged
>   line 12 — captured agent output, copied through unchanged
>   line 13 — captured agent output, copied through unchanged
>   line 14 — captured agent output, copied through unchanged
>   line 15 — captured agent output, copied through unchanged
>   line 16 — captured agent output, copied through unchanged
>   line 17 — captured agent output, copied through unchanged
>   line 18 — captured agent output, copied through unchanged
>   line 19 — captured agent output, copied through unchanged
>   line 20 — captured agent output, copied through unchanged
>   line 21 — captured agent output, copied through unchanged
>   line 22 — captured agent output, copied through unchanged
>   line 23 — captured agent output, copied through unchanged
>   line 24 — captured agent output, copied through unchanged
>   line 25 — captured agent output, copied through unchanged
>   line 26 — captured agent output, copied through unchanged
>   line 27 — captured agent output, copied through unchanged
>   line 28 — captured agent output, copied through unchanged
>   line 29 — captured agent output, copied through unchanged
>   line 30 — captured agent output, copied through unchanged
>   line 31 — captured agent output, copied through unchanged
>   line 32 — captured agent output, copied through unchanged
>   line 33 — captured agent output, copied through unchanged
>   line 34 — captured agent output, copied through unchanged
>   line 35 — captured agent output, copied through unchanged
>   line 36 — captured agent output, copied through unchanged
>   line 37 — captured agent output, copied through unchanged
>
> _(truncated)_

## Scoped problem

execx writes every captured line to the job log verbatim, so a token pasted into an issue body is stored in plain text on disk and reprinted by every log tail afterwards.

**In scope.**

- The single point in internal/execx where captured bytes become a log line.
- Configuration of the patterns that decide what a token looks like.

**Out of scope.**

- Rewriting log files already on disk.
- Redacting the prompts sent to the backend.

**Success criteria.**

- A captured line matching a configured token shape is stored as `[redacted]`.
- A line matching nothing is byte-identical to what the command wrote.

## Assumptions

- Redaction belongs in execx rather than in each caller.
  - _basis: Every log line in the tree already goes through execx.capture; no other package writes to the job log._

## Open questions

- Q1: Should the patterns ship with a default set, or must every install configure its own?
  - _why it matters: A default set redacts on day one but will match things nobody intended; an empty default is safe and protects nobody until someone edits the config._
- Q2: Is the agent prompt log worth a follow-up ticket? (not blocking)
  - _why it matters: It has the same exposure and is explicitly out of scope here._

_The scoping agent is blocked on 1 question(s) and will not write a spec until they are settled. Rejecting with your answers re-scopes; approving waives them and plans on the assumptions above._

## What happens next

- approve → planning starts from this problem statement; any open questions are waived
- reject → scoping runs again with your answers as its instruction

```
sdlc approve JOB-1 [--note "..."]
sdlc reject  JOB-1 --reason "..."
```
