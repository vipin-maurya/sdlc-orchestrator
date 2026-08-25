# Run issues — live operation notes

Issues found by *running* the orchestrator, as opposed to reading it. Started
2026-08-24 while driving the first job against the `sdlc` target (this repo
itself). Kept in the same spirit as `improvement-plan.md`: what actually went
wrong, why, and what would have to change.

Each entry says whether it is **pre-existing on main** or introduced by the
branch under test, because that decides which diff the fix belongs in.

---

## R1 — The engine caches config at startup; the web UI does not share it

**Pre-existing on main. Hit at 09:02 on 2026-08-24.**

`sdlc run` and `sdlc serve` are separate processes, each calling `config.Load`
once at startup and holding the result. Adding a target to `sdlc.yaml` and
restarting only the web server left the UI offering a `sdlc` target that the
engine had never heard of. The submit succeeded, and the job escalated one tick
later with `unknown target "sdlc" (configured: expensetracker)`.

`step()` fails at `e.cfg.Target(j.Target)` before `handleCreated` runs, so no
worktree is provisioned and no agent invocation is spent — the job is simply
dead on arrival.

Why it matters: the UI's whole purpose on that page is to say what this config
will do. Offering a target the engine cannot accept is the one thing it must
not do. The submit path has every opportunity to catch this — it is writing a
row the engine will read — and does not.

Candidate fixes, cheapest first:
- Validate the target against the loaded config at submit time and refuse with
  a message, rather than accepting a row that is guaranteed to escalate.
- Have the engine re-read config on a SIGHUP or a file mtime change.
- Have both processes read config through one owner, so "restart the UI" and
  "restart the engine" stop being two separate acts with one shared input.

## R2 — A job that escalates during CREATED can never be resumed

**Pre-existing on main. Same shape as the SCOPING round-cap defect fixed on
`feat/add-planning-agent`.**

`hold()` records `PrevState`, and `sdlc resume` with no `--to` targets it.
`CREATED` is not in `resumable` (`SCOPING, PLANNING, DESIGN_REVIEW, …`), so any
job that escalates inside CREATED — unknown target (R1), a branch that collides
with `protected_branches`, a worktree that fails to provision — records
`PrevState = CREATED` and is refused by the one command whose purpose is
clearing holds.

`--to SCOPING` does not rescue it either: the escalation happened before
`handleCreated` provisioned anything, so there is no worktree for the resumed
state to run in. Confirmed on JOB-1: `.worktrees/JOB-1` does not exist and
`git worktree list` shows only the main checkout.

The only exit is `sdlc cancel` and resubmit, which is fine when zero work has
been done but is not what the interface claims to offer.

This is the same defect class as the scope-gate one already fixed: a job held
*from* a state that `resumable` excludes. Worth fixing generally rather than
per-state — e.g. hold() recording an explicit resume target instead of relying
on `PrevState` happening to be runnable.

## R3 — `sdlc resume` reports success while the engine silently refuses

**Pre-existing on main. Found while trying to recover from R2.**

```
$ sdlc resume JOB-1
JOB-1 resume requested; the engine will act on its next tick
$ echo $?
0
```

The engine then logged, and the operator never saw:

```
JOB-1: refusing to resume into "CREATED" (not a runnable state)
```

The CLI exits 0 because it only writes an approval row; the validation lives in
`handleControls`, one process away. The job page in the web UI shows no error
either — it still reads ESCALATED with the original hold reason, which is
indistinguishable from "the tick has not happened yet".

So the operator's model is "I resumed it and it is working on it", and the
truth is "nothing will ever happen". For an unattended overnight run this is
the worst possible failure mode: silent, and it looks like progress.

At minimum the refusal should land somewhere the operator reads — an event on
the job, a `note` visible on the job page, or the hold reason being rewritten.
Better: `sdlc resume` validates the target state before writing the row, the
way it already validates an explicit `--to` (`--to BOGUS` is rejected properly,
with the valid list; it is only the *implicit* target that goes unchecked).

