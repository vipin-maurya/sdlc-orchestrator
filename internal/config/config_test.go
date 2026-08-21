package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sdlc.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

const minimalTarget = `
targets:
  demo:
    repo_path: C:\repo
    build: { commands: [["cmd", "/c", "exit", "0"]] }
    unit_test: { command: ["cmd", "/c", "exit", "0"] }
`

func TestDefaultsApplied(t *testing.T) {
	cfg, err := load(t, minimalTarget)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.MaxFixAttempts != 3 {
		t.Errorf("default max_fix_attempts = %d", cfg.Limits.MaxFixAttempts)
	}
	if cfg.States[StCodeReview].MustDifferBackendFrom != StImplementing {
		t.Error("default independence constraint missing on CODE_REVIEW")
	}
	tg := cfg.Targets["demo"]
	if tg.DefaultBranch != "main" || tg.BranchPrefix != "sdlc/" {
		t.Errorf("target defaults not applied: %+v", tg)
	}
	if tg.WorktreesDir != filepath.Join(`C:\repo`, ".worktrees") {
		t.Errorf("worktrees_dir default = %s", tg.WorktreesDir)
	}
	if tg.UITest.OnNoDevice != "skip" {
		t.Errorf("ui_test.on_no_device default = %q", tg.UITest.OnNoDevice)
	}
	if cfg.Database.Path != filepath.Join(cfg.Orchestrator.DataDir, "sdlc.db") {
		t.Errorf("db path default = %s", cfg.Database.Path)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	_, err := load(t, "orchestrator:\n  max_paralel_jobs: 3\n"+minimalTarget)
	if err == nil || !strings.Contains(err.Error(), "field max_paralel_jobs not found") {
		t.Errorf("typo key not rejected: %v", err)
	}
}

func TestIndependenceViolationFailsLoad(t *testing.T) {
	// Route CODE_REVIEW to the same backend as IMPLEMENTING.
	_, err := load(t, minimalTarget+`
states:
  IMPLEMENTING: { agent: opus }
  CODE_REVIEW:  { agent: sonnet, must_differ_backend_from: IMPLEMENTING }
`)
	if err == nil || !strings.Contains(err.Error(), "independence violation") {
		t.Errorf("independence violation not caught: %v", err)
	}
}

func TestBranchPrefixCannotShadowProtected(t *testing.T) {
	_, err := load(t, `
targets:
  demo:
    repo_path: C:\repo
    branch_prefix: "main"
    build: { commands: [["x"]] }
    unit_test: { command: ["x"] }
`)
	if err == nil || !strings.Contains(err.Error(), "protected branch") {
		t.Errorf("shadowing branch_prefix not caught: %v", err)
	}
}

func TestUnknownAgentBackendRejected(t *testing.T) {
	_, err := load(t, minimalTarget+`
agents:
  ghost: { backend: nonexistent, model: m }
`)
	if err == nil || !strings.Contains(err.Error(), "not defined under backends") {
		t.Errorf("unknown backend not caught: %v", err)
	}
}

func TestEnvExpansion(t *testing.T) {
	os.Setenv("SDLC_TEST_REPO", `C:\expanded`)
	defer os.Unsetenv("SDLC_TEST_REPO")
	cfg, err := load(t, `
targets:
  demo:
    repo_path: ${SDLC_TEST_REPO}
    build: { commands: [["x"]] }
    unit_test: { command: ["x"] }
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Targets["demo"].RepoPath != `C:\expanded` {
		t.Errorf("env not expanded: %s", cfg.Targets["demo"].RepoPath)
	}
}

func TestDurationParsing(t *testing.T) {
	cfg, err := load(t, minimalTarget+"limits:\n  max_job_duration: 90m\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.MaxJobDuration.D().Minutes() != 90 {
		t.Errorf("duration = %s", cfg.Limits.MaxJobDuration)
	}
	if _, err := load(t, minimalTarget+"limits:\n  max_job_duration: ninety\n"); err == nil {
		t.Error("invalid duration accepted")
	}
}

// human_gates is a hand-written list in a YAML file, so "Spec", "SPEC" and a
// stray space around it all have to name the same checkpoint. A config that
// quietly means "no gate" because somebody capitalised a word is a config that
// stops nothing, and the operator only finds out when the job merges itself.
func TestHumanGateMatching(t *testing.T) {
	cases := []struct {
		name  string
		gates []string
		probe string
		want  bool
	}{
		{"exact match", []string{"spec"}, "spec", true},
		{"upper case in the config", []string{"SPEC"}, "spec", true},
		{"mixed case in the config", []string{"Code"}, "code", true},
		{"whitespace around the entry", []string{"  spec  "}, "spec", true},
		{"one of several entries", []string{"merge", "code", "release"}, "code", true},
		{"a different gate is listed", []string{"code"}, "spec", false},
		{"nothing is listed", nil, "spec", false},
		{"empty slice is not a wildcard", []string{}, "code", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Policies{HumanGates: tc.gates}
			if got := p.HumanGate(tc.probe); got != tc.want {
				t.Errorf("Policies{HumanGates: %q}.HumanGate(%q) = %v, want %v",
					tc.gates, tc.probe, got, tc.want)
			}
		})
	}
}

// The two gates the pipeline can reach must both come back on, and neither
// must switch the other on: `human_gates: [spec]` stopping a job before build
// as well would be a config that does more than it says.
func TestHumanGatesAreIndependent(t *testing.T) {
	cfg, err := load(t, minimalTarget+"policies:\n  human_gates: [spec]\n")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Policies.HumanGate("spec") {
		t.Error("human_gates: [spec] did not enable the spec gate")
	}
	if cfg.Policies.HumanGate("code") {
		t.Error("human_gates: [spec] enabled the code gate as well")
	}
}

// An unknown gate name is a typo, and a typo that loads is a checkpoint the
// operator believes in and never gets.
func TestUnknownHumanGateRejected(t *testing.T) {
	_, err := load(t, minimalTarget+"policies:\n  human_gates: [speck]\n")
	if err == nil || !strings.Contains(err.Error(), "unknown gate") {
		t.Errorf("human_gates: [speck] was accepted: %v", err)
	}
}
