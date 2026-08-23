# Problem scoping Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `SCOPING` agent state between `CREATED` and `PLANNING` that turns a raw issue into a validated `problem.json` — scope boundaries, success criteria, assumptions, open questions — and parks at a new `scope` gate when the agent is genuinely blocked.

**Architecture:** `SCOPING` is an ordinary agent state: it takes part in prompt rendering, retry, quota, timeout and crash-resume machinery unchanged, and its post-condition is `artifact.LoadProblem`. Routing out of it is decided by a `clarity` field that cross-field validation holds to the rest of the artifact, so an agent cannot raise a blocking question and route itself past the human. The gate reuses the existing approval row, decision verbs, and `review.GateFor` mapping; no new gate machinery is introduced.

**Tech Stack:** Go 1.26, stdlib only. SQLite via the existing store. No new `go.mod` entries.

Source spec: [`docs/scoping-spec.md`](scoping-spec.md) v1.0, as amended by A1/A2/A3 below.

## Global Constraints

- **Zero new `go.mod` entries.** Stdlib only, as everywhere else in this repo.
- **No existing artifact schema changes.** `spec/1`, `plan/1`, `review/1`, `implementation/1`, `analysis/1`, `verdict/1` are read-only for this work. An old job's artifacts must stay loadable.
- **No database migration.** `Counters` is stored as JSON; the one new field is `omitempty` and reads back as zero on existing rows.
- **`policies.scoping: off` reproduces current behaviour exactly** — `CREATED → PLANNING`, no scoping invocation. Held by a test in Task 5.
- **Every task ends green:** `gofmt -l .` empty, `go vet ./...` clean, `go test ./... -race` passing.
- **The state→gate mapping lives only in `review.GateFor`.** Never add a second switch; the CLI, the server and the engine all read it.
- Commit messages follow the repo's existing style: imperative subject, a body explaining *why*. No AI attribution trailers.

---

## Amendments in force (override the spec)

- **A1 — No custom button labels.** Spec §7.3 gives the scope gate's actions the labels "Proceed as scoped" and "Answer & re-scope". `review.Action.Decision` *is* the button text and also the API verb (`renderActions`, review.go:464), so a custom label needs a new `Label` field on the shared block model plus fallbacks in both renderers and their golden tests. The operator-facing intent — knowing what each button does in scope-gate vocabulary — is fully served by the per-gate `Effect` spans, which is exactly what every other gate uses. Buttons stay `approve`/`reject`; §7.3's table becomes `Effect` text. Task 3.
- **A2 — Reason-on-reject already exists.** Spec §6.3 requires a rejection at the scope gate to carry a non-empty reason. Both surfaces already enforce this for *every* gate: `cli.cmdDecision` refuses with `error: reject requires --reason` ([cli.go:419](../internal/cli/cli.go:419)), and `server.handleReject` refuses an empty reason field ([actions.go:104](../internal/server/actions.go:104)). The requirement stands and is covered by a regression test in Task 6; no new enforcement code is written.
- **A3 — The fake agent is shared test infrastructure.** `runFakeAgent` in `internal/engine/engine_test.go` dispatches on role substrings found in the *rendered prompt*. `prompts/scoping.md` must therefore contain the literal phrase `scoping agent`, and Task 5 adds a case plus env knobs to the fake. This is the one piece of mutable shared test state in the plan; Task 5 owns it exclusively.

---

## File Structure

Work is sequential, not wave-partitioned — this is one feature threaded through the existing state machine, and the server-plan's exclusive-ownership model would be ceremony here. Tasks 1 and 2 are independent of each other and of everything else; 3 depends on nothing but is consumed by 5–7; 5 onward are strictly ordered.

| File | Responsibility | Task |
|---|---|---|
| `internal/artifact/artifact.go` | `Problem`/`Assumption`/`OpenQuestion` types, `LoadProblem` validation | 1 |
| `internal/config/config.go` | `StScoping`, defaults, `MaxScopeRounds`, `Policies.Scoping`, validation | 2 |
| `internal/engine/states.go` | `SScoping`, `SAwaitScope`, `isParked`, `resumable` | 3 |
| `internal/store/store.go` | `Counters.ScopeRounds` | 3 |
| `internal/review/review.go` | `GateScope`, `GateFor`, `gateTitle`, `renderActions` | 3 |
| `prompts/scoping.md` | the scoping agent's instructions | 4 |
| `internal/prompt/prompt.go` | `defaults[StScoping]` | 4 |
| `internal/engine/handlers.go` | `handleScoping`, `handleCreated` routing, planning staging | 5, 8 |
| `internal/engine/engine.go` | dispatch case, `handleGate` scope arms | 5, 6 |
| `internal/review/review.go` | `renderProblem`, `Render` case | 7 |
| `prompts/planning.md`, `prompts/design_review.md` | consume the scoped problem | 8 |
| `internal/cli/cli.go`, `sdlc.example.yaml`, `docs/` | operator surfaces | 9 |

---

## Task 1: `problem/1` artifact and validation

**Files:**
- Modify: `internal/artifact/artifact.go` (add types after `Analysis`, loader after `LoadAnalysis`)
- Test: `internal/artifact/artifact_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `artifact.Problem`, `artifact.Assumption`, `artifact.OpenQuestion`, `func LoadProblem(path string) (*Problem, error)`. Task 5 calls `LoadProblem` as the `SCOPING` post-condition and reads `Clarity`; Task 7 reads every field to render the gate document.

- [ ] **Step 1: Write the failing tests**

Add `"encoding/json"` to `internal/artifact/artifact_test.go`'s imports (it currently has `os`, `path/filepath`, `strings`, `testing`), then add:

```go
// validProblem is the baseline every case below mutates. Keeping one literal
// means a new required field breaks every case at once rather than silently
// leaving the older ones testing a shape that no longer validates.
func validProblem() map[string]any {
	return map[string]any{
		"schema":            "problem/1",
		"problem_statement": "SmsExpenseParser drops amounts written with a non-breaking space.",
		"in_scope":          []string{"the SMS amount parser"},
		"out_of_scope":      []string{"the notification parser"},
		"success_criteria":  []string{"an SMS with U+00A0 before the amount parses to the same value as one with a plain space"},
		"assumptions":       []any{},
		"open_questions":    []any{},
		"clarity":           "clear",
	}
}

