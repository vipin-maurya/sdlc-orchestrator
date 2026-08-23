# JOB-1 — Redact tokens before they reach the job log

**Approve the release.**

- state: `AWAITING_RELEASE_APPROVAL` (waiting 1h)
- branch: `sdlc/JOB-1`
- worktree: `testdata/golden/worktree-gone`
- artifacts: `testdata/golden/jobs/JOB-1/artifacts`

## Merged

`sdlc/JOB-1` is merged into `main`. Approving runs the release command:

```
scripts/ship.sh --tag v1.4.0
```

## What was merged

Redaction happens once, in the capture path, with the pattern table beside it.

**Files changed.**

- internal/execx/execx.go
- internal/execx/redact.go

**Tests added or changed.**

- internal/execx/redact_test.go

**Plan steps NOT done.**

- S3 — the SPEC section it belongs in is being rewritten by another change; documenting it here would conflict

## What happens next

- approve → the release command runs
- reject → the job completes without releasing (the merge stands)

```
sdlc approve JOB-1 [--note "..."]
sdlc reject  JOB-1 --reason "..."
```
