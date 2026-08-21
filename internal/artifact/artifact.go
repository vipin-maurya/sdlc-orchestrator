// Package artifact defines the per-job artifact layout and the JSON schemas
// exchanged between agent states (SPEC §5). Validation is structural and
// strict: a transition never happens on an artifact that does not validate.
package artifact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Dir returns the job's root data directory: <data_dir>/jobs/<job-id>.
func Dir(dataDir, jobID string) string { return filepath.Join(dataDir, "jobs", jobID) }

func ArtifactsDir(dataDir, jobID string) string { return filepath.Join(Dir(dataDir, jobID), "artifacts") }
func LogsDir(dataDir, jobID string) string      { return filepath.Join(Dir(dataDir, jobID), "logs") }
func PromptsDir(dataDir, jobID string) string   { return filepath.Join(Dir(dataDir, jobID), "prompts") }

// EnsureLayout creates the job directory tree and writes issue.md.
func EnsureLayout(dataDir, jobID, issueTitle, issueBody string) error {
	for _, d := range []string{ArtifactsDir(dataDir, jobID), LogsDir(dataDir, jobID), PromptsDir(dataDir, jobID)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	issue := fmt.Sprintf("# %s\n\n%s\n", issueTitle, issueBody)
	return os.WriteFile(filepath.Join(Dir(dataDir, jobID), "issue.md"), []byte(issue), 0o644)
}

// --- Schemas ------------------------------------------------------------

type Spec struct {
	Schema                string   `json:"schema"`
	IssueSummary          string   `json:"issue_summary"`
	Approach              string   `json:"approach"`
	AffectedFiles         []string `json:"affected_files"`
	AcceptanceCriteria    []string `json:"acceptance_criteria"`
	ErrorPaths            []string `json:"error_paths"`
	OutOfScope            []string `json:"out_of_scope"`
	CompatibilityConcerns []string `json:"compatibility_concerns"`
}

type PlanStep struct {
	ID           string   `json:"id"`
	Description  string   `json:"description"`
	Files        []string `json:"files"`
	Verification string   `json:"verification"`
}

type Plan struct {
	Schema string     `json:"schema"`
	Steps  []PlanStep `json:"steps"`
	Risks  []string   `json:"risks"`
}

type Finding struct {
	ID             string `json:"id"`
	Severity       string `json:"severity"` // blocker|major|minor|nit
	File           string `json:"file,omitempty"`
	Description    string `json:"description"`
	Recommendation string `json:"recommendation"`
	// Verification is attached by the verify pass to findings that could gate.
	// It is nil on a raw review artifact and on findings never put to a vote.
	Verification *Verification `json:"verification,omitempty"`
}

// Verdict is one independent verifier's answer about one finding
// (schema verdict/1). Each verifier is asked to refute; "refuted" is the
// answer it is nudged toward, so a confirmation costs something to reach.
type Verdict struct {
	Schema    string `json:"schema"`
	FindingID string `json:"finding_id"`
	Verdict   string `json:"verdict"` // confirmed | refuted
	Evidence  string `json:"evidence"`
	Reasoning string `json:"reasoning"`
}

// Confirmed reports whether this verdict upholds the finding.
func (v Verdict) Confirmed() bool { return v.Verdict == "confirmed" }

// Verification is the aggregate of a finding's verdicts, recorded on the
// finding itself. A refuted finding is never deleted: the record has to show
// what was considered and why it was set aside, or the next reviewer has no
// way to know the question was already asked.
type Verification struct {
	Votes            int       `json:"votes"`
	Confirmed        int       `json:"confirmed"`
	Refuted          int       `json:"refuted"`
	MinConfirm       int       `json:"min_confirm"`
	Survived         bool      `json:"survived"`
	OriginalSeverity string    `json:"original_severity"`
	Verdicts         []Verdict `json:"verdicts"`
}

type Review struct {
	Schema   string    `json:"schema"`
	Reviewed string    `json:"reviewed"` // spec|diff|final
	Findings []Finding `json:"findings"`
	Summary  string    `json:"summary"`
}

// SeverityRank orders finding severities from advisory to fatal. It is the
// only place the ordering is defined; config validates thresholds against it.
var SeverityRank = map[string]int{"nit": 0, "minor": 1, "major": 2, "blocker": 3}

// ValidSeverity reports whether s is a known finding severity.
func ValidSeverity(s string) bool { _, ok := SeverityRank[s]; return ok }

// Blocking returns the findings at or above threshold severity — the set that
// must be resolved before the job advances past this gate (SPEC §3.2).
// Pass ⇔ len==0. An unknown threshold falls back to "blocker" (config
// validation rejects those, so this only guards a zero value).
func (r Review) Blocking(threshold string) []Finding {
	min, ok := SeverityRank[threshold]
	if !ok {
		min = SeverityRank["blocker"]
	}
	var out []Finding
	for _, f := range r.Findings {
		if SeverityRank[f.Severity] >= min {
			out = append(out, f)
		}
	}
	return out
}

// DowngradedSeverity is where a finding lands when its verifiers refuse to
// confirm it. It is not deleted and not silently deleted-by-severity either:
// "nit" keeps it in the artifact and out of every gate.
const DowngradedSeverity = "nit"

// ApplyVerdicts folds each finding's verdicts back into the review.
//
// A finding survives at its stated severity when at least minConfirm verifiers
// confirmed it; otherwise it is downgraded to a nit, annotated with every
// verdict, and stops gating. Findings with no verdicts are untouched — that is
// the case for everything below the gating threshold, which is never voted on.
//
// The asymmetry is deliberate. A reviewer produces findings from reading code;
// a verifier is asked to disprove one specific claim with evidence. Making the
// second job the one that decides is the whole point: a fluent, well-reasoned,
// wrong finding reads exactly like a right one until somebody checks it.
func ApplyVerdicts(r *Review, byFinding map[string][]Verdict, minConfirm int) (surviving, downgraded []Finding) {
	if minConfirm < 1 {
		minConfirm = 1
	}
	for i := range r.Findings {
		f := &r.Findings[i]
		vs := byFinding[f.ID]
		if len(vs) == 0 {
			continue
		}
		confirmed := 0
		for _, v := range vs {
			if v.Confirmed() {
				confirmed++
			}
		}
		v := &Verification{
			Votes:            len(vs),
			Confirmed:        confirmed,
			Refuted:          len(vs) - confirmed,
			MinConfirm:       minConfirm,
			Survived:         confirmed >= minConfirm,
			OriginalSeverity: f.Severity,
			Verdicts:         vs,
		}
		f.Verification = v
		if v.Survived {
			surviving = append(surviving, *f)
			continue
		}
		f.Severity = DowngradedSeverity
		downgraded = append(downgraded, *f)
	}
	return surviving, downgraded
}

// StepResult is one plan step's disposition, reported by the implementer.
type StepResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`         // done | skipped
	Note   string `json:"note,omitempty"` // required when skipped
}

type Implementation struct {
	Schema              string   `json:"schema"`
	Summary             string   `json:"summary"`
	FilesChanged        []string `json:"files_changed"`
	TestsAddedOrChanged []string `json:"tests_added_or_changed"`
	// StepsCompleted accounts for every step in plan.json. The schema keeps it
	// optional because fix.json shares implementation/1 and a fix has no plan
	// steps to account for; IMPLEMENTING enforces full coverage in its
	// post-condition, where the plan is actually in hand.
	StepsCompleted      []StepResult `json:"steps_completed,omitempty"`
	TestChangeRequested *string      `json:"test_change_requested"`
}

// CheckStepCoverage reports which plan steps the implementation failed to
// account for. Silence about a step is the failure mode this exists to catch:
// before steps_completed was part of the schema, the field was decoded away
// and two verification steps vanished from a run with no trace.
func (im Implementation) CheckStepCoverage(p *Plan) (missing []string) {
	seen := make(map[string]bool, len(im.StepsCompleted))
	for _, s := range im.StepsCompleted {
		seen[s.ID] = true
	}
	for _, st := range p.Steps {
		if !seen[st.ID] {
			missing = append(missing, st.ID)
		}
	}
	return missing
}

// SkippedSteps returns the steps reported as deliberately not done.
func (im Implementation) SkippedSteps() []StepResult {
	var out []StepResult
	for _, s := range im.StepsCompleted {
		if s.Status == "skipped" {
			out = append(out, s)
		}
	}
	return out
}

type Analysis struct {
	Schema         string   `json:"schema"`
	Classification string   `json:"classification"` // code_bug|test_bug|environment|unknown
	Reasoning      string   `json:"reasoning"`
	FailingTargets []string `json:"failing_targets"`
	FixHint        string   `json:"fix_hint"`
}

// --- Loading + validation ----------------------------------------------

// bom is the UTF-8 byte-order mark. Agents writing through PowerShell (or
// any editor defaulting to "UTF-8 with signature") prefix it; it is not
// whitespace, so TrimSpace leaves it in place and encoding/json rejects the
// file with "invalid character 'ï'". Every artifact decode strips it first.
const bom = "\ufeff"

// trimJunk removes a leading BOM and surrounding whitespace, in either order.
func trimJunk(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, bom)
	return strings.TrimSpace(s)
}

// ReadJSON loads path into v after stripping a UTF-8 BOM and any accidental
// markdown fences an agent wrapped the JSON in. Text after the JSON value is
// ignored: the stream decoder reads exactly one value and stops there.
func ReadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s := trimJunk(string(data))
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		s = trimJunk(strings.TrimSuffix(trimJunk(s), "```"))
	}
	dec := json.NewDecoder(strings.NewReader(s))
	return dec.Decode(v)
}

