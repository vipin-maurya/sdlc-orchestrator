package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSpec(t *testing.T) {
	good := `{"schema":"spec/1","issue_summary":"s","approach":"a",
		"affected_files":["f"],"acceptance_criteria":["c"],
		"error_paths":[],"out_of_scope":[],"compatibility_concerns":[]}`
	if _, err := LoadSpec(write(t, "spec.json", good)); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	// Fenced JSON (agent wrapped it in markdown) must still parse.
	if _, err := LoadSpec(write(t, "spec.json", "```json\n"+good+"\n```")); err != nil {
		t.Fatalf("fenced spec rejected: %v", err)
	}
	bad := []string{
		`{"schema":"spec/2","issue_summary":"s","approach":"a","affected_files":["f"],"acceptance_criteria":["c"]}`,
		`{"schema":"spec/1","issue_summary":"","approach":"a","affected_files":["f"],"acceptance_criteria":["c"]}`,
		`{"schema":"spec/1","issue_summary":"s","approach":"a","affected_files":[],"acceptance_criteria":["c"]}`,
		`{"schema":"spec/1","issue_summary":"s","approach":"a","affected_files":["f"],"acceptance_criteria":[]}`,
		`not json`,
	}
	for i, b := range bad {
		if _, err := LoadSpec(write(t, "spec.json", b)); err == nil {
			t.Errorf("bad spec %d accepted", i)
		}
	}
}

