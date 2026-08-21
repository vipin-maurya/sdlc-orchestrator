# JOB-1 — Redact tokens before they reach the job log

**Approve the spec and plan before any code is written.**

- state: `AWAITING_SPEC_APPROVAL` (waiting 1h)
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

## Spec

Captured agent output is written to the job log verbatim, so a token pasted into an issue body ends up in plain text on disk and in every log tail printed afterwards.

**Approach.** Redact once, at the single point where captured bytes become a log line, rather than at each call site, so a new caller cannot forget to do it.

**Acceptance criteria.**

- A captured line matching a configured token shape is stored as `[redacted]`.
- Redaction runs once per captured line, not once per call site.
- A line matching nothing is byte-identical to what the command wrote.

**Files it expects to touch.**

- internal/execx/execx.go
- internal/execx/redact.go
- internal/config/config.go

**Error paths.**

- A malformed pattern fails config validation at load, naming the key and the regexp error.
- A pattern that matches everything is rejected: a log of nothing but `[redacted]` is not a log.

**Out of scope.**

- Rewriting log files already on disk.
- Redacting the prompts sent to the backend.

**Compatibility concerns.**

- Existing logs keep their current contents; nothing rewrites them, so a token already leaked stays leaked until the file is deleted.

## Plan (3 steps)

- **S1** Add redact.Line and the pattern table it reads.
  - files: internal/execx/redact.go
  - verify: go test ./internal/execx/ -run TestRedact
- **S2** Call redact.Line from the capture path, once, where bytes become a line.
  - files: internal/execx/execx.go, internal/execx/redact.go
  - verify: go test ./internal/execx/ -race
- **S3** Document the pattern table beside the config key that feeds it.

**Risks.**

- A pattern that is too broad redacts a legitimate failure message and makes the log useless exactly when it is needed.

## Design review

The approach is right and the acceptance criteria are testable. Two gaps: one criterion has no owner in the plan, and the error path for an over-broad pattern is described but not planned for.

_source: `design_review.r1.json`_

- **[minor]** Acceptance criterion 2 ("once per captured line") is not observable from outside the package, so nothing in the plan can verify it.
  - recommendation: Either drop it or state it as a property a test can read: one call to redact.Line per line written.
- **[nit]** `internal/config/config.go` The spec names a config key for the pattern table but does not say what an empty table means.

## What happens next

- approve → implementation starts from this plan
- reject → planning runs again with your reason as its instruction

```
sdlc approve JOB-1 [--note "..."]
sdlc reject  JOB-1 --reason "..."
```
