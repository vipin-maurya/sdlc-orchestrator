# JOB-1 — Redact tokens before they reach the job log

**This job stopped and needs a decision.**

- state: `ESCALATED` (waiting 1h)
- branch: `sdlc/JOB-1`
- worktree: `testdata/golden/worktree-gone`
- artifacts: `testdata/golden/jobs/JOB-1/artifacts`

## Why it stopped

Three fix attempts did not make the unit tests pass. The last analysis classified the failure as `environment`, which this loop cannot fix on its own.

## Last events

```
03:04:05  AWAITING_SPEC_APPROVAL enter        parked for the spec gate
03:04:05  AWAITING_SPEC_APPROVAL approval     approve — start with the pattern table
03:04:05  IMPLEMENTING       enter        
03:04:05  IMPLEMENTING       agent_run    implementer wrote implementation.json
03:04:05  BUILDING           exec_run     go build ./... (exit 0)
03:04:05  TESTING            exec_run     go test ./... (exit 1): --- FAIL: TestRedactsAToken (0.00s) redact_test.go:41: got "ghp_r…
03:04:05  ANALYZING          enter        classified as environment
03:04:05  FIXING             agent_run    fixer ran with the analysis as its instruction and changed nothing
03:04:05  BUILDING           exec_run     go test ./... (exit 1)
03:04:05  ESCALATED          error        fix attempts exhausted (3 of 3)
03:04:05  ESCALATED          enter        waiting for a human
03:04:05  ESCALATED          note         the sandbox has no network; the failing test fetches a module
```

## Diff

_worktree testdata/golden/worktree-gone is gone; nothing to diff_

## What happens next

- resume → the job re-enters the state it stopped in, or the one you name
- cancel → the job ends and its worktree is cleaned up

```
sdlc resume JOB-1 [--to STATE] [--note "do X instead"]
sdlc cancel JOB-1
```
