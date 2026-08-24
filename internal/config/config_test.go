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

func TestDefaultServerListen(t *testing.T) {
	if got := Default().Server.Listen; got != "127.0.0.1:7777" {
		t.Errorf("default server.listen = %q, want 127.0.0.1:7777", got)
	}
	// A config file that says nothing about the server must still come out of
	// Load bound to loopback rather than to nothing at all.
	cfg, err := load(t, minimalTarget)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:7777" {
		t.Errorf("loaded server.listen = %q, want 127.0.0.1:7777", cfg.Server.Listen)
	}
	if _, err := load(t, minimalTarget+"server:\n  listen: 127.0.0.1:9999\n"); err != nil {
		t.Errorf("an explicit loopback listen was rejected: %v", err)
	}
}

// The server has no authentication, so the bind address is the whole of its
// access control: a routable one publishes an approve button to everything that
// can route to this machine. The rule is checked here rather than at bind time
// because `sdlc serve --addr` and the config key must answer it identically,
// and the message has to name the way out (ssh -L) or the operator's next move
// is to widen the bind until it works.
func TestListenMustBeLoopback(t *testing.T) {
	cases := []struct {
		name          string
		addr          string
		allowPortZero bool
		wantErr       bool
	}{
		{name: "IPv4 loopback", addr: "127.0.0.1:7777"},
		{name: "the name an operator types", addr: "localhost:7777"},
		{name: "IPv6 loopback", addr: "[::1]:7777"},
		{name: "any interface", addr: "0.0.0.0:7777", wantErr: true},
		{name: "no host at all", addr: ":7777", wantErr: true},
		{name: "a name that is not localhost", addr: "example.com:7777", wantErr: true},
		{name: "a routable IPv4", addr: "192.168.1.10:7777", wantErr: true},
		{name: "no port", addr: "127.0.0.1", wantErr: true},
		{name: "ephemeral port in a config file", addr: "127.0.0.1:0", wantErr: true},
		{name: "ephemeral port from --addr", addr: "127.0.0.1:0", allowPortZero: true},
		{name: "port out of range", addr: "127.0.0.1:99999", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateListen(tc.addr, tc.allowPortZero)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateListen(%q, %v) = %v, wantErr %v", tc.addr, tc.allowPortZero, err, tc.wantErr)
			}
		})
	}

	// The refusal has to teach the fix, not just refuse.
	err := ValidateListen("0.0.0.0:7777", false)
	if err == nil {
		t.Fatal("0.0.0.0:7777 was accepted")
	}
	for _, want := range []string{"loopback", "ssh -L"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err, want)
		}
	}
}

// The config key goes through the same rule as the flag, and a rejected address
// must fail the load rather than surface when the port is already open.
func TestNonLoopbackListenFailsLoad(t *testing.T) {
	_, err := load(t, minimalTarget+"server:\n  listen: 0.0.0.0:7777\n")
	if err == nil || !strings.Contains(err.Error(), "server.listen") {
		t.Errorf("0.0.0.0 in the config file was accepted: %v", err)
	}
}

// The carve-out this replaces accepted the literal string "localhost" and
// returned it to be bound as-is, which trusts /etc/hosts: a machine that maps
// localhost to a routable address would have passed validation and then bound
// that address. Normalizing to the literal that gets bound closes the gap
// without a lookup, so the assertion is on the returned string, not on nil.
func TestNormalizeListenNeverTrustsAHostname(t *testing.T) {
	got, err := NormalizeListen("localhost:7777", false)
	if err != nil {
		t.Fatalf("NormalizeListen(localhost:7777) = %v", err)
	}
	if got != "127.0.0.1:7777" {
		t.Fatalf("NormalizeListen(localhost:7777) = %q, want 127.0.0.1:7777 — the bound string must be an IP literal", got)
	}
	// Every other name is refused outright rather than resolved, including the
	// ones that look local.
	for _, addr := range []string{"localhost.localdomain:7777", "LOCALHOST:7777", "example.com:7777", "myhost:7777"} {
		if got, err := NormalizeListen(addr, false); err == nil {
			t.Errorf("NormalizeListen(%q) = %q, want a refusal: a name is not an address", addr, got)
		}
	}
}

