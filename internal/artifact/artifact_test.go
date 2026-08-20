package artifact

import (
	"os"
	"path/filepath"
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

func TestLoadReviewAndBlockers(t *testing.T) {
	r, err := LoadReview(write(t, "r.json", `{"schema":"review/1","reviewed":"diff",
		"findings":[{"id":"F1","severity":"blocker","description":"d","recommendation":"r"},
		            {"id":"F2","severity":"minor","description":"d","recommendation":"r"}],
		"summary":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Blockers()) != 1 {
		t.Errorf("want 1 blocker, got %d", len(r.Blockers()))
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