## R4 — The submit page prints a `sdlc submit` command that does not parse

**Pre-existing on main. Found at 09:30 by copying the command the page offers.**

The submit page's "THE SAME COMMAND" panel renders the target as a positional
argument:

```
sdlc submit expensetracker --file issue.md
```

`cmdSubmit` takes the target as a flag, so that line is rejected outright:

```
error: unexpected argument(s) sdlc; usage: sdlc submit --target <key> --title "..." (--body "..." | --file issue.md)
```

Both sources are wrong, so JavaScript on or off makes no difference:
- `internal/server/templates/submit.html:96` — the server-rendered fallback,
  `sdlc submit &lt;target&gt; --title "..." --file issue.md`
- `internal/server/static/app.js:386` — `var line = "sdlc submit " + t;`

The panel exists so an operator can move from the UI to the CLI, and the one
thing it has to get right is the argument shape. Fix is a one-word edit in each
place (`--target ` before the value), but the two copies are the reason it
drifted: the correct form lives in `cli.usage` and neither of these reads it.

## R5 — The default SCOPING timeout has little headroom

**Observation, not a failure. JOB-2, 2026-08-24.**

SCOPING ran 09:29:09 → 09:40:10, so 11m01s of a 15m timeout — 73% of budget
used on a ~10k-line Go repo with an issue that names no specific file.

The state does the widest reading in the pipeline by design: the prompt tells
it to ground the statement in current behaviour, and the artifact it produced
cites roughly a dozen `file:line` locations across `config`, `engine`, `store`,
`server` and `jobs`. That is the state working correctly, and it is also why it
is the state most likely to run long on a bigger target.

15m is the shipped default (`config.Default()`, `sdlc.example.yaml`). Against
ExpenseTracker, or any repo several times this size, a first run has a fair
chance of hitting the cap — and a timeout here costs the whole state with
nothing harvested. Worth either raising the default or saying in
`sdlc.example.yaml` that this one scales with repo size, unlike the reviewers
which read a bounded diff.

## R6 — A network blip burns the whole retry budget and escalates the job

**Pre-existing on main. Killed JOB-2 in PLANNING, 2026-08-24 10:10–10:16.**
**FIXED 2026-08-24 — see "How R6 was fixed" below.**

The engine already has the right concept for "the backend is temporarily
unusable, do not spend budget on it": `res.QuotaHit` returns `quotaErr`, which
suspends the job to `BLOCKED_ON_QUOTA` with a backoff and consumes no retry
budget (`jobctx.go:403`). A DNS or connectivity failure is the same class of
condition and does not reach it.

`QuotaHit` is `matchesAny(out, s.Backend.QuotaErrorPatterns)` (`agent.go:219`),
and the shipped patterns are:

```
(?i)rate.?limit   (?i)usage.?limit   (?i)overloaded   (?i)quota
```

The claude CLI returned:

```
API Error: Can't reach the API server — check your internet or DNS (ENOTFOUND)
```

with `"terminal_reason":"api_error"`. Nothing matches, so it is treated as an
ordinary agent failure, spends an attempt, and repeats until the budget is out.

What that cost, in 36 minutes:

| time | what |
|---|---|
| 09:40:10 | PLANNING attempt 1 starts |
| 10:10 | attempt 1 gives up at the 30m state timeout, log `015` is 0 bytes |
| 10:10–10:13 | attempt 2, ENOTFOUND after 178.9s |
| 10:13–10:16 | attempt 3, ENOTFOUND after 187.5s |
| 10:16:27 | `PLANNING -> ESCALATED`, job parked |

The job then sat at the hold gate for 10h42m. It had already spent 11 minutes
of Opus producing a good `problem.json`; all of that was stranded behind a
transient outage that had cleared long before anyone looked.

This is the exact scenario unattended overnight running exists for, and it is
the one the retry budget is least suited to: three attempts inside six minutes
is not a retry policy for a network outage, it is a way of converting a blip
into a dead job.

