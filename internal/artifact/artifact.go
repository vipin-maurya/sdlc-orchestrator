// Package artifact defines the per-job artifact layout and the JSON schemas
// exchanged between agent states (SPEC §5). Validation is structural and
// strict: a transition never happens on an artifact that does not validate.
package artifact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
}

type Review struct {
	Schema   string    `json:"schema"`
	Reviewed string    `json:"reviewed"` // spec|diff|final
	Findings []Finding `json:"findings"`
	Summary  string    `json:"summary"`
}

// Blockers returns the blocker-severity findings. Pass ⇔ len==0 (SPEC §3.2).
func (r Review) Blockers() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Severity == "blocker" {
			out = append(out, f)
		}
	}
	return out
}

type Implementation struct {
	Schema               string   `json:"schema"`
	Summary              string   `json:"summary"`
	FilesChanged         []string `json:"files_changed"`
	TestsAddedOrChanged  []string `json:"tests_added_or_changed"`
	TestChangeRequested  *string  `json:"test_change_requested"`
}

type Analysis struct {
	Schema         string   `json:"schema"`
	Classification string   `json:"classification"` // code_bug|test_bug|environment|unknown
	Reasoning      string   `json:"reasoning"`
	FailingTargets []string `json:"failing_targets"`
	FixHint        string   `json:"fix_hint"`
}

// --- Loading + validation ----------------------------------------------

// ReadJSON loads path into v after stripping any accidental markdown fences
// an agent wrapped the JSON in.
func ReadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s := strings.TrimSpace(string(data))
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	dec := json.NewDecoder(strings.NewReader(s))
	return dec.Decode(v)
}

func vErr(file, format string, a ...any) error {
	return fmt.Errorf("%s: %s", filepath.Base(file), fmt.Sprintf(format, a...))
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

var validSeverities = map[string]bool{"blocker": true, "major": true, "minor": true, "nit": true}

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
		if !validSeverities[f.Severity] {
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
	if im.Schema != "implementation/1" {
		return nil, vErr(path, `schema must be "implementation/1", got %q`, im.Schema)
	}
	if im.Summary == "" {
		return nil, vErr(path, "summary required")
	}
	return &im, nil
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
