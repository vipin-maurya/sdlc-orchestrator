# Running sdlc unattended

The orchestrator is a long-lived engine. `sdlc run` holds the SQLite database,
the gradle slots and the device pool, and ticks itself every
`orchestrator.poll_interval`; every other subcommand just writes the database
and exits, so you drive a running engine from a second terminal. It runs the
same way on Windows, macOS, and Linux — only the supervisor underneath it
changes.

Its sibling, [autoship](https://github.com/vipinm/autoship), is the opposite
shape on purpose: a scheduled one-shot that wakes, decides, possibly works, and
exits. sdlc *ends* at autoship — the `RELEASING` state runs
`targets.<key>.ship.command` in the main repo — so the two are installed the
same way and supervised differently, and this document is the mirror of
[autoship's `docs/scheduling.md`](https://github.com/vipinm/autoship/blob/main/docs/scheduling.md).

This document covers the four things that have to be true before it can be
trusted: the binary is installed, the config validates, the drain soak was
boring, and the release path was promoted deliberately rather than by default.

---

## 1. Install

Build the binary into a directory that is already on `PATH` — the same tools
directory autoship installs into, so both live side by side:

<details open><summary><b>Windows</b></summary>

```powershell
go build -o C:\tools\sdlc.exe .\cmd\sdlc
```

</details>

<details><summary><b>macOS</b></summary>

```bash
go build -o /usr/local/bin/sdlc ./cmd/sdlc
```

</details>

<details><summary><b>Linux</b></summary>

```bash
go build -o ~/.local/bin/sdlc ./cmd/sdlc
```

</details>

The repo root is not an install location: `/sdlc` and `/sdlc.exe` are
gitignored, and every script and doc here takes the tools path.

Copy [`sdlc.example.yaml`](../sdlc.example.yaml) next to it as `sdlc.yaml` and
edit the `targets` and `backends` sections. It is versioned, contains no
secrets, and every key in it is real — unknown keys are rejected at load.

```bash
cp sdlc.example.yaml sdlc.yaml
```

Point every invocation at it with the global `--config` flag (it precedes the
subcommand), or set `SDLC_CONFIG` once and drop the flag:

```bash
export SDLC_CONFIG=/path/to/sdlc.yaml       # setx SDLC_CONFIG ... on Windows
```

---

## 2. Validate

`validate` is the doctor: it checks the config *and* the environment the engine
will actually run in — that `git` and the agent binaries exist, that the backend
independence rule holds (`CODE_REVIEW`/`FINAL_REVIEW` must not resolve to the
same backend as `IMPLEMENTING`), that the device and gradle settings are
coherent, and that any pinned agent versions still match.

```bash
sdlc --config /path/to/sdlc.yaml validate
sdlc --config /path/to/sdlc.yaml validate --smoke
```

`--smoke` additionally sends one real headless prompt per configured backend.
It costs quota, so it is not the default — but it is the only check that proves
the agent CLIs are authenticated rather than merely present. Run it once per
machine, and again after changing a backend.

---

## 3. The drain soak

Before handing the engine to a supervisor, watch it do one job end to end in the
foreground:

```bash
sdlc --config /path/to/sdlc.yaml submit --target expensetracker \
    --title "Fix transaction sync" --file issue.md
sdlc --config /path/to/sdlc.yaml run --once
```

`--once` drains the runnable work and exits, which is what you want while you
are still reading every transition. In a second terminal:

```bash
sdlc status                 # all jobs
sdlc status JOB-1           # detail, counters, pending actions
sdlc events JOB-1           # agent, model, tokens, exit codes per state
sdlc logs JOB-1 --last      # artifact/log paths + tail of the latest log
```

A state that invokes an agent can run for ten or twenty minutes. It is not
silent while it does: the engine prints a heartbeat naming the state, how long
it has been going, and the last action it saw (`orchestrator.heartbeat_interval`),
and `sdlc status JOB-1` shows the same as a `running:` line. `sdlc logs
JOB-1 --last` tails the log of a state that is **still running** — agent output
is written to the log as it arrives.

For a running commentary rather than a heartbeat, the backend has to stream its
actions: set `backends.claude.stream_json: true` (it adds `--output-format
stream-json --verbose` to the invocation) and each tool call is echoed as one
line. Turn the console echo off with `orchestrator.stream_output: false` for a
headless run; the events and the heartbeat stay either way.

The soak is boring when: the job reaches `AWAITING_MERGE_APPROVAL` without
escalating, `events` shows the states running on the agents you configured, and
the release step at the end was a dry run (§4). Ctrl-C at any point — the engine
is crash-safe by construction, and the next `run` reconciles the worktree
(`reset --hard` + `clean`) to the HEAD recorded on state entry before re-running
that state.

---

## 4. Promotion

Two gates stand between a green job and a published build, and both are
deliberate.

**The approval gates.** `AWAITING_MERGE_APPROVAL` and
`AWAITING_RELEASE_APPROVAL` are human gates; the engine parks there until you
run `approve` (or `reject --reason "..."`, which sends the job back to
`FIXING`). Leave them in place until the pipeline has earned trust — an engine
that is *allowed* to publish unattended still won't until you say so.

**The ship command.** `RELEASING` runs `targets.<key>.ship.command` in the main
repo. It ships pointing at autoship's dry run:

```yaml
    ship:
      command: ["autoship", "dry-run", "--config", "autoship.yaml"]
      resume_command: ["autoship", "resume", "--config", "autoship.yaml"]
      auto_resume_halt: true
      timeout: 60m
```

`autoship dry-run` does everything except the Play commit and the S6 push, so a
full sdlc job can run to `COMPLETED` without anything being published. Promote
it by changing one word — `dry-run` to `run` — once autoship's own soak (see its
`docs/scheduling.md` §3) has been boring too, and not before.

Release credentials never enter this repo: they live in autoship's native
encrypted store (DPAPI / Keychain / Secret Service), and the agents can neither
read them nor invoke the ship command.

---

## 5. Running unattended

Each platform gets a native supervisor. All three installers accept the same
knobs — `--exe`, `--config`, `--name`, `--restart`, `--once`, and a
print-without-installing switch — and all three register a *user*-scoped unit,
because the agent CLIs read per-user credentials and the builds need the user's
Gradle and Android SDK caches.

| | Windows | macOS | Linux |
|---|---|---|---|
| Supervisor | Task Scheduler (at logon) | launchd (LaunchAgent, `KeepAlive`) | systemd (`--user` service) |
| Install script | [`scripts/register-task.ps1`](../scripts/register-task.ps1) | [`scripts/install-launchd.sh`](../scripts/install-launchd.sh) | [`scripts/install-systemd-service.sh`](../scripts/install-systemd-service.sh) |
| Preview flag | `-WhatIf` | `--print-only` | `--print-only` |

This is the one place the two repos diverge, and the divergence is the point:
autoship installs a *timer* because it is a one-shot, sdlc installs a
*keep-alive service* because it is an engine. Same script names, same flags,
same user scoping; different unit body.

<details open><summary><b>Windows</b></summary>

```powershell
powershell -File scripts/register-task.ps1 -ExePath C:\tools\sdlc.exe -ConfigPath C:\Users\you\repos\sdlc-orchestrator\sdlc.yaml -WhatIf
```

Drop `-WhatIf` to register it. Then:

```powershell
schtasks /query /tn sdlc /v /fo list
schtasks /run /tn sdlc
```

</details>

<details><summary><b>macOS</b></summary>

```bash
scripts/install-launchd.sh \
    --exe /usr/local/bin/sdlc \
    --config ~/repos/sdlc-orchestrator/sdlc.yaml --print-only
```

Drop `--print-only` to install and load it. Then:

```bash
launchctl list | grep dev.sdlc.sdlc
tail -f ~/Library/Logs/sdlc/stdout.log
```

</details>

<details><summary><b>Linux</b></summary>

```bash
scripts/install-systemd-service.sh \
    --exe ~/.local/bin/sdlc \
    --config ~/repos/sdlc-orchestrator/sdlc.yaml --print-only
```

Drop `--print-only` to install and start it. Then:

```bash
systemctl --user status sdlc.service
journalctl --user -u sdlc.service -f
sudo loginctl enable-linger $USER    # headless: keep it running after logout
```

</details>

A restart is a normal event, not a recovery procedure — see §3. The engine also
holds `<data_dir>/engine.lock`, so a supervisor that starts a second copy cannot
corrupt the first; the second exits.

---

## 6. When a job stops

The engine never loops silently. Every budget exhaustion lands a job in a state
with a reason attached, and every one of them is cleared by hand:

| State | What happened | How it clears |
|---|---|---|
| `AWAITING_SPEC_APPROVAL` | The spec and plan passed design review, and `policies.human_gates` lists `spec` | `sdlc review JOB-1`, or `sdlc approve JOB-1` / `sdlc reject JOB-1 --reason "..."` |
| `AWAITING_CODE_APPROVAL` | The implementation passed code review, and `policies.human_gates` lists `code` | `sdlc review JOB-1`, or `sdlc approve JOB-1` / `sdlc reject JOB-1 --reason "..."` |
| `AWAITING_MERGE_APPROVAL` / `AWAITING_RELEASE_APPROVAL` | Human gate | `sdlc approve JOB-1` / `sdlc reject JOB-1 --reason "..."` |
| `ESCALATED` | A round cap, retry cap, invocation budget or policy violation | `sdlc resume JOB-1` (optionally `--to STATE`) |
| `TIMED_OUT` | `limits.max_job_duration` or a per-state timeout | `sdlc resume JOB-1` |
| `BLOCKED_ON_QUOTA` | The agent CLI was rate-limited | Nothing — it resumes itself after `quota_backoff` |

`BLOCKED_ON_QUOTA` is deliberately not a failure: a rate-limited run suspends
the job instead of burning a retry budget. Start with `sdlc status JOB-1` for
the reason and the pending action, then `sdlc events JOB-1` for the full trail.

You do not have to go looking for a stopped job in the first place. The run
console announces a gate the moment a job parks on it — what is being decided,
how long it has waited, and the path to the gate document the engine wrote —
and re-announces it every `orchestrator.gate_reminder_interval` (10m by
default; `0` announces once and never repeats) for as long as it stays open.
`sdlc review JOB-1` prints that document in full and, on a terminal, prompts
for the decision; with no job id it walks every job waiting on a human, so one
command clears the queue that built up overnight.

Whatever the stopped state had done is on the branch, not lost in a dirty
worktree: IMPLEMENTING and FIXING commit each step the agent reports finishing
(`... checkpoint S3: ...`), and a state that escalates has its remainder
committed as `... (incomplete)` before the job parks. So `git log` in the job's
worktree is the record of how far it got, and you can commit a fix on the branch
yourself — the orchestrator adopts commits it did not make rather than resetting
over them.

autoship's halt latch is the equivalent idea one layer down, and the two are
wired together: with `ship.auto_resume_halt: true`, a release retry runs
`ship.resume_command` first, so a halt autoship set on a previous tick does not
silently fail every subsequent sdlc release attempt.

---

## 7. Where the two repos meet

```
sdlc: issue ──▶ plan ──▶ implement ──▶ review ──▶ build ──▶ test ──▶ merge
                                                                      │
                                            ship.command in the main repo
                                                                      ▼
autoship: S0 gate ──▶ preflight ──▶ build+test ──▶ assemble ──▶ Play ──▶ tag
```

sdlc decides *whether* a change is fit to merge; autoship decides *whether the
repo state is fit to ship* and does the shipping. Neither reaches into the
other: sdlc invokes autoship as a command with an exit code, and autoship has no
idea sdlc exists.