func writeProblem(t *testing.T, m map[string]any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "problem.json")
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadProblemAcceptsEachClarity(t *testing.T) {
	cases := []struct {
		clarity string
		mutate  func(m map[string]any)
	}{
		{"clear", func(m map[string]any) {}},
		{"assumed", func(m map[string]any) {
			m["assumptions"] = []any{map[string]any{
				"assumption": "only the SMS path is affected",
				"basis":      "NotificationParser has its own amount regex, unchanged since 1.0.2",
			}}
		}},
		{"blocked", func(m map[string]any) {
			m["open_questions"] = []any{map[string]any{
				"id": "Q1", "question": "Should the old format stay readable?",
				"why_it_matters": "decides whether a migration is needed", "blocking": true,
			}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.clarity, func(t *testing.T) {
			m := validProblem()
			m["clarity"] = tc.clarity
			tc.mutate(m)
			got, err := LoadProblem(writeProblem(t, m))
			if err != nil {
				t.Fatalf("valid %s problem rejected: %v", tc.clarity, err)
			}
			if got.Clarity != tc.clarity {
				t.Errorf("clarity=%q, want %q", got.Clarity, tc.clarity)
			}
		})
	}
}

// The clarity field decides the route, so it is checked in both directions.
// Forward: "blocked" with nothing to ask parks the job forever on a question
// the operator cannot answer because it was never written down. Reverse: a
// blocking question with clarity "clear" lets the agent route itself past the
// human, which defeats the state.
func TestLoadProblemRejectsInvalid(t *testing.T) {
	blocking := []any{map[string]any{
		"id": "Q1", "question": "which parser?", "why_it_matters": "different work", "blocking": true,
	}}
	cases := []struct {
		name   string
		mutate func(m map[string]any)
		want   string
	}{
		{"wrong schema", func(m map[string]any) { m["schema"] = "problem/2" }, "problem/1"},
		{"no statement", func(m map[string]any) { m["problem_statement"] = "" }, "problem_statement"},
		{"no success criteria", func(m map[string]any) { m["success_criteria"] = []string{} }, "success_criteria"},
		{"unknown clarity", func(m map[string]any) { m["clarity"] = "murky" }, "clarity"},
		{"blocked with no blocking question", func(m map[string]any) { m["clarity"] = "blocked" }, "blocking"},
		{"blocking question but clarity clear", func(m map[string]any) { m["open_questions"] = blocking }, "blocking"},
		{"assumed with no assumptions", func(m map[string]any) { m["clarity"] = "assumed" }, "assumptions"},
		{"question with no id", func(m map[string]any) {
			m["clarity"] = "blocked"
			m["open_questions"] = []any{map[string]any{"question": "which parser?", "blocking": true}}
		}, "id"},
		{"duplicate question ids", func(m map[string]any) {
			m["clarity"] = "blocked"
			m["open_questions"] = []any{
				map[string]any{"id": "Q1", "question": "a", "blocking": true},
				map[string]any{"id": "Q1", "question": "b", "blocking": false},
			}
		}, "Q1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validProblem()
			tc.mutate(m)
			_, err := LoadProblem(writeProblem(t, m))
			if err == nil {
				t.Fatal("invalid problem accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// A non-blocking question is informational at any clarity: it reaches the gate
// document and the planning prompt, and never routes.
func TestLoadProblemAllowsNonBlockingQuestionWhenClear(t *testing.T) {
	m := validProblem()
	m["open_questions"] = []any{map[string]any{
		"id": "Q1", "question": "is the notification path worth a follow-up?",
		"why_it_matters": "possible second ticket", "blocking": false,
	}}
	if _, err := LoadProblem(writeProblem(t, m)); err != nil {
		t.Fatalf("non-blocking question rejected at clarity clear: %v", err)
	}
}

// Agents on Windows backends write BOMs routinely, and fenced JSON is a
// standing failure mode; every other Load* strips both.
func TestLoadProblemStripsBOMAndFences(t *testing.T) {
	m := validProblem()
	b, _ := json.Marshal(m)
	p := filepath.Join(t.TempDir(), "problem.json")
	if err := os.WriteFile(p, []byte("﻿```json\n"+string(b)+"\n```"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProblem(p); err != nil {
		t.Fatalf("BOM/fence-wrapped problem rejected: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/artifact/ -run TestLoadProblem -v
```

Expected: FAIL — `undefined: LoadProblem`.

- [ ] **Step 3: Add the types**

In `internal/artifact/artifact.go`, after the `Analysis` struct:

```go
// Assumption is one ambiguity the scoping agent resolved on its own. The basis
// is not decoration: an assumption without the evidence behind it is
// indistinguishable from a guess, and the whole value of the field to a
// downstream reader is being able to check the reasoning in one glance.
type Assumption struct {
	Assumption string `json:"assumption"`
	Basis      string `json:"basis"`
}

// OpenQuestion is an ambiguity the scoping agent could not resolve. Blocking
// is reserved for a choice where the answers lead to materially different work
// and nothing in the repository decides between them; anything with a
// defensible default belongs in Assumptions instead.
type OpenQuestion struct {
	ID           string `json:"id"`
	Question     string `json:"question"`
	WhyItMatters string `json:"why_it_matters"`
	Blocking     bool   `json:"blocking"`
}

// Problem is the scoped statement of what the job is for (schema problem/1).
// It is written by SCOPING before any spec exists, and it is what PLANNING is
// bound by and DESIGN_REVIEW checks the spec against.
type Problem struct {
	Schema           string         `json:"schema"`
	ProblemStatement string         `json:"problem_statement"`
	InScope          []string       `json:"in_scope"`
	OutOfScope       []string       `json:"out_of_scope"`
	SuccessCriteria  []string       `json:"success_criteria"`
	Assumptions      []Assumption   `json:"assumptions"`
	OpenQuestions    []OpenQuestion `json:"open_questions"`
	// Clarity routes the job out of SCOPING: clear | assumed | blocked.
	// LoadProblem holds it to the rest of the artifact in both directions, so
	// it is a checked claim rather than a declaration.
	Clarity string `json:"clarity"`
}

// Clarity values.
const (
	ClarityClear   = "clear"
	ClarityAssumed = "assumed"
	ClarityBlocked = "blocked"
)

// BlockingQuestions returns the questions that stop the job. The engine routes
// on it and the gate document prints it, so the definition of "blocking" lives
// in one place.
func (p Problem) BlockingQuestions() []OpenQuestion {
	var out []OpenQuestion
	for _, q := range p.OpenQuestions {
		if q.Blocking {
			out = append(out, q)
		}
	}
	return out
}
```

- [ ] **Step 4: Add the loader**

After `LoadAnalysis` in the same file:

```go
var validClarities = map[string]bool{
	ClarityClear: true, ClarityAssumed: true, ClarityBlocked: true,
}

func LoadProblem(path string) (*Problem, error) {
	var p Problem
	if err := ReadJSON(path, &p); err != nil {
		return nil, err
	}
	var missing []string
	if p.Schema != "problem/1" {
		missing = append(missing, `schema (must be "problem/1")`)
	}
	if strings.TrimSpace(p.ProblemStatement) == "" {
		missing = append(missing, "problem_statement (non-empty)")
	}
	if len(p.SuccessCriteria) == 0 {
		missing = append(missing, "success_criteria (non-empty)")
	}
	if len(missing) > 0 {
		return nil, vErr(path, "missing or invalid required field(s): %s; keys actually present: %s",
			strings.Join(missing, ", "), presentKeys(path))
	}
	if !validClarities[p.Clarity] {
		return nil, vErr(path, "clarity %q invalid (clear|assumed|blocked)", p.Clarity)
	}
	seen := make(map[string]bool, len(p.OpenQuestions))
	for i, q := range p.OpenQuestions {
		if q.ID == "" || strings.TrimSpace(q.Question) == "" {
			return nil, vErr(path, "open_questions[%d]: id and question are required", i)
		}
		if seen[q.ID] {
			return nil, vErr(path, "open_questions[%d]: duplicate id %q", i, q.ID)
		}
		seen[q.ID] = true
	}
	// The clarity field decides whether a human is asked, so it is held to the
	// rest of the artifact in BOTH directions. Without the forward check an
	// agent can claim to be blocked with nothing to ask, and the job parks on a
	// question that was never written down. Without the reverse check an agent
	// can raise a blocking question and still route itself straight past the
	// human, which is the whole thing this state exists to prevent.
	blocking := len(p.BlockingQuestions())
	if p.Clarity == ClarityBlocked && blocking == 0 {
		return nil, vErr(path, `clarity is "blocked" but no open_questions entry has blocking: true; say what you need answered`)
	}
	if p.Clarity != ClarityBlocked && blocking > 0 {
		return nil, vErr(path, `%d open_questions entr(y/ies) are blocking: true, so clarity must be "blocked", not %q`, blocking, p.Clarity)
	}
	if p.Clarity == ClarityAssumed && len(p.Assumptions) == 0 {
		return nil, vErr(path, `clarity is "assumed" but assumptions is empty; name what you assumed and on what basis`)
	}
	return &p, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/artifact/ -run TestLoadProblem -v
```

Expected: PASS, all subtests.

- [ ] **Step 6: Full package suite and vet**

```bash
gofmt -l internal/artifact && go vet ./internal/artifact/ && go test ./internal/artifact/ -race
```

Expected: no gofmt output, no vet output, `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/artifact/artifact.go internal/artifact/artifact_test.go
git commit -m "Add the problem/1 artifact and its validation

SCOPING needs an artifact whose clarity field can be trusted to route the job,
so clarity is checked against the rest of the document in both directions: a
blocked problem must say what it needs answered, and a blocking question
forces clarity to be blocked. Either gap alone would let an agent route itself
past the human it is supposed to ask."
```

---

## Task 2: Config surface

**Files:**
- Modify: `internal/config/config.go` (state constants, `AgentStates`, `Limits`, `Policies`, `Default`, `Validate`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.StScoping = "SCOPING"`; `Limits.MaxScopeRounds int`; `Policies.Scoping string` with `Policies.ScopingEnabled() bool`; `scope` accepted by `Policies.HumanGate`. Task 5 reads `MaxScopeRounds` and `ScopingEnabled()`; Task 3 has no config dependency.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestScopingDefaults(t *testing.T) {
	c := Default()
	st, ok := c.States[StScoping]
	if !ok {
		t.Fatal("Default() has no SCOPING state")
	}
	if st.Agent != "opus" {
		t.Errorf("SCOPING agent=%q, want opus", st.Agent)
	}
	if !c.Policies.ScopingEnabled() {
		t.Error("scoping is off by default; it must default on")
	}
	if c.Limits.MaxScopeRounds != 2 {
		t.Errorf("max_scope_rounds=%d, want 2", c.Limits.MaxScopeRounds)
	}
	// A state absent from AgentStates is never backfilled and never validated.
	found := false
	for _, s := range AgentStates {
		if s == StScoping {
			found = true
		}
	}
	if !found {
		t.Error("SCOPING is not in AgentStates")
	}
}

func TestScopingPolicyValidation(t *testing.T) {
	for _, v := range []string{"on", "off"} {
		c := Default()
		c.Policies.Scoping = v
		if err := c.Validate(); err != nil {
			t.Errorf("policies.scoping %q rejected: %v", v, err)
		}
	}
	c := Default()
	c.Policies.Scoping = "sometimes"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "scoping") {
		t.Errorf("policies.scoping \"sometimes\" accepted or unnamed: %v", err)
	}
	if c.Policies.ScopingEnabled() {
		t.Error("an invalid value must not read as enabled")
	}
}

func TestScopeIsAValidHumanGate(t *testing.T) {
	c := Default()
	c.Policies.HumanGates = []string{"scope"}
	if err := c.Validate(); err != nil {
		t.Fatalf("human_gates: [scope] rejected: %v", err)
	}
	if !c.Policies.HumanGate("scope") {
		t.Error("HumanGate(\"scope\") is false after human_gates: [scope]")
	}
	if c.Policies.HumanGate("spec") {
		t.Error("human_gates: [scope] enabled the spec gate as well")
	}
}

// A config written before this feature has no SCOPING entry and must still
// load, with the default backfilled by applyComputedDefaults.
func TestConfigWithoutScopingStateBackfills(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sdlc.yaml")
	body := "states:\n  PLANNING: { agent: opus, timeout: 30m }\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("pre-feature config rejected: %v", err)
	}
	if c.States[StScoping].Agent == "" {
		t.Error("SCOPING was not backfilled")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/config/ -run "Scoping|ScopeIsAValid|WithoutScoping" -v
```

Expected: FAIL — `undefined: StScoping`.

- [ ] **Step 3: Add the constant, the state, and the fields**

In `internal/config/config.go`, add to the agent-state const block (before `StPlanning`, mirroring pipeline order):

```go
	StScoping      = "SCOPING"
```

Add it to `AgentStates`, first:

```go
var AgentStates = []string{
	StScoping, StPlanning, StDesignReview, StImplementing, StCodeReview,
	StAnalyzing, StFixing, StFinalReview, StVerifying,
}
```

In `Limits`, after `MaxDesignReviewRounds`:

```go
	// MaxScopeRounds caps the human↔agent re-scoping loop. A loop that neither
	// side ends is worse than a stop, so exhausting it escalates.
	MaxScopeRounds int `yaml:"max_scope_rounds"`
```

In `Policies`, after `HumanGates`:

```go
	// Scoping is on|off. Off restores the pre-feature pipeline exactly:
	// CREATED goes straight to PLANNING and no problem.json is produced.
	Scoping string `yaml:"scoping"`
```

Add the accessor next to `HumanGate`:

```go
// ScopingEnabled reports whether the SCOPING state runs. It is a string rather
// than a bool in YAML to match run_in/push/cleanup_worktrees, and it is read
// through this method so an invalid value — which Validate rejects — can never
// read as enabled.
func (p Policies) ScopingEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(p.Scoping), "on")
}
```

- [ ] **Step 4: Add the defaults**

In `Default()`, add to `Limits`:

```go
				MaxScopeRounds:            2,
```

Add to `States`, before `StPlanning`:

```go
			// SCOPING keeps Bash, unlike the reviewers: scoping "Fix 1.0.6
			// issues" means reading what 1.0.6 actually changed, and git log is
			// how that is answered. Read-only exploration is the whole job of
			// this state. It still may not edit the tree — discardTreeChanges
			// enforces it.
			StScoping: {Agent: "opus", Timeout: Duration(15 * time.Minute), DisallowedTools: []string{"Edit", "NotebookEdit"}},
```

Add to `Policies`:

```go
				Scoping:                 "on",
```

- [ ] **Step 5: Add the validation**

In `Validate()`, alongside the other enum checks:

```go
	switch strings.ToLower(strings.TrimSpace(c.Policies.Scoping)) {
	case "on", "off":
	default:
		fail("policies.scoping must be on|off, got %q", c.Policies.Scoping)
	}
	if c.Limits.MaxScopeRounds < 1 {
		fail("limits.max_scope_rounds must be >= 1")
	}
```

And extend the `human_gates` switch to accept `scope`:

```go
		case "scope", "spec", "code", "merge", "release":
```

updating the failure message to `(scope|spec|code|merge|release)`.

- [ ] **Step 6: Run the tests to verify they pass**

```bash
go test ./internal/config/ -race
```

Expected: PASS. `example_config_test.go` also passes — `sdlc.example.yaml` has no `scoping` key yet and therefore takes the default; Task 9 adds the documented key.

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/config && go vet ./internal/config/
git add internal/config/config.go internal/config/config_test.go
git commit -m "Add the SCOPING state to the config schema

SCOPING joins AgentStates so it is backfilled and validated like every other
agent state, which is what lets a config written before this feature load
unchanged. policies.scoping: off restores the previous pipeline exactly, and
max_scope_rounds bounds the re-scoping loop."
```

---

## Task 3: States, counter, and gate registration

**Files:**
- Modify: `internal/engine/states.go`, `internal/store/store.go`, `internal/review/review.go`
- Test: `internal/engine/states_test.go` (create if absent), `internal/review/review_test.go`

**Interfaces:**
- Consumes: `config.StScoping` (Task 2).
- Produces: `engine.SScoping`, `engine.SAwaitScope`; `store.Counters.ScopeRounds int`; `review.GateScope = "scope"` returned by `review.GateFor("AWAITING_SCOPE_APPROVAL")`. Tasks 5 and 6 route on these; the CLI and server pick up the new gate for free through `GateFor`.

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/states_test.go`:

```go
package engine

import (
	"testing"

	"github.com/vipinm/sdlc-orchestrator/internal/review"
)

func TestScopingStateClassification(t *testing.T) {
	if !isAgentState(SScoping) {
		t.Error("SCOPING is not an agent state")
	}
	if !isParked(SAwaitScope) {
		t.Error("AWAITING_SCOPE_APPROVAL is not parked")
	}
	if isTerminal(SScoping) || isHeld(SScoping) {
		t.Error("SCOPING must be neither terminal nor held")
	}
	if producesCode(SScoping) {
		t.Error("SCOPING must not be checkpointed as code-producing")
	}
	if !needsWorktreeReconcile(SScoping) {
		t.Error("a partial SCOPING run must be discarded and re-run")
	}
	if !IsResumableState(SScoping) {
		t.Error("SCOPING must be a valid `sdlc resume --to` target")
	}
	// Pipeline order: resume --to lists states in the order they run.
	if got := ResumableStates(); got[0] != SScoping {
		t.Errorf("resumable[0]=%s, want SCOPING first", got[0])
	}
	if review.GateFor(SAwaitScope) != review.GateScope {
		t.Errorf("GateFor(%s)=%q, want %q", SAwaitScope, review.GateFor(SAwaitScope), review.GateScope)
	}
}
```

Add to `internal/review/review_test.go`:

```go
func TestScopeGateTitleAndActions(t *testing.T) {
	if GateFor("AWAITING_SCOPE_APPROVAL") != GateScope {
		t.Fatal("scope state is not mapped to the scope gate")
	}
	// A gate with no title falls through to the generic "Decision needed.",
	// which tells the operator nothing about what they are deciding.
	got := gateTitle(GateScope, Options{})
	if got == "Decision needed." || got == "" {
		t.Errorf("scope gate has no title of its own: %q", got)
	}
	var bs []Block
	renderActions(&bs, GateScope, Options{Job: &store.Job{ID: "JOB-1"}})
	var actions []Action
	for _, b := range bs {
		if b.Kind == BlockActions {
			actions = b.Actions
		}
	}
	if len(actions) != 2 {
		t.Fatalf("scope gate has %d actions, want 2 (approve, reject)", len(actions))
	}
	for _, a := range actions {
		if len(a.Effect) == 0 {
			t.Errorf("action %q has no effect text", a.Decision)
		}
	}
}
```

Add one row to the existing `TestReviewRendersEveryGate` table in `internal/cli/review_test.go` — every state a human can wait in must render the line that says what to do about it, and the table is where that is enforced:

```go
		{"AWAITING_SCOPE_APPROVAL", review.GateScope, "Approve the scoped problem before a spec is written."},
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/engine/ -run TestScopingStateClassification -v; go test ./internal/review/ -run TestScopeGate -v; go test ./internal/cli/ -run TestReviewRendersEveryGate -v
```

Expected: FAIL — `undefined: SScoping`, `undefined: GateScope`.

- [ ] **Step 3: Add the states**

In `internal/engine/states.go`, in the const block:

```go
	SScoping      = config.StScoping
```

placed immediately before `SPlanning`, and with the parked states:

```go
	// SAwaitScope is where a job waits when the scoping agent raised a blocking
	// question, or when policies.human_gates asks for a scope checkpoint.
	SAwaitScope   = "AWAITING_SCOPE_APPROVAL"
```

Add `SAwaitScope` to `isParked`'s case list, and `SScoping` to `resumable`, first:

```go
var resumable = []string{
	SScoping, SPlanning, SDesignReview, SImplementing, SCodeReview,
	SBuilding, STesting, SFlakeCheck, SAnalyzing, SFixing, SFinalReview,
	SMerging, SReleasing,
}
```

`isAgentState`, `needsWorktreeReconcile` and `producesCode` need no edit: the first two read `config.AgentStates` (Task 2 added `SCOPING`), and the third already excludes everything but `IMPLEMENTING` and `FIXING`.

- [ ] **Step 4: Add the counter**

In `internal/store/store.go`, in `Counters`, after `DesignReviewRounds`:

```go
	// ScopeRounds counts SCOPING attempts a human sent back. Like
	// DesignReviewRounds it increments only on rejection, so it is 0 for a
	// scoping accepted first time. omitempty keeps pre-feature rows readable.
	ScopeRounds int `json:"scope_rounds,omitempty"`
```

- [ ] **Step 5: Register the gate**

In `internal/review/review.go`, add to the gate const block:

```go
	GateScope   = "scope"
```

Add to `GateFor`, before the spec case:

```go
	case "AWAITING_SCOPE_APPROVAL":
		return GateScope
```

Add to `gateTitle`:

```go
	case GateScope:
		return "Approve the scoped problem before a spec is written."
```

Add to `renderActions`, before `GateSpec` (A1: scope vocabulary lives in `Effect`, the decisions stay `approve`/`reject`):

```go
	case GateScope:
		blk.Actions = []Action{
			{Decision: "approve", Effect: text("planning starts from this problem statement; any open questions are waived")},
			{Decision: "reject", Effect: text("scoping runs again with your answers as its instruction")},
		}
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
go test ./internal/engine/ -run TestScopingStateClassification -v && go test ./internal/review/ ./internal/cli/ -race
```

Expected: PASS. The block-model exhaustiveness tests are unaffected — no new `BlockKind` or `SpanKind` was introduced.

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/engine internal/store internal/review internal/cli && go vet ./internal/engine/ ./internal/store/ ./internal/review/ ./internal/cli/
git add internal/engine/states.go internal/engine/states_test.go internal/store/store.go internal/review/review.go internal/review/review_test.go internal/cli/review_test.go
git commit -m "Register the SCOPING state and the scope gate

The state->gate mapping stays in review.GateFor alone, so the CLI's approve
and reject commands and the server's decision handlers pick up the new gate
without a second switch to keep in step."
```

---

## Task 4: The scoping prompt

**Files:**
- Create: `prompts/scoping.md`
- Modify: `internal/prompt/prompt.go` (the `defaults` map)
- Test: `internal/prompt/prompt_test.go` (create if absent)

**Interfaces:**
- Consumes: `config.StScoping` (Task 2).
- Produces: a rendered prompt for `SCOPING` containing the literal phrase `scoping agent` — which A3's fake agent dispatches on — and the `{{.RejectReason}}` branch Task 5 fills.

- [ ] **Step 1: Write the failing test**

Create `internal/prompt/prompt_test.go`:

```go
package prompt

import (
	"strings"
	"testing"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
)

func TestScopingPromptRenders(t *testing.T) {
	got, hash, err := Render(config.StScoping, "", "", Ctx{
		JobID: "JOB-9", Branch: "sdlc/JOB-9",
		IssueTitle: "Fix 1.0.6 issues", IssueBody: "several things are broken",
		Round: 1, MaxRounds: 2,
	})
	if err != nil {
		t.Fatalf("no default prompt for SCOPING: %v", err)
	}
	if hash == "" {
		t.Error("prompt hash is empty")
	}
	// runFakeAgent in internal/engine dispatches on this phrase, and so does a
	// human reading the prompt log. Changing it breaks the engine tests.
	if !strings.Contains(got, "scoping agent") {
		t.Error(`rendered prompt does not contain "scoping agent"`)
	}
	for _, want := range []string{"JOB-9", "Fix 1.0.6 issues", "problem/1", ".sdlc/problem.json", "clarity"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered prompt is missing %q", want)
		}
	}
	// With no rejection there must be no re-scope section.
	if strings.Contains(got, "A human sent this back") {
		t.Error("re-scope section rendered on a first attempt")
	}
}

func TestScopingPromptCarriesTheRejection(t *testing.T) {
	got, _, err := Render(config.StScoping, "", "", Ctx{
		JobID: "JOB-9", Round: 2, MaxRounds: 2,
		RejectReason: "Q1: only the SMS parser. Q2: keep the old format readable.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Q1: only the SMS parser") {
		t.Error("the operator's answers are not in the re-scope prompt")
	}
	if !strings.Contains(got, ".sdlc/context/problem.json") {
		t.Error("the re-scope prompt does not point at the previous problem statement")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/prompt/ -v
```

Expected: FAIL — `no default prompt for state SCOPING`.

- [ ] **Step 3: Register the prompt**

In `internal/prompt/prompt.go`, add to `defaults`, first:

```go
	config.StScoping:      "scoping.md",
```

- [ ] **Step 4: Write `prompts/scoping.md`**

```markdown
You are the scoping agent in an automated SDLC pipeline (job {{.JobID}}, branch {{.Branch}}).
You are working inside a git worktree of the target repository. Attempt {{.Round}} of {{.MaxRounds}}.

Your job is to work out **what is being asked**. It is not to design a solution. A different
agent writes the specification and the plan, and it will be bound by what you write here.

# Issue

Title: {{.IssueTitle}}

{{.IssueBody}}
{{if .RejectReason}}
# A human sent this back

Your previous problem statement is at `.sdlc/context/problem.json`. A human read it and replied:

> {{.RejectReason}}

This is an instruction, not a suggestion, and it takes precedence over your previous reading of
the issue. Where the reply answers one of your open questions, move that question into
`assumptions` with the human's answer as its `basis` and remove it from `open_questions`. Do not
re-ask a question that has been answered, and do not restate your previous statement in new
wording.
{{end}}

# What to do

Read the repository. Find the code the issue is about and describe what it does **now**. A
problem statement that only rephrases the title is worthless: the next agent can read the title
itself. The value you add is the grounding — naming the real function, the real format, the real
current behaviour that the issue is complaining about.

Then decide, honestly, how well you understand the ask:

- **Resolve what you can.** An ambiguity with a defensible default, given the code in front of
  you, is an entry in `assumptions` — not a question. Say what you assumed and what in the
  repository supports it. This is the normal case and it keeps the pipeline moving.
- **Ask only when it matters.** A question is `blocking` only when the possible answers lead to
  materially different work and nothing in the repository decides between them. A blocking
  question stops the job and waits for a human, so the bar is real: if you would be comfortable
  picking one and recording it as an assumption, do that instead.
- **Name what is out of scope.** List the work a reasonable reader would *assume* is included
  and is not. An empty `out_of_scope` on a non-trivial issue is a scoping failure, not a clean
  bill of health.

Success criteria are observable outcomes, not tasks. "The parser handles U+00A0" is a criterion;
"update the parser" is not.

# Rules

- Do NOT write a specification, a plan, an approach, or an implementation.
- Do NOT modify any source file. Your only write is `.sdlc/problem.json`.
- Do NOT run builds or tests; the orchestrator does that.
- You may read files, search, and run read-only git commands to understand the current state.

# Output (mandatory)

Write EXACTLY this file, containing only valid JSON (no markdown fences, no commentary):

`.sdlc/problem.json`:
```json
{
  "schema": "problem/1",
  "problem_statement": "what is wrong or wanted, grounded in the current behaviour you found",
  "in_scope": ["concrete piece of work this job covers"],
  "out_of_scope": ["work a reader might reasonably assume is included, and is not"],
  "success_criteria": ["observable outcome that shows the problem is solved"],
  "assumptions": [{"assumption": "what you took as true", "basis": "what in the repo supports it"}],
  "open_questions": [{"id": "Q1", "question": "...", "why_it_matters": "what changes depending on the answer", "blocking": true}],
  "clarity": "clear | assumed | blocked"
}
```

`clarity` is checked against the rest of the file and the job will fail the state if it disagrees:

- `clear` — no assumptions worth recording and nothing blocking.
- `assumed` — you resolved ambiguity yourself; `assumptions` must be non-empty.
- `blocked` — at least one question has `blocking: true`. Use this only when you truly cannot
  proceed: it stops the job until a human answers.

Set `blocking: true` if and only if `clarity` is `blocked`. Non-blocking questions are welcome at
any clarity — they are passed on as observations and never stop the job.
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/prompt/ -v
```

Expected: PASS, both tests.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/prompt && go vet ./internal/prompt/
git add prompts/scoping.md internal/prompt/prompt.go internal/prompt/prompt_test.go
git commit -m "Add the scoping agent prompt

The prompt's central instruction is the bar for blocking: an ambiguity with a
defensible default is an assumption with its basis recorded, and only a choice
the repository cannot settle is allowed to stop the job and wait for a human."
```

---

## Task 5: `handleScoping`, routing, and dispatch

**Files:**
- Modify: `internal/engine/handlers.go` (`handleCreated`, new `handleScoping`)
- Modify: `internal/engine/engine.go` (dispatch switch)
- Test: `internal/engine/engine_test.go` (fake-agent case per A3, and the state tests)

**Interfaces:**
- Consumes: `artifact.LoadProblem` (Task 1), `config.StScoping`/`MaxScopeRounds`/`ScopingEnabled` (Task 2), `SScoping`/`SAwaitScope`/`Counters.ScopeRounds` (Task 3), the `scoping.md` prompt (Task 4).
- Produces: `func (c *jobCtx) handleScoping(ctx context.Context) (string, error)`; `artifacts/problem.json` on disk for every scoped job. Task 6 consumes the parked state; Task 7 reads the artifact; Task 8 stages it.

- [ ] **Step 1: Teach the fake agent to scope (A3)**

In `internal/engine/engine_test.go`, add a case to `runFakeAgent`'s switch, before the planning case:

```go
	case strings.Contains(prompt, "scoping agent"):
		clarity := os.Getenv("FAKE_SCOPE_CLARITY")
		if clarity == "" {
			clarity = "clear"
		}
		// FAKE_SCOPE_BLOCK_ONCE blocks the first attempt only, so a test can
		// drive block -> reject -> re-scope -> clear. It uses the marker-file
		// trick FAKE_DESIGN_BLOCK_ONCE already uses: the fake is a fresh
		// process each invocation and has nowhere else to remember.
		if os.Getenv("FAKE_SCOPE_BLOCK_ONCE") != "" {
			marker := filepath.Join(os.Getenv("FAKE_MARKER_DIR"), "scoped-once")
			if _, err := os.Stat(marker); err != nil {
				_ = os.WriteFile(marker, []byte("1"), 0o644)
				clarity = "blocked"
			} else {
				clarity = "clear"
			}
		}
		questions, assumptions := "[]", "[]"
		switch clarity {
		case "blocked":
			questions = `[{"id":"Q1","question":"which parser?","why_it_matters":"different work","blocking":true}]`
		case "assumed":
			assumptions = `[{"assumption":"only the SMS path","basis":"NotificationParser has its own regex"}]`
		}
		writeOut("problem.json", fmt.Sprintf(`{"schema":"problem/1",
			"problem_statement":"the parser drops amounts with a non-breaking space",
			"in_scope":["the SMS amount parser"],"out_of_scope":["the notification parser"],
			"success_criteria":["U+00A0 before the amount parses like a plain space"],
			"assumptions":%s,"open_questions":%s,"clarity":%q}`, assumptions, questions, clarity))
```

- [ ] **Step 2: Write the failing tests**

Add to `internal/engine/engine_test.go`:

```go
// The default path: a clear problem statement goes straight to planning, and
// the artifact is harvested where every later state and the gate document
// expect to find it.
func TestScopingClearGoesToPlanning(t *testing.T) {
	e := newEnv(t)
	j := e.submit("non-breaking space in SMS amounts")

	e.runEngine()

	if got := e.jobState(j.ID); got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q", got.State, got.HoldReason)
	}
	if !e.artifactExists(j.ID, "problem.json") {
		t.Error("problem.json was not harvested")
	}
	if n := len(e.promptsFor(j.ID, SScoping)); n != 1 {
		t.Errorf("SCOPING dispatched %d time(s), want 1", n)
	}
}

// A blocking question stops the job at the gate, not in the ESCALATED hold: a
// vague ticket is an expected outcome and belongs in the approve/reject
// surface the operator already uses.
func TestScopingBlockedParksAtTheGate(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_SCOPE_CLARITY", "blocked")
	j := e.submit("fix the thing")

	e.runEngine()

	got := e.jobState(j.ID)
	if got.State != SAwaitScope {
		t.Fatalf("state=%s hold=%q, want %s", got.State, got.HoldReason, SAwaitScope)
	}
	if n := len(e.promptsFor(j.ID, SPlanning)); n != 0 {
		t.Errorf("PLANNING ran %d time(s) before the scope was settled", n)
	}
}

// The policy gate parks a perfectly clear problem statement too, for an
// operator who wants to check every one.
func TestScopingPolicyGateParksAClearProblem(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.HumanGates = []string{"scope"}
	j := e.submit("non-breaking space in SMS amounts")

	e.runEngine()

	if got := e.jobState(j.ID); got.State != SAwaitScope {
		t.Fatalf("state=%s, want %s", got.State, SAwaitScope)
	}
}

// policies.scoping: off must reproduce the pre-feature pipeline exactly.
func TestScopingOffSkipsTheState(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.Scoping = "off"
	j := e.submit("non-breaking space in SMS amounts")

	e.runEngine()

	if got := e.jobState(j.ID); got.State != SAwaitMerge {
		t.Fatalf("state=%s hold=%q", got.State, got.HoldReason)
	}
	if n := len(e.promptsFor(j.ID, SScoping)); n != 0 {
		t.Errorf("SCOPING ran %d time(s) with scoping off", n)
	}
	if e.artifactExists(j.ID, "problem.json") {
		t.Error("problem.json exists with scoping off")
	}
}

// A loop between a human and an agent that neither side ends is worse than a
// stop, so the round cap escalates rather than dispatching the agent again.
func TestScopeRoundCapEscalates(t *testing.T) {
	e := newEnv(t)
	e.cfg.Limits.MaxScopeRounds = 1
	j := e.submit("fix the thing")
	j.Counters.ScopeRounds = 1
	j.State = SScoping
	if err := e.st.UpdateJob(j); err != nil {
		t.Fatal(err)
	}

	e.runEngine()

	got := e.jobState(j.ID)
	if got.State != SEscalated {
		t.Fatalf("state=%s, want %s", got.State, SEscalated)
	}
	if !strings.Contains(got.HoldReason, "scope") {
		t.Errorf("hold reason does not name the scope loop: %q", got.HoldReason)
	}
	if n := len(e.promptsFor(j.ID, SScoping)); n != 0 {
		t.Errorf("the agent ran %d time(s) with the budget already spent", n)
	}
}

// Like the planner and the reviewers, the scoping agent must leave the tree
// clean; a source edit is discarded rather than carried into planning.
func TestScopingDiscardsTreeEdits(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_SCOPE_EDIT", "1")
	j := e.submit("non-breaking space in SMS amounts")

	e.runEngine()

	wt := filepath.Join(e.repo, ".worktrees", j.ID)
	if out := git(t, wt, "status", "--porcelain"); strings.Contains(out, "src/app.txt") {
		t.Errorf("scoping left an edit in the tree: %q", out)
	}
}
```

For the last test, add to the fake's scoping case:

```go
		if os.Getenv("FAKE_SCOPE_EDIT") != "" {
			_ = os.WriteFile("src/app.txt", []byte("scoped\n"), 0o644)
		}
```

- [ ] **Step 3: Run the tests to verify they fail**

```bash
go test ./internal/engine/ -run "TestScoping|TestScopeRound" -v
```

Expected: FAIL — `undefined: (*jobCtx).handleScoping` once the dispatch case is referenced; before that, the jobs run `CREATED → PLANNING` and the state assertions fail.

- [ ] **Step 4: Route out of `CREATED`**

In `internal/engine/handlers.go`, change the last line of `handleCreated`:

```go
	if !c.e.cfg.Policies.ScopingEnabled() {
		return SPlanning, nil
	}
	return SScoping, nil
```

- [ ] **Step 5: Write `handleScoping`**

Insert into `internal/engine/handlers.go` between `handleCreated` and `handlePlanning`:

```go
// --- SCOPING -------------------------------------------------------------

func (c *jobCtx) handleScoping(ctx context.Context) (string, error) {
	// Check the budget before dispatching, not after: an agent invocation
	// spent on a round that can never be accepted is pure cost.
	if c.job.Counters.ScopeRounds >= c.e.cfg.Limits.MaxScopeRounds {
		return "", escalate("scope still unresolved after %d round(s); see problem.json and the gate history",
			c.job.Counters.ScopeRounds)
	}
	if err := c.resetExchange(); err != nil {
		return "", err
	}
	pctx := c.baseCtx()
	pctx.Round = c.job.Counters.ScopeRounds + 1
	pctx.MaxRounds = c.e.cfg.Limits.MaxScopeRounds
	rejected := c.job.Counters.HumanRejectReason
	// Stage the previous statement whenever there is one to revise. Keying this
	// on the counter alone would send the agent back in with nothing to answer
	// and it would write a new statement from scratch — the same trap
	// handlePlanning documents.
	if c.job.Counters.ScopeRounds > 0 || rejected != "" {
		c.stageArtifact("problem.json")
	}
	pctx.RejectReason = rejected
	problemPath := filepath.Join(c.sdlcDir(), "problem.json")
	err := c.runAgent(ctx, SScoping, pctx, func() error {
		if _, err := artifact.LoadProblem(problemPath); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if _, err := c.harvest("problem.json", "problem.json"); err != nil {
		return "", err
	}
	prob, err := artifact.LoadProblem(filepath.Join(c.artDir(), "problem.json"))
	if err != nil {
		return "", err
	}
	// The objection applied to this one attempt.
	c.job.Counters.HumanRejectReason = ""
	c.discardTreeChanges(ctx) // scoping must not leave code edits behind

	if blocking := prob.BlockingQuestions(); len(blocking) > 0 {
		ids := make([]string, 0, len(blocking))
		for _, q := range blocking {
			ids = append(ids, q.ID)
		}
		c.e.event(c.job, "note", map[string]any{
			"awaiting_scope_approval": true, "reason": "agent_blocked",
			"blocking_questions": ids, "problem_statement": prob.ProblemStatement,
		})
		return SAwaitScope, nil
	}
	if c.e.cfg.Policies.HumanGate("scope") {
		c.e.event(c.job, "note", map[string]any{
			"awaiting_scope_approval": true, "reason": "policy_gate",
			"clarity": prob.Clarity, "assumptions": len(prob.Assumptions),
			"problem_statement": prob.ProblemStatement,
		})
		return SAwaitScope, nil
	}
	return SPlanning, nil
}
```

- [ ] **Step 6: Add the dispatch case**

In `internal/engine/engine.go`, in `step`'s switch, before `case SPlanning`:

```go
	case SScoping:
		next, herr = jc.handleScoping(ctx)
```

- [ ] **Step 7: Run the tests to verify they pass**

```bash
go test ./internal/engine/ -run "TestScoping|TestScopeRound" -v
```

Expected: PASS. `TestScopingBlockedParksAtTheGate` and `TestScopingPolicyGateParksAClearProblem` leave the job parked because Task 6 has not taught `handleGate` to consume the row yet — that is the correct behaviour for this task and both tests assert only the parked state.

- [ ] **Step 8: Run the whole engine suite**

```bash
go test ./internal/engine/ -race
```

Expected: PASS. Every pre-existing end-to-end test now runs through `SCOPING` first; if one asserts an exact dispatch count for a state other than `SCOPING` it is unaffected, and `TestHappyPathToCompletion` still reaches `COMPLETED`.

- [ ] **Step 9: Commit**

```bash
gofmt -l internal/engine && go vet ./internal/engine/
git add internal/engine/handlers.go internal/engine/engine.go internal/engine/engine_test.go
git commit -m "Run SCOPING between CREATED and PLANNING

The round budget is checked before the agent is dispatched rather than after,
because an invocation spent on a round that can never be accepted is pure
cost. A blocking question parks the job at a gate rather than escalating: a
vague ticket is an expected outcome, not a failure."
```

---

## Task 6: The scope gate decision

**Files:**
- Modify: `internal/engine/engine.go` (`handleGate`)
- Test: `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: `review.GateScope` (Task 3), `SAwaitScope` and the parked jobs Task 5 produces.
- Produces: the `AWAITING_SCOPE_APPROVAL → PLANNING | SCOPING | CANCELLED` transitions. Nothing later depends on it.

- [ ] **Step 1: Write the failing tests**

Add to `internal/engine/engine_test.go`:

```go
// Approving with questions outstanding is a waiver, and a waiver that leaves
// no trace is the silent decision this pipeline exists to prevent.
func TestScopeApprovalWaivesOpenQuestions(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_SCOPE_CLARITY", "blocked")
	j := e.submit("fix the thing")
	e.runEngine()
	if got := e.jobState(j.ID); got.State != SAwaitScope {
		t.Fatalf("state=%s, want %s", got.State, SAwaitScope)
	}

	if err := e.st.AddApproval(&store.Approval{JobID: j.ID, Gate: "scope", Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	// The agent would block again on a second scoping run; approval must not
	// send it back there.
	t.Setenv("FAKE_SCOPE_CLARITY", "clear")
	e.runEngine()

	got := e.jobState(j.ID)
	if got.State == SAwaitScope || got.State == SScoping {
		t.Fatalf("approval did not move the job past scoping: state=%s", got.State)
	}
	if n := len(e.promptsFor(j.ID, SScoping)); n != 1 {
		t.Errorf("SCOPING ran %d time(s); approval must not re-scope", n)
	}
	if d := e.eventDetails(j.ID); !strings.Contains(d, "scope_questions_waived") || !strings.Contains(d, "Q1") {
		t.Error("the waived questions were not recorded in the event log")
	}
}

// Rejection carries the operator's answers back into a fresh scoping round.
func TestScopeRejectionRescopesWithTheAnswers(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_MARKER_DIR", t.TempDir())
	t.Setenv("FAKE_SCOPE_BLOCK_ONCE", "1")
	j := e.submit("fix the thing")
	e.runEngine()
	if got := e.jobState(j.ID); got.State != SAwaitScope {
		t.Fatalf("state=%s, want %s", got.State, SAwaitScope)
	}

	const answer = "Q1: only the SMS parser."
	if err := e.st.AddApproval(&store.Approval{
		JobID: j.ID, Gate: "scope", Decision: "reject", Reason: answer,
	}); err != nil {
		t.Fatal(err)
	}
	e.runEngine()

	got := e.jobState(j.ID)
	if got.Counters.ScopeRounds != 1 {
		t.Errorf("scope_rounds=%d, want 1", got.Counters.ScopeRounds)
	}
	if got.Counters.HumanRejectReason != "" {
		t.Error("the rejection reason was not cleared after the round consumed it")
	}
	prompts := e.promptsFor(j.ID, SScoping)
	if len(prompts) != 2 {
		t.Fatalf("SCOPING ran %d time(s), want 2", len(prompts))
	}
	if !strings.Contains(prompts[1], answer) {
		t.Errorf("the second scoping prompt does not carry the answers:\n%s", prompts[1])
	}
	if got.State == SAwaitScope || got.State == SScoping {
		t.Errorf("the re-scoped job did not move on: state=%s", got.State)
	}
}

// --cancel at the scope gate ends the job, as it does at the spec gate.
func TestScopeRejectionWithCancelEndsTheJob(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_SCOPE_CLARITY", "blocked")
	j := e.submit("fix the thing")
	e.runEngine()

	if err := e.st.AddApproval(&store.Approval{
		JobID: j.ID, Gate: "scope", Decision: "reject", Reason: "not worth doing", Cancel: true,
	}); err != nil {
		t.Fatal(err)
	}
	e.runEngine()

	if got := e.jobState(j.ID); got.State != SCancelled {
		t.Fatalf("state=%s, want %s", got.State, SCancelled)
	}
}
```

Add the CLI regression that A2 says needs no new code, to `internal/cli/cli_test.go`. It uses the existing `newReviewEnv`/`e.job`/`captureStderr` harness from `review_test.go` (same package) — do not add a second one. Add `"github.com/vipinm/sdlc-orchestrator/internal/review"` to that file's imports (it currently has only stdlib):

```go
// A2: reject already requires a reason on every gate, and the new one inherits
// that for free through review.GateFor. The scope gate's reason IS the
// operator's answers, so an empty one would hand the next scoping round
// nothing to act on. Pinned here because the rule is inherited rather than
// written, and inherited behaviour is what silently stops applying.
func TestRejectAtScopeGateRequiresAReason(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("AWAITING_SCOPE_APPROVAL", "fix the thing")

	var code int
	stderr := captureStderr(t, func() {
		code = cmdDecision(e.cfg, []string{j.ID}, "reject")
	})
	if code == 0 {
		t.Error("reject with no --reason was accepted at the scope gate")
	}
	if !strings.Contains(stderr, "--reason") {
		t.Errorf("stderr does not say what is missing:\n%s", stderr)
	}

	captureStderr(t, func() {
		code = cmdDecision(e.cfg, []string{j.ID, "--reason", "Q1: only the SMS parser"}, "reject")
	})
	if code != 0 {
		t.Fatalf("reject with a reason exited %d at the scope gate, want 0", code)
	}
	a, err := e.st.PendingApproval(j.ID, review.GateScope)
	if err != nil || a == nil {
		t.Fatalf("no pending row at the scope gate: %v %v", a, err)
	}
	if a.Reason != "Q1: only the SMS parser" {
		t.Errorf("reason=%q, want the operator's answers verbatim", a.Reason)
	}
}
```

Also add an approve case, since `cmdDecision` resolves the gate through `review.GateFor` and a missing mapping would fail silently by writing the row under an empty gate name:

```go
func TestApproveAtScopeGateWritesTheScopeRow(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("AWAITING_SCOPE_APPROVAL", "fix the thing")

	var code int
	captureStderr(t, func() { code = cmdDecision(e.cfg, []string{j.ID}, "approve") })
	if code != 0 {
		t.Fatalf("approve exited %d at the scope gate, want 0", code)
	}
	if a, err := e.st.PendingApproval(j.ID, review.GateScope); err != nil || a == nil {
		t.Fatalf("approve did not write a row under the scope gate: %v %v", a, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/engine/ -run TestScope -v
```

Expected: FAIL — the jobs stay in `AWAITING_SCOPE_APPROVAL`; `handleGate` has no arm for the gate and silently consumes nothing.

- [ ] **Step 3: Add the gate arms**

In `internal/engine/engine.go`, in `handleGate`'s switch, before the `GateSpec` cases:

```go
	case gate == review.GateScope && a.Decision == "approve":
		// Approval with questions outstanding is a waiver: planning proceeds on
		// the recorded assumptions. Naming the waived questions is the point —
		// a waiver nobody can find later is indistinguishable from a question
		// that was never asked.
		if prob, err := artifact.LoadProblem(
			filepath.Join(artifact.ArtifactsDir(e.cfg.Orchestrator.DataDir, j.ID), "problem.json"),
		); err == nil {
			if blocking := prob.BlockingQuestions(); len(blocking) > 0 {
				ids := make([]string, 0, len(blocking))
				for _, q := range blocking {
					ids = append(ids, q.ID)
				}
				e.event(j, "note", map[string]any{"scope_questions_waived": ids})
			}
		}
		e.transition(j, SPlanning, "scope approved")
	case gate == review.GateScope && a.Decision == "reject":
		if a.Cancel {
			e.transition(j, SCancelled, "scope rejected (cancelled): "+a.Reason)
			e.cleanup(ctx, j)
		} else {
			// Unlike the spec gate, this counter IS spent on a human decision:
			// re-scoping is a conversation between the operator and the agent,
			// and max_scope_rounds is what stops it running forever.
			j.Counters.ScopeRounds++
			j.Counters.HumanRejectReason = a.Reason
			e.transition(j, SScoping, "scope rejected: "+a.Reason)
		}
```

Add the `artifact` and `path/filepath` imports to `engine.go` if they are not already present.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/engine/ -run TestScope -v && go test ./internal/cli/ -run "AtScopeGate" -v
```

Expected: PASS.

- [ ] **Step 5: Full suite**

```bash
go test ./... -race
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/engine/engine.go internal/engine/engine_test.go internal/cli/cli_test.go
git commit -m "Decide the scope gate

Approving with questions outstanding is a waiver, so the waived question ids
go into the event log: a waiver nobody can find later is indistinguishable
from a question that was never asked. Rejection spends a scope round, unlike
the spec gate, because re-scoping is a conversation and max_scope_rounds is
what stops it running forever."
```

---

## Task 7: The scope gate document

**Files:**
- Modify: `internal/review/review.go` (`Render` case, `renderProblem`)
- Test: `internal/review/review_test.go`, `internal/review/testdata/`

**Interfaces:**
- Consumes: `artifact.Problem` (Task 1), `GateScope` (Task 3).
- Produces: the rendered gate document for `scope`. The CLI's `sdlc review` and the server's gate page render it through the existing shared path, so no caller changes.

- [ ] **Step 1: Write the failing test**

Add `"time"` and `"github.com/vipinm/sdlc-orchestrator/internal/artifact"` to `internal/review/review_test.go`'s imports, then add:

```go
func TestScopeGateDocument(t *testing.T) {
	dir := t.TempDir()
	j := &store.Job{
		ID: "JOB-3", IssueTitle: "fix the thing", IssueBody: "several things are broken",
		State: "AWAITING_SCOPE_APPROVAL", Branch: "sdlc/JOB-3", WorktreePath: filepath.Join(dir, "wt"),
		StateEnteredAt: time.Now().Add(-4 * time.Minute),
	}
	art := artifact.ArtifactsDir(dir, j.ID)
	if err := os.MkdirAll(art, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":"problem/1",
		"problem_statement":"SmsExpenseParser drops amounts written with a non-breaking space",
		"in_scope":["the SMS amount parser"],"out_of_scope":["the notification parser"],
		"success_criteria":["U+00A0 before the amount parses like a plain space"],
		"assumptions":[{"assumption":"only the SMS path is affected","basis":"NotificationParser has its own regex"}],
		"open_questions":[{"id":"Q1","question":"should the old format stay readable?","why_it_matters":"decides whether a migration is needed","blocking":true}],
		"clarity":"blocked"}`
	if err := os.WriteFile(filepath.Join(art, "problem.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := Render(context.Background(), Options{Job: j, DataDir: dir})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if d.Gate != GateScope {
		t.Errorf("gate=%q, want %q", d.Gate, GateScope)
	}
	for _, want := range []string{
		"SmsExpenseParser drops amounts",       // the statement
		"the notification parser",              // out of scope
		"U+00A0 before the amount",             // success criteria
		"NotificationParser has its own regex", // the assumption's basis
		"should the old format stay readable?", // the blocking question
		"decides whether a migration is needed",
	} {
		if !strings.Contains(d.Body, want) {
			t.Errorf("document is missing %q\n---\n%s", want, d.Body)
		}
	}
	// The operator must be told why the job is parked; the two causes need
	// different responses.
	if !strings.Contains(d.Body, "blocked") {
		t.Error("the document does not say the agent is blocked")
	}
}

// The policy-gate case has no blocking question and must not imply one.
func TestScopeGateDocumentPolicyGateWording(t *testing.T) {
	dir := t.TempDir()
	j := &store.Job{ID: "JOB-4", IssueTitle: "clear ticket", State: "AWAITING_SCOPE_APPROVAL",
		StateEnteredAt: time.Now()}
	art := artifact.ArtifactsDir(dir, j.ID)
	if err := os.MkdirAll(art, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":"problem/1","problem_statement":"a clear problem",
		"in_scope":["x"],"out_of_scope":["y"],"success_criteria":["z"],
		"assumptions":[],"open_questions":[],"clarity":"clear"}`
	if err := os.WriteFile(filepath.Join(art, "problem.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := Render(context.Background(), Options{Job: j, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Body, "Open questions") {
		t.Error("an open-questions section was rendered with no questions")
	}
	if !strings.Contains(d.Body, "human_gates") {
		t.Error("the document does not say why a clear problem is parked")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/review/ -run TestScopeGateDocument -v
```

Expected: FAIL — `Render` has no `GateScope` case, so the document contains only the header and actions.

- [ ] **Step 3: Add the render case**

In `internal/review/review.go`, in `Render`'s switch, before `case GateSpec`:

```go
	case GateScope:
		renderIssue(&d.Blocks, o.Job)
		renderProblem(&d.Blocks, art)
```

- [ ] **Step 4: Write `renderProblem`**

Add next to `renderSpec`. It uses only existing `BlockKind`s, so the exhaustiveness tests need no new cases and the terminal and browser cannot come to describe the gate differently:

```go
// renderProblem prints the scoped problem and says why the job is parked. The
// two causes — the agent raised a blocking question, or human_gates asks for a
// checkpoint — need different responses from the operator, so the document
// distinguishes them rather than leaving them to be inferred from the presence
// of a questions section.
func renderProblem(bs *[]Block, art string) {
	p, err := artifact.LoadProblem(filepath.Join(art, "problem.json"))
	if err != nil {
		*bs = append(*bs, section("Scoped problem"),
			aside("problem.json is missing or unreadable: "+err.Error()))
		return
	}
	*bs = append(*bs, section("Scoped problem"), para(txt(p.ProblemStatement)))
	bullets(bs, "In scope", p.InScope)
	bullets(bs, "Out of scope", p.OutOfScope)
	bullets(bs, "Success criteria", p.SuccessCriteria)

	if len(p.Assumptions) > 0 {
		items := make([]Item, 0, len(p.Assumptions))
		for _, a := range p.Assumptions {
			items = append(items, Item{
				Spans: []Span{txt(a.Assumption)},
				Sub:   [][]Span{{Span{Kind: SpanEmphasis, Text: "basis: " + a.Basis}}},
			})
		}
		*bs = append(*bs, section("Assumptions"), Block{Kind: BlockBullets, Items: items})
	}

	blocking := p.BlockingQuestions()
	if len(p.OpenQuestions) > 0 {
		// Blocking questions first: they are the ones an answer is needed for.
		ordered := append(append([]artifact.OpenQuestion{}, blocking...), nonBlocking(p)...)
		items := make([]Item, 0, len(ordered))
		for _, q := range ordered {
			label := q.ID + ": " + q.Question
			if !q.Blocking {
				label += " (not blocking)"
			}
			it := Item{Spans: []Span{txt(label)}}
			if q.WhyItMatters != "" {
				it.Sub = [][]Span{{Span{Kind: SpanEmphasis, Text: "why it matters: " + q.WhyItMatters}}}
			}
			items = append(items, it)
		}
		*bs = append(*bs, section("Open questions"), Block{Kind: BlockBullets, Items: items})
	}

	if len(blocking) > 0 {
		// aside, not asideOf: this remark quotes no command or path, and
		// asideOf exists for the ones that do.
		*bs = append(*bs, aside(fmt.Sprintf(
			"The scoping agent is blocked on %d question(s) and will not write a spec until they are settled. "+
				"Rejecting with your answers re-scopes; approving waives them and plans on the assumptions above.",
			len(blocking))))
		return
	}
	*bs = append(*bs, asideOf(
		txt("The scoping agent reported clarity "), code(p.Clarity),
		txt(" and raised nothing blocking. This job is parked because "), code("policies.human_gates"),
		txt(" lists "), code("scope"), txt("."),
	))
}

// nonBlocking is the complement of Problem.BlockingQuestions.
func nonBlocking(p *artifact.Problem) []artifact.OpenQuestion {
	var out []artifact.OpenQuestion
	for _, q := range p.OpenQuestions {
		if !q.Blocking {
			out = append(out, q)
		}
	}
	return out
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/review/ -race
```

Expected: PASS, including the block/span exhaustiveness tests and the existing goldens (no existing document changed).

- [ ] **Step 6: Check the browser renderer agrees**

```bash
go test ./internal/server/ -race
```

Expected: PASS. `renderProblem` introduces no new `BlockKind`, so the HTML template already handles every block it emits.

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/review && go vet ./internal/review/
git add internal/review/review.go internal/review/review_test.go
git commit -m "Render the scope gate document

The document distinguishes the two reasons a job parks here — a blocking
question, or a configured checkpoint — because they need different responses
and inferring the cause from the presence of a questions section is exactly
the kind of guess the block model exists to remove. No new block kind, so the
terminal and the browser stay in step for free."
```

---

## Task 8: Planning and design review consume the scoped problem

**Files:**
- Modify: `internal/engine/handlers.go` (`handlePlanning`, `handleDesignReview`)
- Modify: `prompts/planning.md`, `prompts/design_review.md`
- Test: `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: `artifacts/problem.json` (Task 5).
- Produces: `.sdlc/context/problem.json` staged for `PLANNING` and `DESIGN_REVIEW`. Nothing later depends on it.

- [ ] **Step 1: Write the failing tests**

Add to `internal/engine/engine_test.go`:

```go
func TestPlanningIsBoundByTheScopedProblem(t *testing.T) {
	e := newEnv(t)
	j := e.submit("non-breaking space in SMS amounts")

	e.runEngine()

	prompts := e.promptsFor(j.ID, SPlanning)
	if len(prompts) == 0 {
		t.Fatal("PLANNING never ran")
	}
	for _, want := range []string{"the parser drops amounts", "the notification parser"} {
		if !strings.Contains(prompts[0], want) {
			t.Errorf("planning prompt is missing %q from the scoped problem:\n%s", want, prompts[0])
		}
	}
	dr := e.promptsFor(j.ID, SDesignReview)
	if len(dr) == 0 || !strings.Contains(dr[0], "problem.json") {
		t.Error("the design reviewer was not pointed at the scoped problem")
	}
}

// With scoping off there is no problem.json, and both prompts must render as
// they did before the feature.
func TestPlanningWithoutAScopedProblem(t *testing.T) {
	e := newEnv(t)
	e.cfg.Policies.Scoping = "off"
	j := e.submit("non-breaking space in SMS amounts")

	e.runEngine()

	prompts := e.promptsFor(j.ID, SPlanning)
	if len(prompts) == 0 {
		t.Fatal("PLANNING never ran")
	}
	if strings.Contains(prompts[0], "Scoped problem") {
		t.Error("a Scoped problem section rendered with no problem.json")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/engine/ -run "TestPlanningIsBound|TestPlanningWithout" -v
```

Expected: FAIL — the planning prompt has no scoped-problem content.

- [ ] **Step 3: Stage the artifact**

`stageArtifact` is already a no-op for a missing file (it is called unconditionally for `spec.json` on the first planning round today), so both handlers can call it unconditionally.

In `handlePlanning`, after `pctx := c.baseCtx()`:

```go
	// The scoped problem is what this spec must solve, and what DESIGN_REVIEW
	// will check it against. Staged unconditionally: with policies.scoping off
	// there is no file and the prompt's section does not render.
	c.stageArtifact("problem.json")
```

In `handleDesignReview`, alongside the existing `stageArtifact` calls:

```go
	c.stageArtifact("problem.json")
```

- [ ] **Step 4: Add the prompt context field**

The prompt needs the problem's content, not just the staged path, so the planner cannot skip reading it. Add to `prompt.Ctx` in `internal/prompt/prompt.go`:

```go
	// ScopedProblem is problem.json's content, inlined so the planner is bound
	// by it without having to be trusted to open the staged file. "" when
	// policies.scoping is off.
	ScopedProblem string
```

Add a helper to `internal/engine/jobctx.go`, next to `findingsJSON`:

```go
// problemJSON returns the harvested problem.json, or "" when there is none.
// Like findingsJSON it returns a string rather than a struct: the prompt
// template inlines it verbatim, and a read error is the same as absence here —
// the section simply does not render.
func (c *jobCtx) problemJSON() string {
	b, err := os.ReadFile(filepath.Join(c.artDir(), "problem.json"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
```

Set it in both handlers, after `pctx := c.baseCtx()`:

```go
	pctx.ScopedProblem = c.problemJSON()
```

- [ ] **Step 5: Amend `prompts/planning.md`**

Insert after the `# Issue` section and before the `{{if .PrevFindings}}` block:

```markdown
{{if .ScopedProblem}}
# Scoped problem

A scoping agent already read this issue and the repository and settled what is being asked.
This is the problem your spec must solve, and the design reviewer will check your spec against it.

```json
{{.ScopedProblem}}
```

Two rules bind you to it:

- Your spec's `out_of_scope` must include every entry of the problem's `out_of_scope`.
- Do not widen `in_scope`. If work outside it is genuinely necessary, say so explicitly in
  `approach` and name it — do not fold it in silently.

Where the problem records an assumption, treat it as settled. Where it records a non-blocking
open question, treat it as a known unknown: do not design around an answer nobody gave.
{{end}}
```

- [ ] **Step 6: Amend `prompts/design_review.md`**

Add to the `# Inputs` list:

```markdown
- Scoped problem: `.sdlc/context/problem.json` (absent when scoping is disabled)
```

Add checklist item 8:

```markdown
8. If a scoped problem is present: does the spec actually solve *that* problem? Raise a finding
   when the spec silently widens scope beyond `in_scope`, drops a `success_criteria` entry, or
   solves something adjacent to what was scoped. Severity as calibrated below — a spec that
   solves the wrong problem is a `blocker`.
```

- [ ] **Step 7: Run the tests to verify they pass**

```bash
go test ./internal/engine/ -run "TestPlanningIsBound|TestPlanningWithout" -v && go test ./internal/prompt/ ./internal/engine/ -race
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/engine/handlers.go internal/engine/jobctx.go internal/engine/engine_test.go internal/prompt/prompt.go prompts/planning.md prompts/design_review.md
git commit -m "Bind planning to the scoped problem and let design review check it

The problem is inlined into the planning prompt rather than only staged as a
file, so the planner is bound by it without having to be trusted to open it.
Design review gets the same document and one checklist item, which is the
automated check that scope discipline survives an unattended run."
```

---

## Task 9: Operator surfaces and documentation

**Files:**
- Modify: `internal/cli/cli.go` (the `ACTION NEEDED` switch)
- Modify: `sdlc.example.yaml`, `docs/SPEC.md`, `docs/running.md`
- Test: `internal/cli/cli_test.go`, `internal/config/example_config_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: nothing consumed by code.

- [ ] **Step 1: Write the failing test**

`cmdStatus` prints through `fmt.Printf` to `os.Stdout` and the package has no stdout capture, only `captureStderr`. Add its twin to `internal/cli/review_test.go`, immediately below `captureStderr`, so the two read as a pair:

```go
// captureStdout is captureStderr's twin. cmdStatus writes through fmt.Printf
// rather than an injectable writer, so this is the only way to assert what an
// operator actually sees.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() {
		os.Stdout = old
	}()
	fn()
	w.Close()
	return <-done
}
```

Then add to `internal/cli/cli_test.go`:

```go
// Every parked state names its action. A gate the operator cannot find is a
// job that waits forever — the failure `sdlc status` printing context for
// exactly one of three waiting states already caused once.
func TestStatusNamesTheScopeAction(t *testing.T) {
	e := newReviewEnv(t)
	j := e.job("AWAITING_SCOPE_APPROVAL", "fix the thing")

	var code int
	out := captureStdout(t, func() { code = cmdStatus(e.cfg, []string{j.ID}) })
	if code != 0 {
		t.Fatalf("status exited %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "ACTION NEEDED") {
		t.Errorf("status does not flag the scope gate:\n%s", out)
	}
	if !strings.Contains(out, "scoped problem") {
		t.Errorf("status does not say what is being decided:\n%s", out)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/cli/ -run TestStatusNamesTheScopeAction -v
```

Expected: FAIL — the status output has no `ACTION NEEDED` line for the new state.

- [ ] **Step 3: Add the status line**

In `internal/cli/cli.go`, in the `switch j.State` of the status printer, before `case engine.SAwaitSpec`:

```go
	case engine.SAwaitScope:
		fmt.Println("\n  ACTION NEEDED: approve the scoped problem before a spec is written")
```

- [ ] **Step 4: Document the config**

In `sdlc.example.yaml`, add to `limits:` after `max_design_review_rounds`:

```yaml
  max_scope_rounds: 2           # human<->agent re-scoping loop cap
```

Add to `states:`, before `PLANNING`:

```yaml
  # SCOPING turns the raw issue into a scoped problem statement before any spec
  # exists. It keeps Bash, unlike the reviewers: scoping "fix the 1.0.6 issues"
  # means reading what 1.0.6 actually changed, and git log is how that is
  # answered. It still may not edit the tree.
  SCOPING:       { agent: opus,   timeout: 15m, disallowed_tools: [Edit, NotebookEdit] }
```

Add to `policies:`:

```yaml
  # on | off. SCOPING runs before PLANNING and produces problem.json: the
  # problem statement, scope boundaries, success criteria, the assumptions the
  # agent made, and anything it could not resolve. A question it cannot resolve
  # parks the job at the `scope` gate with the question attached, rather than
  # letting a guess reach the spec. off restores the previous pipeline exactly.
  scoping: on
```

and extend the `human_gates` comment:

```yaml
  # Optional earlier stops: `scope` after scoping and before any spec is
  # written, `spec` after design review passes and before any code is written,
  # `code` after code review passes and before build and test. Merge and
  # release are always enforced; this key only adds stops. The scope gate is
  # enforced regardless when the scoping agent reports a blocking question.
  # human_gates: [scope, spec, code]
```

- [ ] **Step 5: Update the specs and the runbook**

In `docs/SPEC.md`:
- §3.1: add `SCOPING` and `AWAITING_SCOPE_APPROVAL` to the state list.
- §3.2: add the transitions from [`docs/scoping-spec.md`](scoping-spec.md) §3.2.
- §3.3: add `scope_rounds`.
- §5.1: add the `problem/1` schema.
- §6.3: add `scoping.md` to the prompt table.
- §12: add `policies.scoping`, `limits.max_scope_rounds`, the `SCOPING` state entry, and `scope` in `human_gates`.
- §14: add `prompts/scoping.md`.

In `docs/running.md`, add a section covering: what the scope gate is, the two reasons a job parks there, that approving with questions outstanding is a waiver, and the two commands:

```bash
sdlc approve JOB-7
sdlc reject JOB-7 --reason "Q1: only the SMS parser. Q2: keep the old format readable."
```

- [ ] **Step 6: Run the full suite**

```bash
gofmt -l . && go vet ./... && go test ./... -race
```

Expected: no output from the first two, all packages `ok`. `example_config_test.go` validates `sdlc.example.yaml` against the strict decoder, so a typo in step 4 fails here.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/cli.go internal/cli/cli_test.go internal/cli/review_test.go sdlc.example.yaml docs/SPEC.md docs/running.md
git commit -m "Document the scope gate and its configuration

A gate the operator cannot find is a job that waits forever, so the new parked
state names its action in sdlc status alongside every other gate."
```

---

## Acceptance criteria

- **AC-1** — A job whose scoping reports `clarity: clear` runs `CREATED → SCOPING → PLANNING` with exactly one scoping invocation, and `artifacts/problem.json` exists.
- **AC-2** — A job whose scoping reports a blocking question parks in `AWAITING_SCOPE_APPROVAL` and `PLANNING` has not run.
- **AC-3** — `clarity: blocked` with no blocking question, and a blocking question with any other clarity, both fail the state with an error naming the contradiction.
- **AC-4** — Rejecting at the scope gate with `--reason` re-enters `SCOPING`, increments `scope_rounds`, and the rendered second prompt contains the operator's text.
- **AC-5** — Approving at the scope gate with questions outstanding transitions to `PLANNING` and records the waived question ids in the event log.
- **AC-6** — `scope_rounds` at `max_scope_rounds` escalates without dispatching the agent.
- **AC-7** — `policies.scoping: off` produces `CREATED → PLANNING`, zero scoping invocations, and no `problem.json`.
- **AC-8** — A config file with no `SCOPING` state and no `scoping` key loads and validates.
- **AC-9** — `git diff --stat` shows no change to `spec/1`, `plan/1`, `review/1`, `implementation/1`, `analysis/1` or `verdict/1`, and no SQL schema change.
- **AC-10** — The scope gate document names the problem statement, both scope lists, the success criteria, every assumption with its basis, every open question with why it matters, and why the job is parked.
- **AC-11** — `grep -c "AWAITING_SCOPE_APPROVAL" internal/` returns hits only in `states.go` and `review.go`; no second state→gate switch exists.
- **AC-12** — `gofmt -l .` is empty, `go vet ./...` is clean, and `go test ./... -race` passes.

---

## Risk register

| Risk | Mitigation |
|---|---|
| Every existing engine test now runs an extra state, changing dispatch counts and timings. | Task 5 step 8 runs the whole engine suite before committing. Tests assert per-state counts, not totals, so only a test asserting a total would break. |
| The fake agent dispatches on the phrase `scoping agent`; editing the prompt's first line silently breaks the engine suite. | `TestScopingPromptRenders` asserts the phrase directly, so the prompt change fails in `internal/prompt` with a clear message rather than as a mysterious engine failure. |
| An agent that always reports `blocked` turns unattended runs into a queue of parked jobs. | `max_scope_rounds` bounds the loop; the prompt sets an explicit bar for blocking; `policies.scoping: off` is the escape hatch. Watch the ratio of `agent_blocked` to `policy_gate` notes over the first few real runs. |
| Inlining `problem.json` into the planning prompt grows it on every job. | The artifact is small by construction — a statement, four short lists, and questions. If it ever is not, that is itself a scoping failure worth seeing. |
| `scope_rounds` increments on a human decision, unlike every other round counter. | Called out in the code comment at the increment site and in the commit message, because the asymmetry is deliberate and would otherwise read as a bug. |
