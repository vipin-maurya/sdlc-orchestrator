# Review findings — the server and UI build

Audit of `ea7555e..b74ba56` (the five commits that added `internal/diff`,
the gate-document goldens, the block model, the truncation flag, and
`internal/jobs`): 7,656 lines added, 1,495 of them non-test.

Five reviewers worked disjoint areas. Each was required to *execute* every
hypothesis before reporting it, and to mutate the source and confirm the tests
failed — a test that passes against broken logic is itself a finding. Nothing
below is a reading of the code; every item was run.

---

## The pattern

**Every defect found landed where a comment asserted coverage that did not
exist.**

- The truncation defect sat in the single line that survived mutation E of the
  execx table — the one term no test constrained.
- `renderDiff` had 11 of 22 statements unreached by any golden, because the
  fixture points at a missing worktree to keep the goldens git-free. Two
  independent findings landed inside that gap.
- `TestParseBinary` carries a comment saying "the text file in the same patch
  must survive the binary one" — but the fixture orders the files so nothing
  follows the payload. The branch has 0% coverage and the assertion is vacuous.
- Three comments cited tests that were never written: a CLI test holding an
  error line "to the byte", a `TestEveryBlockKindRenders` failing the build on a
  missing HTML case, and a claim that an agent's context window binds before the
  4 MiB cap matters.

The tests were not wrong. The prose about them was, and prose does not fail CI.
When a comment claims a safety net, the claim is worth checking against `grep`
before trusting it — including, and especially, when you wrote it yourself.

---

## Fixed

| Defect | Where | Commit |
|---|---|---|
| `localhost` accepted verbatim; an `/etc/hosts` entry could bind a **routable** interface on a server with no authentication | `config.go:445` | `f4e19a7` |
| `Truncated` computed before the return paths diverge, so stderr overflow labelled a **complete** stdout capture as clipped — proven with a complete 25 KB patch reported as exceeding 4 MiB | `execx.go:191` | `47356de` |
| `ErrNoTitle` fired only on `""`, so `"   "` became the planner's brief; no size cap (a 50 MiB body was stored); control characters reached the approver's terminal | `submit.go:37` | `f4e19a7` |
| `TitleAndBody` stripped one `#`, so `### H` yielded the title `## H` | `submit.go:77` | `f4e19a7` |
| `AllBlockKinds` hand-maintained, so a kind used on an uncovered path degraded silently with the whole suite green | `blocks.go:32` | `6a91002` |
| `strings.Repeat("#", Level)` panicked on a negative level, in the renderer whose stated ethos is visible degradation | `blocks.go:126` | `6a91002` |
| `para()` appended into its caller's backing array | `blocks.go:278` | `6a91002` |
| Truncation notice said "what follows is its head" from above the branch deciding whether anything follows; `Doc.DiffTruncated` had no reader; the `.diff` artifact was clipped mid-hunk with no marker | `review.go:151,369` | `6a91002` |
| `-update-golden` wrote and returned without comparing, with no CI guard | `golden_test.go:74` | `6a91002` |

Each fix carries a test that fails without it, verified by reverting the fix and
observing the failure.

---

## Open

**`internal/diff` — none of these are fixed.** The reviewer for this area was
cut short before editing anything.

1. **Pure renames report a path that does not exist.** `headerPath` strips an
   `a/`/`b/` prefix from `rename from`/`rename to`/`copy from`/`copy to`, which
   carry **unprefixed** paths. `git mv a/foo.txt a/renamed.txt` parses as
   `foo.txt → renamed.txt`; git's own `--numstat` says `a/{foo.txt =>
   renamed.txt}`. A rename *with* edits self-heals via the later `---` line; a
   pure rename or copy does not. (`diff.go:575`)
2. **`Split` hangs** on a `Kind` outside the enum — the loop advances only
   through the three known branches. Unreachable today; exported API bound for a
   request path. (`split.go:34`)
3. **The 70% intra-line guard is unpinned.** Changing it to 0.99 leaves the whole
   file green. Denominator, comparison strictness, and byte-vs-rune unit are all
   free variables. (`split.go:20`)
4. **A filename ending in a space is destroyed** by `TrimRight(s, " ")`. Legal on
   POSIX, unquoted by git. Deleting the line breaks no test. (`diff.go:570`)
5. **`File.flag` is quadratic** and its "bounded growth" comment is false — it
   dedups only identical messages while two sites emit variable text. 829 KB of
   crafted input takes 79.6 s. Not reachable from git output; reachable by the
   shipped fuzzer. (`diff.go:498`)
6. **An out-of-range hunk count silently becomes 0** with no `Malformed` flag,
   producing a deleted line with `OldNo == 0`. (`diff.go:510`)
7. **Eight surviving mutants**, including the binary-payload exit (T1) and
   `copy from`/`copy to`, which has no fixture anywhere.

**Also open, outside `internal/diff`:** `gitx.go:305` runs `git diff` with no
prefix flags, so a user's `diff.noprefix` or `diff.mnemonicPrefix` silently
mangles every path. Pin `--src-prefix=a/ --dst-prefix=b/` at the call site.

**Also open:** `jobs.Submit` has no callers. `cmdSubmit` carries its own copy and
the two already disagree on the empty-target exit code (2 vs 1) and the
missing-title message, so cutting the CLI over is a behaviour change, not a
refactor.

---

## Verified correct — do not re-litigate

These were tested hard and held. Recorded so the next reader does not spend the
effort again.

- **No data race in the truncation capture.** `lw` and `elw` are separate
  instances with one writer each, and `cmd.Run`'s `Wait` orders the read after
  both writes. 20 `-race` runs with 400 KiB on both streams, *plus a positive
  control* that deliberately shared a writer and did produce a race — so the
  probe was shown able to detect one.
- **The block-model refactor is output-preserving.** 423 scenarios rendered
  through both `77376dc:review.go` and the current code, byte-identical, with a
  negative-controlled harness.
- **Side-by-side pairing is sound.** The per-column invariant holds over 200,000
  random hunks and all 3,280 exhaustive kind-shapes of length ≤ 7; 500,000 UTF-8
  pairs produced zero mid-rune splits.
- **The parser never panics** across every prefix, every single-byte mutation,
  and 20,000 random structural soups. C-quoted unquoting is correct against
  `git diff --name-only -z` ground truth, including non-UTF-8 bytes. The CRLF
  rule matches its specification in all four quadrants, and the three fixtures
  separating them are load-bearing. Filename disambiguation is correct for every
  git-shaped input.
- **`ValidateListen` refuses every non-loopback IP literal** — 50+ forms:
  decimal, hex, octal-ish, short-form, IPv4-mapped, NAT64, zoned, bracketed,
  whitespace-padded, homoglyph, IDN, punycode. The bypass was purely the name.
- **The goldens bite** on the paths they cover: six distinct render-path
  mutations each failed with a precise first-difference line. They pass in
  twelve timezones.
