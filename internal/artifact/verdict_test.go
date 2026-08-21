package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodVerdict = `{"schema":"verdict/1","finding_id":"F1","verdict":"refuted",
  "evidence":"disassembled navigation-runtime-2.6.0.jar from ~/.gradle/caches",
  "reasoning":"getPreviousBackStackEntry skips NavGraph entries in this version"}`

func TestLoadVerdict(t *testing.T) {
	v, err := LoadVerdict(writeTemp(t, "v.json", goodVerdict))
	if err != nil {
		t.Fatal(err)
	}
	if v.FindingID != "F1" || v.Verdict != "refuted" || v.Confirmed() {
		t.Errorf("got %+v", v)
	}
}

// Agents on Windows backends emit BOMs routinely; a verdict is no different
// from any other artifact in that respect.
func TestLoadVerdictStripsBOM(t *testing.T) {
	if _, err := LoadVerdict(writeTemp(t, "v.json", bom+goodVerdict)); err != nil {
		t.Fatalf("BOM-prefixed verdict rejected: %v", err)
	}
}

// A verdict with no evidence is an opinion, and an opinion is exactly what the
// verify pass exists to stop trusting.
func TestLoadVerdictRequiresEvidence(t *testing.T) {
	cases := map[string]string{
		"no evidence":  `{"schema":"verdict/1","finding_id":"F1","verdict":"refuted","evidence":"  ","reasoning":"because"}`,
		"no reasoning": `{"schema":"verdict/1","finding_id":"F1","verdict":"refuted","evidence":"read the jar","reasoning":""}`,
		"bad verdict":  `{"schema":"verdict/1","finding_id":"F1","verdict":"probably","evidence":"e","reasoning":"r"}`,
		"bad schema":   `{"schema":"verdict/2","finding_id":"F1","verdict":"refuted","evidence":"e","reasoning":"r"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadVerdict(writeTemp(t, "v.json", body)); err == nil {
				t.Error("accepted an invalid verdict")
			}
		})
	}
}

func verdicts(results ...string) []Verdict {
	out := make([]Verdict, 0, len(results))
	for _, r := range results {
		out = append(out, Verdict{Schema: "verdict/1", FindingID: "F1", Verdict: r,
			Evidence: "e", Reasoning: "r"})
	}
	return out
}

// The whole vote matrix at the shipped default (3 votes, 2 to confirm), plus
// the boundaries either side of it.
func TestApplyVerdictsVoteMatrix(t *testing.T) {
	cases := []struct {
		name       string
		votes      []string
		minConfirm int
		survives   bool
	}{
		{"3 of 3", []string{"confirmed", "confirmed", "confirmed"}, 2, true},
		{"2 of 3", []string{"confirmed", "refuted", "confirmed"}, 2, true},
		{"1 of 3", []string{"refuted", "refuted", "confirmed"}, 2, false},
		{"0 of 3", []string{"refuted", "refuted", "refuted"}, 2, false},
		{"2 of 3, unanimity required", []string{"confirmed", "refuted", "confirmed"}, 3, false},
		{"1 of 3, one vote enough", []string{"refuted", "refuted", "confirmed"}, 1, true},
		// A partial fan-out still decides on the votes it has: two dropped
		// verdicts must not be counted as refutations they never made.
		{"1 of 1 counted", []string{"confirmed"}, 2, false},
		{"2 of 2 counted", []string{"confirmed", "confirmed"}, 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rev := &Review{
				Schema: "review/1", Reviewed: "spec", Summary: "s",
				Findings: []Finding{{ID: "F1", Severity: "blocker", Description: "d"}},
			}
			surviving, downgraded := ApplyVerdicts(rev, map[string][]Verdict{"F1": verdicts(tc.votes...)}, tc.minConfirm)

			f := rev.Findings[0]
			if f.Verification == nil {
				t.Fatal("no verification attached")
			}
			if f.Verification.Survived != tc.survives {
				t.Errorf("survived=%v, want %v (%+v)", f.Verification.Survived, tc.survives, f.Verification)
			}
			if tc.survives {
				if f.Severity != "blocker" {
					t.Errorf("severity=%q, want blocker", f.Severity)
				}
				if len(surviving) != 1 || len(downgraded) != 0 {
					t.Errorf("surviving=%d downgraded=%d, want 1/0", len(surviving), len(downgraded))
				}
				if len(rev.Blocking("blocker")) != 1 {
					t.Error("a surviving finding must still gate")
				}
			} else {
				if f.Severity != DowngradedSeverity {
					t.Errorf("severity=%q, want %q", f.Severity, DowngradedSeverity)
				}
				if len(surviving) != 0 || len(downgraded) != 1 {
					t.Errorf("surviving=%d downgraded=%d, want 0/1", len(surviving), len(downgraded))
				}
				if len(rev.Blocking("blocker")) != 0 {
					t.Error("a refuted finding must not gate")
				}
			}
			// Either way the finding stays in the artifact with every verdict.
			if len(rev.Findings) != 1 {
				t.Errorf("%d finding(s) left, want 1 — findings are annotated, never removed", len(rev.Findings))
			}
			if got := len(f.Verification.Verdicts); got != len(tc.votes) {
				t.Errorf("kept %d verdict(s), want %d", got, len(tc.votes))
			}
			if f.Verification.OriginalSeverity != "blocker" {
				t.Errorf("original_severity=%q, want blocker", f.Verification.OriginalSeverity)
			}
		})
	}
}

// Findings nobody voted on are left exactly as they were — that is every
// finding below the gating threshold, which is never verified.
func TestApplyVerdictsLeavesUnvotedFindingsAlone(t *testing.T) {
	rev := &Review{
		Schema: "review/1", Reviewed: "spec", Summary: "s",
		Findings: []Finding{
			{ID: "F1", Severity: "blocker", Description: "gating"},
			{ID: "F2", Severity: "minor", Description: "advisory"},
		},
	}
	ApplyVerdicts(rev, map[string][]Verdict{"F1": verdicts("refuted", "refuted")}, 2)

	if rev.Findings[0].Severity != DowngradedSeverity {
		t.Errorf("F1 severity=%q, want downgraded", rev.Findings[0].Severity)
	}
	if rev.Findings[1].Severity != "minor" {
		t.Errorf("F2 severity=%q, want minor untouched", rev.Findings[1].Severity)
	}
	if rev.Findings[1].Verification != nil {
		t.Error("F2 was annotated despite never being voted on")
	}
}

// JOB-1's F1, replayed end to end through the aggregation: a confident,
// well-reasoned false positive that two verifiers refute must stop gating.
func TestJob1FalsePositiveIsRefuted(t *testing.T) {
	rev := &Review{
		Schema: "review/1", Reviewed: "spec", Summary: "one finding",
		Findings: []Finding{{
			ID: "F1", Severity: "major", File: "ScanSummaryFragment.kt",
			Description: "previousBackStackEntry from ScanSummaryFragment resolves to the " +
				"nav_approval_flow graph entry, not the nav_transform entry, so the Snackbar never shows",
			Recommendation: "use getBackStackEntry(R.id.nav_transform)",
		}},
	}
	ApplyVerdicts(rev, map[string][]Verdict{"F1": {
		{Schema: "verdict/1", FindingID: "F1", Verdict: "refuted",
			Evidence:  "disassembled navigation-runtime-2.6.0.jar: getPreviousBackStackEntry drops the top entry then applies firstOrNull { it.destination !is NavGraph }",
			Reasoning: "graph entries are skipped, so the write lands on nav_transform"},
		{Schema: "verdict/1", FindingID: "F1", Verdict: "refuted",
			Evidence: "same artifact, bytecode at offset 78-84 confirms the instanceof NavGraph filter", Reasoning: "claim does not hold at this version"},
		{Schema: "verdict/1", FindingID: "F1", Verdict: "confirmed",
			Evidence: "read the fragment source", Reasoning: "looks wrong to me"},
	}}, 2)

	if got := len(rev.Blocking("major")); got != 0 {
		t.Fatalf("%d finding(s) still gate at major; the false positive would have forced a rework round", got)
	}
	v := rev.Findings[0].Verification
	if v == nil || v.Confirmed != 1 || v.Refuted != 2 {
		t.Fatalf("verification = %+v, want 1 confirmed / 2 refuted", v)
	}
	// The refutation itself has to survive into the record: the next reader
	// must be able to see the claim was examined, not just that it vanished.
	if len(v.Verdicts) != 3 || v.Verdicts[0].Evidence == "" {
		t.Error("the verdicts were not preserved on the finding")
	}
}

// Two findings that share an id (or leave it blank) must not share one bucket
// of verdicts: the engine mints unique ids before verifying, and this pins the
// aggregation's half of that contract — a bucket only ever touches its finding.
func TestApplyVerdictsMatchesByID(t *testing.T) {
	rev := &Review{
		Schema: "review/1", Reviewed: "diff", Summary: "s",
		Findings: []Finding{
			{ID: "F1", Severity: "blocker", Description: "first"},
			{ID: "F2", Severity: "blocker", Description: "second"},
		},
	}
	ApplyVerdicts(rev, map[string][]Verdict{
		"F1": verdicts("refuted", "refuted", "refuted"),
		"F2": verdicts("confirmed", "confirmed", "refuted"),
	}, 2)

	if rev.Findings[0].Severity != DowngradedSeverity {
		t.Errorf("F1 severity=%q, want downgraded", rev.Findings[0].Severity)
	}
	if rev.Findings[1].Severity != "blocker" {
		t.Errorf("F2 severity=%q, want blocker (2 of 3 confirmed it)", rev.Findings[1].Severity)
	}
	if got := len(rev.Blocking("blocker")); got != 1 {
		t.Errorf("%d finding(s) gate, want exactly 1", got)
	}
}
