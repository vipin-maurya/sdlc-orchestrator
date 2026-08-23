# JOB-1 — Redact tokens before they reach the job log

**Approve merging this change into main.**

- state: `AWAITING_MERGE_APPROVAL` (waiting 1h)
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

## Final review

Nothing blocks the merge. One advisory finding stands and one was put to a vote and refuted.

_source: `final_review.verified.json`_

- **[nit]** `internal/execx/execx.go` Claimed that the redaction pass allocates a new buffer per line and doubles the cost of a noisy build.
  - recommendation: None: the allocation is the scanner's own, which the loop already reused.
  - verifiers: refuted 1/3 (was minor)
- **[minor]** The pattern table is documented in the config example but not in SPEC §12, which is where an operator looks first.

## Diff

_worktree testdata/golden/worktree-gone is gone; nothing to diff_

## What happens next

- approve → merged into `main` (no fast-forward)
- reject → a fix round starts with your reason; `--cancel` ends the job instead

```
sdlc approve JOB-1 [--note "..."]
sdlc reject  JOB-1 --reason "..."
```