Two separate fixes:
- Classify transport failures like quota: suspend with a backoff instead of
  spending attempts. Either add patterns (`ENOTFOUND`, `ECONNREFUSED`,
  `ETIMEDOUT`, `can't reach the API`) or — better, since patterns are a
  fragile way to read a structured field — key off the CLI's own
  `terminal_reason: "api_error"`, which is already in the JSON result.
- Space the attempts. Back-to-back retries make the budget meaningless against
  anything lasting more than a few minutes.

### R6b — the hold reason reports the symptom, not the cause

The operator-visible message was:

```
PLANNING failed after 3 attempts: post-condition:
open ...\.worktrees\JOB-2\.sdlc\spec.json: The system cannot find the file specified.
```

"spec.json is missing" is what the orchestrator noticed; the network being down
is why. The cause was in `logs/047_PLANNING_opus.log`, which the operator has to
know to open. Escalating on a post-condition failure should carry the agent's
own error when there is one — otherwise the first hypothesis is "the agent wrote
the wrong file" or "the prompt is broken", and both are wrong.

### How R6 was fixed

A new suspension state, `BLOCKED_ON_NETWORK`, sitting beside `BLOCKED_ON_QUOTA`
and behaving the same way: park, back off, resume into the interrupted state,
spend no retry budget. A separate state rather than a reuse of the quota one,
because an operator debugging a DNS outage must not be told the job is waiting
out a rate limit.

