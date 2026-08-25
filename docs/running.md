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
| `AWAITING_SCOPE_APPROVAL` | The scoping agent raised a blocking question, or `policies.human_gates` lists `scope` | `sdlc review JOB-1`, or `sdlc approve JOB-1` / `sdlc reject JOB-1 --reason "..."` |
| `AWAITING_SPEC_APPROVAL` | The spec and plan passed design review, and `policies.human_gates` lists `spec` | `sdlc review JOB-1`, or `sdlc approve JOB-1` / `sdlc reject JOB-1 --reason "..."` |
| `AWAITING_CODE_APPROVAL` | The implementation passed code review, and `policies.human_gates` lists `code` | `sdlc review JOB-1`, or `sdlc approve JOB-1` / `sdlc reject JOB-1 --reason "..."` |
| `AWAITING_MERGE_APPROVAL` / `AWAITING_RELEASE_APPROVAL` | Human gate | `sdlc approve JOB-1` / `sdlc reject JOB-1 --reason "..."` |
| `ESCALATED` | A round cap, retry cap, invocation budget or policy violation | `sdlc resume JOB-1` (optionally `--to STATE`) |
| `TIMED_OUT` | `limits.max_job_duration` or a per-state timeout | `sdlc resume JOB-1` |
| `BLOCKED_ON_QUOTA` | The agent CLI was rate-limited | Nothing — it resumes itself after `quota_backoff` |
| `BLOCKED_ON_NETWORK` | The agent CLI could not reach its API at all (DNS, refused connection) | Nothing — it resumes itself after `transport_backoff`. The hold reason quotes the CLI's own error |

`BLOCKED_ON_NETWORK` is the same idea for a backend that could not be reached.
Both spend no retry budget: `limits.max_agent_retries` exists to bound an agent
getting the work wrong, and neither a rate limit nor a dead network is that. A
transient outage used to burn all three attempts in a few minutes and escalate
the job; now it waits and picks up where it left off.

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

---

## 8. The web UI

`sdlc serve` puts a web UI in front of the same database every other subcommand
writes. It is a second interface to the engine, not a second engine: a decision
recorded in the browser is the same `approve`/`reject`/`resume`/`cancel` row
`sdlc approve` writes, and the running engine picks it up on its next tick.
Nothing about the pipeline changes when it is not running.

```bash
sdlc serve                     # binds server.listen, default 127.0.0.1:7777
sdlc serve --addr 127.0.0.1:0  # let the kernel pick a free port
sdlc serve --v                 # one console line per request
```

It prints what it actually bound, not what you asked for — with port 0, or with
`localhost` in the config, those differ:

```
sdlc serve listening on http://127.0.0.1:7777/
  loopback only, and it has no authentication: anyone who can reach this address can approve a merge.
  to reach it from another machine, forward the port: ssh -L 7777:127.0.0.1:7777 host
  press ctrl-c to stop
```

Ctrl-C shuts it down and waits up to 5s for requests already in flight, so an
approval POST mid-write is not cut off.

The UI is read-and-decide: the job list and job detail, the gate document with
the approve/reject form, the diff (unified or side-by-side, plus the raw patch),
the event log, the artifacts, the logs, the prompts as sent, a submit form, and
a read-only config page. Everything it serves is embedded in the binary — no
CDN, no fonts, no analytics — so it renders on a machine with no network at all.
Config *editing* is not offered anywhere: the file on disk stays the only way to
change settings.

The gate page is laid out as a review desk rather than a document with a form
under it. The queue of everything waiting on a human is a rail down the left,
oldest first, so deciding one gate does not mean going back to the list to find
the next; the document is in the middle with the pipeline strip above it, so you
can see which two states the gate sits between; and the decision — the findings,
the gate facts, the note field and the buttons — is a panel on the right that
stays put while the document scrolls. A copy of Approve and Request changes sits
in a bar pinned under the document, so the decision is never more than a glance
away from the thing being decided.

Two conveniences come with it, and both are conveniences only. Ticking a finding
off the checklist in the panel is a note to yourself: nothing is posted, and the
approval row is the same row whether every box is ticked or none is — what it
buys is a count in the bar, which is the difference between having read four
findings and having scrolled past them. And `Ctrl-K` (or `Cmd-K`) opens a search
over the job list that jumps straight to a job, waiting ones first. Neither
exists without JavaScript, and neither is the only way to do anything.