func TestLoadReviewAndBlocking(t *testing.T) {
	r, err := LoadReview(write(t, "r.json", `{"schema":"review/1","reviewed":"diff",
		"findings":[{"id":"F1","severity":"blocker","description":"d","recommendation":"r"},
		            {"id":"F2","severity":"major","description":"d","recommendation":"r"},
		            {"id":"F3","severity":"minor","description":"d","recommendation":"r"},
		            {"id":"F4","severity":"nit","description":"d","recommendation":"r"}],
		"summary":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	// The threshold is inclusive, and it is the whole point of the gate: JOB-1
	// shipped a real defect because its one finding was graded "major" against
	// a hardcoded "blocker" threshold.
	for threshold, want := range map[string]int{"blocker": 1, "major": 2, "minor": 3, "nit": 4} {
		if got := len(r.Blocking(threshold)); got != want {
			t.Errorf("Blocking(%q) = %d finding(s), want %d", threshold, got, want)
		}
	}
	// An unset threshold must not silently block on nits.
	if got := len(r.Blocking("")); got != 1 {
		t.Errorf(`Blocking("") = %d, want 1 (fall back to blocker)`, got)
	}
	if _, err := LoadReview(write(t, "r.json", `{"schema":"review/1","reviewed":"nope","findings":[],"summary":"s"}`)); err == nil {
		t.Error("invalid reviewed value accepted")
	}
	if _, err := LoadReview(write(t, "r.json", `{"schema":"review/1","reviewed":"diff",
		"findings":[{"id":"F1","severity":"catastrophic","description":"d","recommendation":"r"}],"summary":"s"}`)); err == nil {
		t.Error("invalid severity accepted")
	}
}

func TestLoadAnalysis(t *testing.T) {
	for _, c := range []string{"code_bug", "test_bug", "environment", "unknown"} {
		if _, err := LoadAnalysis(write(t, "a.json",
			`{"schema":"analysis/1","classification":"`+c+`","reasoning":"r","failing_targets":[],"fix_hint":""}`)); err != nil {
			t.Errorf("classification %s rejected: %v", c, err)
		}
	}
	// "flaky" is deliberately NOT a valid classification (SPEC §3.2).
	if _, err := LoadAnalysis(write(t, "a.json",
		`{"schema":"analysis/1","classification":"flaky","reasoning":"r"}`)); err == nil {
		t.Error("flaky classification accepted — the schema must forbid it")
	}
}

func TestLoadImplementation(t *testing.T) {
	im, err := LoadImplementation(write(t, "i.json", `{"schema":"implementation/1","summary":"s",
		"files_changed":["a"],"tests_added_or_changed":[],"test_change_requested":"needs it"}`))
	if err != nil {
		t.Fatal(err)
	}
	if im.TestChangeRequested == nil || *im.TestChangeRequested != "needs it" {
		t.Error("test_change_requested not parsed")
	}
	im2, err := LoadImplementation(write(t, "i.json", `{"schema":"implementation/1","summary":"s",
		"files_changed":[],"tests_added_or_changed":[],"test_change_requested":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if im2.TestChangeRequested != nil {
		t.Error("null test_change_requested should be nil")
	}
}

func TestStepsCompleted(t *testing.T) {
	plan := &Plan{Steps: []PlanStep{{ID: "S1"}, {ID: "S2"}, {ID: "S3"}}}

	im, err := LoadImplementation(write(t, "i.json", `{"schema":"implementation/1","summary":"s",
		"files_changed":["a"],"tests_added_or_changed":[],
		"steps_completed":[{"id":"S1","status":"done"},
		                   {"id":"S2","status":"skipped","note":"orchestrator-verified"}],
		"test_change_requested":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := im.CheckStepCoverage(plan); len(got) != 1 || got[0] != "S3" {
		t.Errorf("CheckStepCoverage = %v, want [S3]", got)
	}
	if got := im.SkippedSteps(); len(got) != 1 || got[0].ID != "S2" {
		t.Errorf("SkippedSteps = %v, want one entry for S2", got)
	}

	// fix.json shares implementation/1 and has no plan steps to account for,
	// so an absent steps_completed must stay valid at the schema level.
	if _, err := LoadImplementation(write(t, "f.json", `{"schema":"implementation/1","summary":"fixed",
		"files_changed":["a"],"tests_added_or_changed":[],"test_change_requested":null}`)); err != nil {
		t.Errorf("fix.json without steps_completed rejected: %v", err)
	}

	bad := []string{
		// A skip with no note is the silent omission this is meant to prevent.
		`{"schema":"implementation/1","summary":"s","steps_completed":[{"id":"S1","status":"skipped"}]}`,
		`{"schema":"implementation/1","summary":"s","steps_completed":[{"id":"","status":"done"}]}`,
		`{"schema":"implementation/1","summary":"s","steps_completed":[{"id":"S1","status":"partly"}]}`,
		`{"schema":"implementation/1","summary":"s","steps_completed":[{"id":"S1"}]}`,
	}
	for i, b := range bad {
		if _, err := LoadImplementation(write(t, "i.json", b)); err == nil {
			t.Errorf("bad steps_completed %d accepted", i)
		}
	}
}

// TestReadJSONTolerance pins the wrappers agents actually emit. The BOM cases
// are the reason this exists: a Windows agent wrote implementation.json with a
// leading U+FEFF, encoding/json failed with "invalid character 'ï'", and a
// complete 13-minute implementation was scored as a post-condition failure and
// thrown away.
func TestReadJSONTolerance(t *testing.T) {
	good := `{"schema":"spec/1","issue_summary":"s","approach":"a",` +
		`"affected_files":["f"],"acceptance_criteria":["c"]}`
	fenced := "```json\n" + good + "\n```"

	cases := map[string]string{
		"plain":            good,
		"bom":              bom + good,
		"bom before fence": bom + fenced,
		"bom inside fence": "```json\n" + bom + good + "\n```",
		"blank line first": "\n\n" + bom + good,
		"crlf":             strings.ReplaceAll(bom+fenced, "\n", "\r\n"),
		"trailing prose":   good + "\n\nThat is the spec.\n",
		"bare fence":       "```\n" + good + "\n```",
	}
	for name, in := range cases {
		if _, err := LoadSpec(write(t, "spec.json", in)); err != nil {
			t.Errorf("%s: rejected: %v", name, err)
		}
	}

	// Leading prose before a fence is still unsupported: the decoder starts at
	// the first byte, so this must fail rather than silently half-parse.
	if _, err := LoadSpec(write(t, "spec.json", "Here you go:\n"+fenced)); err == nil {
		t.Error("leading prose accepted; if that is now intended, update this test")
	}
}
