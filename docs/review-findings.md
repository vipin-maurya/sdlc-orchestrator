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
| Pure renames and copies reported a path not in the repository: `a/`/`b/` stripped from four header lines that carry no prefix | `diff.go:575` | `bddc4a1` |
| A filename ending in a space was destroyed by `TrimRight` | `diff.go:570` | `bddc4a1` |
| `Split` hung forever on a `Kind` outside the enum | `split.go:34` | `bddc4a1` |
| The 70% intra-line guard was unpinned — 0.99, 0.35, and a flipped comparison all passed | `split.go:20` | `bddc4a1` |
| `git diff` ran with the user's prefix config, so `diff.noprefix` mangled every path on that machine alone | `gitx.go:305` | `bddc4a1` |

Each fix carries a test that fails without it, verified by reverting the fix and
observing the failure.

---

## Open

**Nothing outstanding in `internal/diff`.** The three items previously listed
here were re-validated by execution and fixed; see below.

*Closed since this was written:*

- **`File.flag` was quadratic and its "bounded growth" comment was false.**
  Re-measured before fixing: 268 KB of distinct junk headers took **13.1s** and
  produced **768 KB** of `Malformed`, growing 16× for every 4× of input. The
  dedup only suppressed a message repeating verbatim, and two callers built
  theirs from the line or the numbers they read, so the `Contains` scan walked
  everything already recorded. `Malformed` is now capped at 1 KiB with the
  remainder counted, and the raw line is clipped before it enters a message:
  1.1 MB now parses in **15ms** with `Malformed` bounded at ~1 KB.
- **An out-of-range hunk count folded to 0 in silence**, leaving a deleted line
  with `OldNo == 0` — the value `Line`'s doc reserves for "does not exist on
  that side". It is now flagged: `hunk header old start "999…" is not a usable
  number`.
- **Five surviving mutants now have fixtures**, each verified to fail with the
  code broken and pass with it restored:
  - T1, the binary-payload exit — `binary_literal.patch` orders the text file
    *before* the payload, so nothing followed it and the branch had 0% coverage
    despite a test comment claiming otherwise. `binary_first.patch` puts the
    payload first.
  - T3, `Parse`'s hand-back — killed by T1's fixture, not by a short-hunk one
    (see below).
  - T4, a blank context line whose leading space a mail client stripped.
  - T5, a chmod *and* an edit, which was badged `StatusModeOnly` — a badge the
    diff page renders as "the contents did not change", about a file whose
    contents did.
  - T6, a quoted path containing an escaped quote on a mode-only change, where
    the `diff --git` line is the only source of the path.

**T8 is an equivalent mutant, not a gap.** Widening `stripSrcPrefix` from
`a/`/`b/` to any single letter changes no real input: git always emits `a/` and
`b/`, `DiffPatchSince` now pins `--src-prefix`/`--dst-prefix`, and the four
rename/copy lines no longer strip a prefix at all. No fixture can distinguish
it, so none was written.

**One correction to the original finding.** T3 claimed the short-hunk path was
what `Parse`'s hand-back protected. It is not: with the hand-back removed,
`short_midpatch.patch` still parses into two correct files, because the header
state recovers on its own from the lines that follow. The hand-back is
load-bearing for the binary payload, which is what actually fails without it.
The short-hunk test was also asserting only that `Malformed` was non-empty,
which passed with the branch deleted because `closeHunk` flags the same file
with a different sentence; it now asserts that branch's own message.

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