func vErr(file, format string, a ...any) error {
	return fmt.Errorf("%s: %s", filepath.Base(file), fmt.Sprintf(format, a...))
}

// presentKeys lists the top-level keys of a JSON object file, for validation
// errors. "the field is required" is not actionable when the agent believes it
// wrote the field under another name; the two lists side by side are.
func presentKeys(path string) string {
	var m map[string]json.RawMessage
	if err := ReadJSON(path, &m); err != nil || len(m) == 0 {
		return "(none — the file is not a JSON object)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func LoadSpec(path string) (*Spec, error) {
	var s Spec
	if err := ReadJSON(path, &s); err != nil {
		return nil, err
	}
	if s.Schema != "spec/1" {
		return nil, vErr(path, `schema must be "spec/1", got %q`, s.Schema)
	}
	if s.IssueSummary == "" || s.Approach == "" {
		return nil, vErr(path, "issue_summary and approach are required")
	}
	if len(s.AffectedFiles) == 0 {
		return nil, vErr(path, "affected_files must be non-empty")
	}
	if len(s.AcceptanceCriteria) == 0 {
		return nil, vErr(path, "acceptance_criteria must be non-empty")
	}
	return &s, nil
}

func LoadPlan(path string) (*Plan, error) {
	var p Plan
	if err := ReadJSON(path, &p); err != nil {
		return nil, err
	}
	if p.Schema != "plan/1" {
		return nil, vErr(path, `schema must be "plan/1", got %q`, p.Schema)
	}
	if len(p.Steps) == 0 {
		return nil, vErr(path, "steps must be non-empty")
	}
	for i, st := range p.Steps {
		if st.ID == "" || st.Description == "" {
			return nil, vErr(path, "steps[%d]: id and description are required", i)
		}
	}
	return &p, nil
}

func LoadReview(path string) (*Review, error) {
	var r Review
	if err := ReadJSON(path, &r); err != nil {
		return nil, err
	}
	if r.Schema != "review/1" {
		return nil, vErr(path, `schema must be "review/1", got %q`, r.Schema)
	}
	switch r.Reviewed {
	case "spec", "diff", "final":
	default:
		return nil, vErr(path, `reviewed must be spec|diff|final, got %q`, r.Reviewed)
	}
	for i, f := range r.Findings {
		if !ValidSeverity(f.Severity) {
			return nil, vErr(path, "findings[%d].severity %q invalid (blocker|major|minor|nit)", i, f.Severity)
		}
		if f.Description == "" {
			return nil, vErr(path, "findings[%d].description required", i)
		}
	}
	if r.Summary == "" {
		return nil, vErr(path, "summary required")
	}
	return &r, nil
}

func LoadImplementation(path string) (*Implementation, error) {
	var im Implementation
	if err := ReadJSON(path, &im); err != nil {
		return nil, err
	}
	// A backend that writes the wrong shape entirely — right idea, invented
	// key names — gets one field named at a time otherwise, and the retry
	// prompt inherits that vagueness. List every missing required field and
	// show what the file did contain, so a retry can see the mismatch.
	var missing []string
	if im.Schema != "implementation/1" {
		missing = append(missing, `schema (must be "implementation/1")`)
	}
	if im.Summary == "" {
		missing = append(missing, "summary (non-empty; it becomes the commit message)")
	}
	if len(missing) > 0 {
		return nil, vErr(path, "missing or invalid required field(s): %s; keys actually present: %s",
			strings.Join(missing, ", "), presentKeys(path))
	}
	for i, s := range im.StepsCompleted {
		if s.ID == "" {
			return nil, vErr(path, "steps_completed[%d].id required", i)
		}
		switch s.Status {
		case "done":
		case "skipped":
			// A skip is allowed, but never silently: the note is what a human
			// reads when asking why a planned step did not happen.
			if s.Note == "" {
				return nil, vErr(path, "steps_completed[%d] (%s) is skipped and must have a note explaining why", i, s.ID)
			}
		default:
			return nil, vErr(path, "steps_completed[%d] (%s).status %q invalid (done|skipped)", i, s.ID, s.Status)
		}
	}
	return &im, nil
}

func LoadVerdict(path string) (*Verdict, error) {
	var v Verdict
	if err := ReadJSON(path, &v); err != nil {
		return nil, err
	}
	if v.Schema != "verdict/1" {
		return nil, vErr(path, `schema must be "verdict/1", got %q; keys present: %s`, v.Schema, presentKeys(path))
	}
	switch v.Verdict {
	case "confirmed", "refuted":
	default:
		return nil, vErr(path, `verdict must be confirmed|refuted, got %q`, v.Verdict)
	}
	// Evidence is the entire contract. A verdict without it is an opinion,
	// and an opinion is what the verify pass exists to stop trusting.
	if strings.TrimSpace(v.Evidence) == "" {
		return nil, vErr(path, "evidence required: state what you inspected and what it showed")
	}
	if strings.TrimSpace(v.Reasoning) == "" {
		return nil, vErr(path, "reasoning required: say why that settles the claim")
	}
	return &v, nil
}

var validClassifications = map[string]bool{
	"code_bug": true, "test_bug": true, "environment": true, "unknown": true,
}

func LoadAnalysis(path string) (*Analysis, error) {
	var a Analysis
	if err := ReadJSON(path, &a); err != nil {
		return nil, err
	}
	if a.Schema != "analysis/1" {
		return nil, vErr(path, `schema must be "analysis/1", got %q`, a.Schema)
	}
	if !validClassifications[a.Classification] {
		return nil, vErr(path, "classification %q invalid (code_bug|test_bug|environment|unknown)", a.Classification)
	}
	if a.Reasoning == "" {
		return nil, vErr(path, "reasoning required")
	}
	return &a, nil
}