The job list is one dense grid, split as it always was into "waiting on you" and
everything else, with a bar per row showing how far through the pipeline the job
has got. A job the engine has parked on a backoff timer after a rate limit is
counted and filtered as `blocked` rather than as running: nothing is being
dispatched for it and nobody is being asked to decide anything about it.

The filter chips above the grid are ordinary links — `?show=waiting`,
`?show=failed`, `?show=all` — so a filtered list survives a reload and can be
pasted to somebody else. The default hides finished and cancelled jobs; `all`
shows them.

### What it does not protect against

**There is no authentication of any kind.** No login, no users, no roles, no
tokens. Anyone who can open a TCP connection to that port can approve a merge,
reject work, cancel a job — which deletes its worktree, losing anything not
committed — and read every artifact, log, prompt and gate document the
orchestrator holds. The trust boundary is the machine, exactly as it is for the
CLI.

What does exist is a set of checks that keep *that* boundary from being widened
by accident or by a web page:

- **The listener is loopback-only.** `server.listen` and `--addr` are both
  validated, `localhost` is rewritten to `127.0.0.1` before the bind rather than
  resolved at bind time, and anything routable is refused — with `--addr` as a
  usage error (exit 2), in the config as a load failure.
- **The `Host` header is allowlisted.** Only `127.0.0.1`, `::1` and `localhost`
  are answered; anything else gets `421 Misdirected Request` with no application
  content in the body. This is what stops DNS rebinding, where a page on an
  attacker's domain re-resolves that name to `127.0.0.1` and becomes same-origin
  with this server. A near-miss such as `127.0.0.1.evil.example` is a different
  host and is refused too.
- **Every POST carries a CSRF token**, double-submit against an `HttpOnly`,
  `SameSite=Lax` cookie. A POST without a matching pair gets `403` and writes
  nothing.
- **Every file served by name is checked for membership** in the directory it
  is served from — the listing decides, not a cleaned prefix — so a traversal
  attempt reaches no file and symlinks do not resolve.

Those defend a single operator on their own workstation. They are **not** a
substitute for authentication and do not become one: put this on an interface
another machine can reach and you have published an approve button. Do not run
it behind a reverse proxy on a shared host, and do not "temporarily" bind
`0.0.0.0` — the config refuses to, and that refusal is the feature.

### Reaching it from another machine

Forward the port over SSH and open it locally:

```bash
ssh -L 7777:127.0.0.1:7777 host
# then browse http://127.0.0.1:7777/ on your laptop
```

This is the sanctioned shape because it puts the authentication where there
already is some: SSH decides who gets to the port, with keys you already manage,
and the server still sees only a loopback connection with a loopback `Host`. The
UI gains no privilege it did not have, and nothing new is exposed if the tunnel
is not up.

### Three things to know while reading it

**The config page redacts, but assume it shows everything.** Values whose key or
flag name looks credential-shaped, and values matching credential-shaped
environment variables, are replaced with `[redacted]`, and the page says when it
has redacted something. That is a courtesy, not a boundary: the UI is a window
onto everything the orchestrator knows — issue text, agent output, prompts,
diffs, paths — and anyone who reaches it reads all of it.

**A page that cannot prove it is current disables its approve buttons.** Each
page keeps a live connection with a heartbeat; when the heartbeat stops, or when
the stream reports that the job has moved to another state, the decision buttons
grey out with the reason written next to them and the fix is to reload. That is
a courtesy too — the server re-reads the job on every decision POST and answers
`409` if its state no longer matches the state the page was rendered from, so a
decision from a stale page cannot land either way. Without JavaScript every page
still renders and every form still works; only the staleness signal is missing,
and each page carries a plain refresh link in its place.

**Gate documents render live, with the saved copy as a fallback.** The gate page
runs the same render `sdlc review` does, against the worktree as it stands. When
that is impossible — the worktree was cleaned up, the target was renamed out of
the config — it falls back to the snapshot written when the job parked, and says
on the page which of the two you are reading.