// Whatever a caller binds has to be the string this function returned, so the
// returned string has to be a valid listen address for every accepted input.
func TestNormalizeListenReturnsABindableAddress(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"127.0.0.1:7777", "127.0.0.1:7777"},
		{"localhost:7777", "127.0.0.1:7777"},
		{"[::1]:7777", "[::1]:7777"},
		{"127.0.0.2:7777", "127.0.0.2:7777"},
	} {
		got, err := NormalizeListen(tc.addr, false)
		if err != nil {
			t.Errorf("NormalizeListen(%q) = %v", tc.addr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeListen(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
	// Port 0 is only reachable through --addr, and the caller binds it too.
	if got, err := NormalizeListen("localhost:0", true); err != nil || got != "127.0.0.1:0" {
		t.Errorf("NormalizeListen(localhost:0, true) = %q, %v; want 127.0.0.1:0", got, err)
	}
}

// Every refusal on this path has to tell the operator what to do instead, and
// the empty value is the one whose answer is "delete the line".
func TestEmptyListenSaysHowToFixIt(t *testing.T) {
	err := ValidateListen("", false)
	if err == nil {
		t.Fatal("an empty listen was accepted")
	}
	for _, want := range []string{"delete the key", defaultListen, "ssh -L"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "missing port in address") {
		t.Errorf("empty listen reported as a malformed host:port: %v", err)
	}
}

// Atoi accepted a sign and leading zeros, so "+7777" and "0007777" named a port
// nobody wrote. The set of accepted ports is exactly the decimal numbers 1-65535
// (plus 0 for --addr).
func TestListenPortAcceptsOnlyPlainDecimal(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:+7777",
		"127.0.0.1:07777",
		"127.0.0.1:0007777",
		"127.0.0.1:-1",
		"127.0.0.1:7777x",
		"127.0.0.1:0x1e61",
		"127.0.0.1: 7777",
	} {
		if got, err := NormalizeListen(addr, false); err == nil {
			t.Errorf("NormalizeListen(%q) = %q, want a refusal", addr, got)
		}
	}
	if _, err := NormalizeListen("127.0.0.1:7777", false); err != nil {
		t.Errorf("a plain decimal port was refused: %v", err)
	}
	// The range still has to be named: "65536" is the number an operator most
	// often reaches for, and the message is the only place the bound appears.
	err := ValidateListen("127.0.0.1:65536", false)
	if err == nil {
		t.Fatal("port 65536 was accepted")
	}
	if !strings.Contains(err.Error(), "1-65535") {
		t.Errorf("out-of-range message %q does not name the range", err)
	}
	if err := ValidateListen("127.0.0.1:65535", false); err != nil {
		t.Errorf("port 65535 was refused: %v", err)
	}
	// allowPortZero still means what it meant.
	if err := ValidateListen("127.0.0.1:0", true); err != nil {
		t.Errorf("port 0 with allowPortZero was refused: %v", err)
	}
	if err := ValidateListen("127.0.0.1:0", false); err == nil {
		t.Error("port 0 was accepted in a config file")
	}
}

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

// sdlc.example.yaml is what new users start from and what CI validates
// against; it must parse cleanly with the new scoping policy and limits.
func TestExampleConfigParsesCleanly(t *testing.T) {
	cfg, err := Load("../../sdlc.example.yaml")
	if err != nil {
		t.Fatalf("sdlc.example.yaml failed to parse: %v", err)
	}
	if !cfg.Policies.ScopingEnabled() {
		t.Error("example config should have scoping enabled by default")
	}
	if cfg.Limits.MaxScopeRounds != 2 {
		t.Errorf("limits.max_scope_rounds = %d, want 2", cfg.Limits.MaxScopeRounds)
	}
	if _, ok := cfg.States[StScoping]; !ok {
		t.Error("example config missing states.SCOPING")
	}
}