- `agent.Result.Unreachable`, decided by `classifyTransport`.
- `unreachableErr` in the engine, handled alongside `quotaErr`.
- `backends.*.transport_error_patterns` and `transport_backoff` (default 2m,
  against quota's 30m — a network blip is usually over in a minute).
- `hold_reason` now carries the backend's own sentence, which is R6b.

**What the classification searches turned out to matter more than the
patterns.** The obvious implementation — match the errno list against the
agent's stdout — has a false positive that this repository walks straight into:
an agent working on sdlc-orchestrator writes about `ECONNREFUSED` and socket
timeouts as ordinary subject matter, and matching the transcript would suspend
a job because the agent used the word. A test written for the patterns caught
it. So the decision is scoped:

1. `terminal_reason` settles it both ways when the CLI reports one — `api_error`
   means unreachable, `max_turns` or `refusal` means the CLI got an answer and
   the run failed on its own merits, whatever the message says about sockets.
2. With an envelope but no `terminal_reason` (agy), only the CLI's own `result`
   field is searched — never the transcript.
3. With no envelope at all the CLI died before it could explain itself, so the
   raw output is all there is.

A run that exited 0 is never unreachable.

Quota still wins over transport where both match: it has the longer,
deliberately-tuned backoff, and treating a rate limit as a network blip would
retry it far too eagerly.

Not fixed, and deliberately: the retry attempts are still back-to-back. With
transport failures no longer spending the budget, the case for spacing them is
much weaker — the remaining scenario is an agent that fails three times in a row
for its own reasons, where retrying immediately is the right thing.

## R7 — Running the orchestrator on its own repo breaks its own test suite

**Pre-existing on main. Found 2026-08-24 while JOB-2 held a worktree open.
FIXED in the same sitting.**

`TestCreateJobHasOneNonTestCaller` walks the repo asserting that exactly one
non-test file calls `store.CreateJob`. It skipped `.git` and nothing else. With
JOB-2 in flight against the `sdlc` target, `.worktrees/JOB-2/` is a full second
checkout of the repository, so the walk found two:

```
store.CreateJob callers = [
  ..\..\.worktrees\JOB-2\internal\jobs\submit.go: if err := st.CreateJob(job); ...
  ..\..\internal\jobs\submit.go:                  if err := st.CreateJob(job); ...
] want exactly internal/jobs/submit.go
```

The invariant was never violated. It was satisfied twice, by the same file, and
the test read that as failure — a red suite caused by a job being in progress,
which is the least useful kind of red there is. It also clears itself when the
job ends and the worktree is cleaned, so it looks intermittent.

Fixed by skipping `.worktrees` in the walk.

Worth noting beyond the one test: this repository's style leans on
source-scanning invariants, and *every* one of them acquires this failure mode
the moment the orchestrator is pointed at its own tree. This is currently the
only tree-walking test (checked: it is the sole `filepath.WalkDir` in any
`_test.go`), but the next one will need the same exclusion. A shared helper for
"walk this repo's real source" would stop it recurring.

Note the asymmetry that hides it: a job's own `go test ./...` runs *inside* the
worktree, where no `.worktrees/` exists, so the pipeline never sees this. Only
the human running tests in the host checkout does.

## R8 — `max_job_duration` counts the time a job spends waiting for a human

**Pre-existing on main. Killed JOB-2 a second time, 2026-08-24 22:34.**

`tick` checks the job-age budget as `now.Sub(j.CreatedAt) > max_job_duration`
(`engine.go:164`) — wall clock from submit, with nothing subtracted. SPEC §12
says exactly that, so it is behaving as specified. The specification is what is
wrong.

JOB-2's actual life:

| | |
|---|---|
| submitted | 09:29 |
| SCOPING, real work | 11 min |
| PLANNING, killed by R6 | ~36 min |
| **parked at a hold gate, waiting for a human** | **~12h45m** |
| resumed | 22:34 |
| TIMED_OUT, 0 seconds later | 22:34 |

Roughly 47 minutes of orchestrator work inside a 12-hour budget, and it died on
resume because a person was asleep for the rest of it.

This collides directly with the product's own design. The pipeline parks jobs
at human gates on purpose — `scope`, `spec`, `code`, and the always-on `merge`
and `release` — and `gate_reminder_interval` exists precisely because a job is
expected to sit waiting long enough that the operator scrolls past the notice.
A budget that runs while nobody is being asked to do anything punishes the
operator for the gate the tool asked them to attend.

It is also silently destructive: the job passes the budget while parked, so the
kill lands on the *resume*, and the operator's action appears to be what killed
it.

The fix is to spend the budget only on time the orchestrator is working. The
contained version: accumulate parked/held/suspended time in `transition()` —
when leaving a state where `isParked`, `isHeld` or `isSuspended` was true, add
`time.Since(j.StateEnteredAt)` to a counter — and check
`now.Sub(j.CreatedAt) - parked` instead. The budget then means "12h of the
orchestrator working on your job", which is what an operator reads it as, and
is the only reading under which the number is theirs to choose sensibly.

BLOCKED_ON_QUOTA and the new BLOCKED_ON_NETWORK need the same treatment for the
same reason: a 30-minute quota backoff should not eat a job's lifetime either.

**Not fixed here** — it needs a `store.Counters` field and a migration, and it
sits outside what this sitting set out to change. Worked around locally by
raising `limits.max_job_duration` in the gitignored `sdlc.yaml` so the run
could continue; that is a workaround and not the answer.

## R9 — The agy backend cannot send a prompt longer than 24,000 characters

**Pre-existing on main. Killed JOB-2 in IMPLEMENTING, 2026-08-24 23:19.
FIXED 2026-08-25 by prompt-by-file — see below.**

`runAgy` (`agent.go:128`) puts the prompt in argv when it is short and on stdin
when it is long:

```go
argv := []string{s.Backend.Binary, "-p"}
if len(s.Prompt) <= maxArgPrompt {          // 24_000
    argv = append(argv, s.Prompt)
}
argv = append(argv, "--output-format", "json")
...
if len(s.Prompt) > maxArgPrompt { stdin = s.Prompt }
```

On the long branch `-p` is emitted with nothing attached, so the command
becomes `agy -p --output-format json …` and `-p` takes `--output-format` as its
prompt. agy says so itself:

```
Error: -p took "--output-format" as its prompt, so the intended prompt was left
as an argument and ignored.
```

JOB-2's IMPLEMENTING prompt was 38,867 bytes — it inlines the spec and a plan
with 29 acceptance criteria. Three attempts failed in about one second total
and the job escalated.

**The stdin path has never worked.** It exists specifically for long prompts and
is broken for every one of them; short prompts work only because they take the
other branch. Every agy state — IMPLEMENTING and FIXING — fails the moment a
plan gets big, which is to say on exactly the jobs that matter most.

Checked against the CLI directly, so the options are not guesses:

- `agy --output-format json -p` with the prompt on stdin → `flag needs an
  argument: -p`. Moving `-p` last does not fix it.
- `--help` confirms `-p`, `--print` and `--prompt` are the same flag and all
  require a value. There is no flag that reads a plain prompt from stdin.
- The only stdin route is `--input-format stream-json`, which requires
  `--output-format stream-json` — a different response format that the agy
  adapter's envelope parsing does not currently read.

So there are three real options, none of them one-line:

1. **Prompt-by-file.** Write the prompt into the worktree's git-excluded
   `.sdlc/` and pass a short `-p` telling the agent to read it. Smallest change
   and it matches what the `exec` backend already does with `{prompt_file}`. It
   does change the agent contract: the instruction arrives as a file to open
   rather than as the message, which is a real difference in how reliably an
   agent follows it.
2. **stream-json.** Use `--input-format stream-json --output-format
   stream-json`. Uses the CLI as intended, but the agy adapter must learn to
   write NDJSON input and read a stream-json envelope.
3. **Raise `maxArgPrompt`.** Does not work: 38,867 already exceeds Windows'
   ~32,767-character command-line limit, which is why the stdin branch exists.

Note what does *not* unblock it: moving IMPLEMENTING to the claude backend.
`must_differ_backend_from` requires CODE_REVIEW and FINAL_REVIEW to run on a
different backend than IMPLEMENTING, and both reviewers are claude — so the
swap fails validation. The constraint is right and should not be relaxed to
work around this.

### How R9 was fixed

Option 1, prompt-by-file. `agyPromptArg` returns the value for `-p`: the prompt
itself when it fits, otherwise it writes the prompt to `.sdlc/prompt.md` in the
worktree and returns a short, blunt pointer to it. `-p` is now always given a
value, which is the invariant whose absence caused the bug. The stdin argument
to `invoke` is gone entirely, because agy has no use for it.

`.sdlc/` was chosen over a temp file because the orchestrator already recreates
that directory before every state and already keeps it out of git, so the fix
adds no cleanup path and no way to commit a prompt by accident.

**A heredoc does not solve this, and it is worth writing down why**, because it
is the obvious idea. Heredocs are shell syntax: `sh -c 'agy -p "$(cat)"'` still
expands to an argv element before the shell calls `CreateProcessW`, and the
limit is on the command line the kernel receives. Measured on this machine:

```
30000 chars -> process launches
32000 chars -> process launches
33000 chars -> WinError 206: The filename or extension is too long
39000 chars -> WinError 206
```

Above ~32,767 the process does not start at all, however the text was
assembled. The only ways past it are a file (this fix) or
`--input-format stream-json`, which is a genuine stdin route but forces
`--output-format stream-json` and a new envelope parser.

Tests pin the invariant rather than the wording: `-p` is never empty and never
starts with a dash, at sizes either side of the threshold and at 200KB; the
long-prompt argument stays under 1KB; the file holds the full prompt; and the
short-prompt path is unchanged and writes no file.

Worth recording that R6's new classification got this one right: the failure is
deterministic and permanent, not transport, so the job escalated instead of
suspending. A classifier that had matched "Error:" broadly would have parked a
job that will never succeed.

---

## Fixed before this run

The eight findings from the branch review of `ca337a7` (scoping agent) were
fixed before the run started and are not repeated here. The one worth
remembering in this context is the SCOPING round-cap defect, because R2 is the
same shape and shows the pattern is not confined to one state.
