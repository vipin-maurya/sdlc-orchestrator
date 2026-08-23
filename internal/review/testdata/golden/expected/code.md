# JOB-1 — Redact tokens before they reach the job log

**Approve the implementation before it goes to build and test.**

- state: `AWAITING_CODE_APPROVAL` (waiting 1h)
- branch: `sdlc/JOB-1`
- worktree: `testdata/golden/worktree-gone`
- artifacts: `testdata/golden/jobs/JOB-1/artifacts`

## Implementation

Redaction happens once, in the capture path, with the pattern table beside it.

**Files changed.**

- internal/execx/execx.go
- internal/execx/redact.go

**Tests added or changed.**

- internal/execx/redact_test.go

**Plan steps NOT done.**

- S3 — the SPEC section it belongs in is being rewritten by another change; documenting it here would conflict

## Code review

Round 2 after verification: the compile-per-call finding survived, the ordering claim did not, and two advisory findings stand unvoted.

_source: `code_review.r2.verified.json`_

- **[major]** `internal/execx/redact.go` The pattern table is compiled on every captured line, so a long build pays the compile cost once per line of output.
  - recommendation: Compile the table once when the config loads and hand the compiled form to the capture path.
  - verifiers: confirmed 2/3 (was major)
- **[nit]** `internal/execx/execx.go` Claimed that redaction runs before the line is split, so a token spanning a chunk boundary escapes.
  - recommendation: None: the verifiers read the capture loop and found the split happens first.
  - verifiers: refuted 0/3 (was blocker)
- **[major]** `internal/server/render.go` A finding's description is interpolated with `template.HTML`, so a review that says <script>alert('xss')</script> executes in the reviewer's browser instead of being read.
  - recommendation: Interpolate as a plain string and let html/template escape it; there is no context here that needs raw HTML.
- **[nit]** No test covers the case where the pattern table is empty, which is the shipped default.

## Diff

_worktree testdata/golden/worktree-gone is gone; nothing to diff_

## What happens next

- approve → the change goes to build and test
- reject → a fix round starts with your reason as its instruction

```
sdlc approve JOB-1 [--note "..."]
sdlc reject  JOB-1 --reason "..."
```
