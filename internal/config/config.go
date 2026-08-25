// Package config defines the sdlc.yaml schema (docs/SPEC.md §12), its
// defaults, strict decoding (unknown keys rejected), env-var expansion, and
// cross-field validation including the backend-independence guardrail.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from YAML strings like "30m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// Agent state names. These are the only states that invoke an agent; the
// full state enum lives in the engine package.
const (
	StScoping      = "SCOPING"
	StPlanning     = "PLANNING"
	StDesignReview = "DESIGN_REVIEW"
	StImplementing = "IMPLEMENTING"
	StCodeReview   = "CODE_REVIEW"
	StAnalyzing    = "ANALYZING"
	StFixing       = "FIXING"
	StFinalReview  = "FINAL_REVIEW"
	// StVerifying is not a state the pipeline moves through. It configures the
	// agent that reviews the reviewer: the verify pass runs inside
	// DESIGN_REVIEW, CODE_REVIEW and FINAL_REVIEW, once per gating finding.
	StVerifying = "VERIFYING"
)

// OptionalHumanGates are the gates policies.human_gates can add, in the order
// the pipeline reaches them. Merge and release are always enforced and are
// accepted in the key as a no-op, so they are not listed here.
//
// This is the list, not a copy of it: Validate checks against it and the web
// UI's gate rows are asserted against it, so a gate added in one place cannot
// go missing from the other.
var OptionalHumanGates = []string{"scope", "spec", "code"}

// defaultTransportPatterns match "the backend could not be reached", as
// distinct from "the backend answered and the answer was wrong". They are the
// fallback for a CLI that reports no structured reason — the claude backend's
// own `terminal_reason: "api_error"` is preferred where it is present.
//
// Node's errno strings are here because both shipped backends are Node CLIs
// and surface them verbatim; the prose alternatives cover a CLI that has
// already turned the errno into a sentence.
//
// Returned by a function rather than shared as a package var so two backends
// cannot end up aliasing one slice and mutating each other.
func defaultTransportPatterns() []string {
	return []string{
		`(?i)\b(ENOTFOUND|ECONNREFUSED|ECONNRESET|ETIMEDOUT|EAI_AGAIN|EHOSTUNREACH|ENETUNREACH|EPIPE)\b`,
		`(?i)can'?t reach the API`,
		`(?i)getaddrinfo`,
		`(?i)socket hang ?up`,
		`(?i)network (error|is unreachable)`,
		`(?i)(connection|request) timed out`,
	}
}

// DefaultTransportPatternsForTest exposes the shipped list so the agent
// package can assert it fires on real CLI output. The patterns and the code
// that matches them live in different packages; a copy in the test would prove
// only that the copy is right.
func DefaultTransportPatternsForTest() []string { return defaultTransportPatterns() }

// AgentStates lists every state that must have an entry under `states:`.
var AgentStates = []string{
	StScoping, StPlanning, StDesignReview, StImplementing, StCodeReview,
	StAnalyzing, StFixing, StFinalReview, StVerifying,
}

type Config struct {
	Orchestrator Orchestrator       `yaml:"orchestrator"`
	Database     Database           `yaml:"database"`
	Limits       Limits             `yaml:"limits"`
	Resources    Resources          `yaml:"resources"`
	Backends     map[string]Backend `yaml:"backends"`
	Agents       map[string]Agent   `yaml:"agents"`
	States       map[string]State   `yaml:"states"`
	Policies     Policies           `yaml:"policies"`
	Git          Git                `yaml:"git"`
	Targets      map[string]Target  `yaml:"targets"`
	Server       Server             `yaml:"server"`

	// Path holds the absolute path of the loaded config file (not a YAML key).
	Path string `yaml:"-"`
}

type Orchestrator struct {
	DataDir         string   `yaml:"data_dir"`
	MaxParallelJobs int      `yaml:"max_parallel_jobs"`
	PollInterval    Duration `yaml:"poll_interval"`
	JobIDPrefix     string   `yaml:"job_id_prefix"`
	LockFile        string   `yaml:"lock_file"`
	// StreamOutput echoes each agent action to the engine's console as it
	// happens. Turn it off for a headless run whose stdout is a log file
	// nobody reads; the heartbeat and the progress events stay either way.
	StreamOutput bool `yaml:"stream_output"`
	// HeartbeatInterval is how often a state that is still running says so.
	// It is the only signal for a backend that prints nothing until it exits,
	// and it is what separates "working" from "hung" in `sdlc status`.
	// 0 disables it.
	HeartbeatInterval Duration `yaml:"heartbeat_interval"`
	// GateReminderInterval is how often the engine re-announces a job that is
	// waiting for a human. A gate announced once scrolls off the console and
	// the job then waits forever in silence, which is indistinguishable from
	// the engine having stalled. 0 announces each gate once and never repeats.
	GateReminderInterval Duration `yaml:"gate_reminder_interval"`
}

type Database struct {
	Path        string   `yaml:"path"`
	BusyTimeout Duration `yaml:"busy_timeout"`
}

type Limits struct {
	MaxJobDuration        Duration `yaml:"max_job_duration"`
	MaxDesignReviewRounds int      `yaml:"max_design_review_rounds"`
	// MaxScopeRounds caps the human↔agent re-scoping loop. A loop that neither
	// side ends is worse than a stop, so exhausting it escalates.
	MaxScopeRounds            int      `yaml:"max_scope_rounds"`
	MaxCodeReviewRounds       int      `yaml:"max_code_review_rounds"`
	MaxFixAttempts            int      `yaml:"max_fix_attempts"`
	MaxFlakeRetries           int      `yaml:"max_flake_retries"`
	FlakeRerunCount           int      `yaml:"flake_rerun_count"`
	MaxAgentRetries           int      `yaml:"max_agent_retries"`
	MaxReleaseRetries         int      `yaml:"max_release_retries"`
	ReleaseRetryBackoff       Duration `yaml:"release_retry_backoff"`
	MaxAgentInvocationsPerJob int      `yaml:"max_agent_invocations_per_job"`
	LogExcerptLines           int      `yaml:"log_excerpt_lines"`
	LogErrorPatterns          []string `yaml:"log_error_patterns"`
	// VerifyVotes is how many independent verifiers each gating review finding
	// is put to before it is allowed to stop the pipeline. 0 disables the
	// verify pass entirely and every finding gates on the reviewer's word.
	// Only findings at or above the gate threshold are ever verified — a clean
	// review, the common case, costs nothing.
	VerifyVotes int `yaml:"verify_votes"`
	// VerifyMinConfirm is how many of those votes must confirm for the finding
	// to survive at its stated severity. Below it, the finding is downgraded
	// to a nit and kept in the artifact with its refutations attached.
	VerifyMinConfirm int `yaml:"verify_min_confirm"`
}

type Resources struct {
	GradleSlots int     `yaml:"gradle_slots"`
	Devices     Devices `yaml:"devices"`
}

type Devices struct {
	Serials        []string     `yaml:"serials"`
	Discover       bool         `yaml:"discover"`
	AdbBinary      string       `yaml:"adb_binary"`
	AcquireTimeout Duration     `yaml:"acquire_timeout"`
	BootEmulator   BootEmulator `yaml:"boot_emulator"`
}

type BootEmulator struct {
	Enabled        bool     `yaml:"enabled"`
	AVDName        string   `yaml:"avd_name"`
	EmulatorBinary string   `yaml:"emulator_binary"`
	BootTimeout    Duration `yaml:"boot_timeout"`
	Headless       bool     `yaml:"headless"`
	KillAfterJob   bool     `yaml:"kill_after_job"`
}

type Backend struct {
	// Kind selects the adapter: "claude", "agy", or "exec".
	// Defaults to the backend's map key so the standard `claude:`/`agy:`
	// entries need no kind field.
	Kind           string   `yaml:"kind"`
	Binary         string   `yaml:"binary"`
	DefaultTimeout Duration `yaml:"default_timeout"`
	// PermissionMode: for claude, passed to --permission-mode (e.g.
	// bypassPermissions). For agy, "skip" passes
	// --dangerously-skip-permissions (required for headless shell access —
	// agy soft-denies shell commands otherwise); "default" passes nothing.
	PermissionMode     string   `yaml:"permission_mode"`
	PrintTimeout       Duration `yaml:"print_timeout"`      // agy only
	SettingsFile       string   `yaml:"settings_file"`      // agy only
	EnsurePermissions  []string `yaml:"ensure_permissions"` // agy only
	AssertModel        bool     `yaml:"assert_model"`       // agy only
	QuotaErrorPatterns []string `yaml:"quota_error_patterns"`
	QuotaBackoff       Duration `yaml:"quota_backoff"`
	// TransportErrorPatterns match a backend that could not be reached at all.
	// Kept separate from the quota patterns because the two need different
	// backoffs: a rate limit is a wait measured in tens of minutes, a DNS
	// failure is usually over in one. Both suspend rather than spending the
	// retry budget, which is the point of either list.
	TransportErrorPatterns []string `yaml:"transport_error_patterns"`
	TransportBackoff       Duration `yaml:"transport_backoff"`
	ExpectedVersion        string   `yaml:"expected_version"`
	ExtraArgs              []string `yaml:"extra_args"`
	// ArgvTemplate is used by kind "exec": each element is expanded with
	// {model} {effort} {prompt_file} placeholders. Prompt text is also piped
	// to stdin.
	ArgvTemplate []string `yaml:"argv_template"`
	// StreamJSON asks the claude backend for --output-format stream-json
	// (with --verbose, which that format requires) instead of a single JSON
	// envelope at the end. It is what makes a long state legible while it
	// runs: one line per tool call rather than nothing for twelve minutes.
	// Off by default — it changes the CLI invocation, and older CLI builds
	// may not accept the combination. Backends of other kinds stream whatever
	// they print anyway and ignore this.
	StreamJSON bool `yaml:"stream_json"`
}

type Agent struct {
	Backend string `yaml:"backend"`
	Model   string `yaml:"model"`
	Effort  string `yaml:"effort"`
}

type State struct {
	Agent                 string   `yaml:"agent"`
	Prompt                string   `yaml:"prompt"` // path; "" = embedded default
	Timeout               Duration `yaml:"timeout"`
	AllowedTools          []string `yaml:"allowed_tools"`
	DisallowedTools       []string `yaml:"disallowed_tools"`
	MustDifferBackendFrom string   `yaml:"must_differ_backend_from"`
}

type Policies struct {
	ProtectedBranches        []string `yaml:"protected_branches"`
	ProtectTestsOnCodeBugFix bool     `yaml:"protect_tests_on_code_bug_fix"`
	TestFileGlobs            []string `yaml:"test_file_globs"`
	ReviewerDiffMustBeEmpty  bool     `yaml:"reviewer_diff_must_be_empty"`
	// *BlocksAt is the lowest finding severity that stops the pipeline at each
	// review gate: blocker | major | minor | nit. Findings below the threshold
	// never block, but they are still forwarded to the next agent rather than
	// discarded. All three default to "blocker" so an existing config keeps
	// its behavior; sdlc.example.yaml sets design review to "major", since
	// that loop is one cheap review round against a whole implementation.
	DesignReviewBlocksAt string `yaml:"design_review_blocks_at"`
	CodeReviewBlocksAt   string `yaml:"code_review_blocks_at"`
	FinalReviewBlocksAt  string `yaml:"final_review_blocks_at"`
	// HumanGates adds human checkpoints earlier than the merge gate:
	//   spec — after design review passes, before any code is written
	//   code — after code review passes, before build and test
	// The merge and release gates are always enforced and need not be listed
	// (listing them is accepted, so a config may spell out all four). Empty by
	// default: an unattended run should not acquire a new place to stop
	// because this key exists.
	HumanGates []string `yaml:"human_gates"`
	// Scoping is on|off. Off restores the previous pipeline exactly:
	// CREATED goes straight to PLANNING and no problem.json is produced.
	Scoping string `yaml:"scoping"`
}

// ScopingEnabled reports whether the SCOPING state runs. It is a string rather
// than a bool in YAML to match run_in/push/cleanup_worktrees, and it is read
// through this method so an invalid value — which Validate rejects — can never
// read as enabled.
func (p Policies) ScopingEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(p.Scoping), "on")
}

// HumanGate reports whether an optional human checkpoint is enabled.
func (p Policies) HumanGate(name string) bool {
	for _, g := range p.HumanGates {
		if strings.EqualFold(strings.TrimSpace(g), name) {
			return true
		}
	}
	return false
}

type Git struct {
	CleanupWorktrees      string `yaml:"cleanup_worktrees"` // on_success | always | never
	DeleteBranchOnSuccess bool   `yaml:"delete_branch_on_success"`
}

type Server struct {
	Listen string `yaml:"listen"`
}

type Target struct {
	RepoPath      string    `yaml:"repo_path"`
	DefaultBranch string    `yaml:"default_branch"`
	BranchPrefix  string    `yaml:"branch_prefix"`
	WorktreesDir  string    `yaml:"worktrees_dir"` // "" = <repo_path>/.worktrees
	Gradle        GradleCfg `yaml:"gradle"`
	Build         BuildCfg  `yaml:"build"`
	UnitTest      CmdCfg    `yaml:"unit_test"`
	Lint          LintCfg   `yaml:"lint"`
	UITest        UITestCfg `yaml:"ui_test"`
	Merge         MergeCfg  `yaml:"merge"`
	Ship          ShipCfg   `yaml:"ship"`
}

type GradleCfg struct {
	IsolatedUserHome bool `yaml:"isolated_user_home"`
}

type BuildCfg struct {
	Commands [][]string `yaml:"commands"`
	Timeout  Duration   `yaml:"timeout"`
}

type CmdCfg struct {
	Command []string `yaml:"command"`
	Timeout Duration `yaml:"timeout"`
}

type LintCfg struct {
	Command []string `yaml:"command"`
	RunIn   string   `yaml:"run_in"` // building | testing | off
	Timeout Duration `yaml:"timeout"`
}

type UITestCfg struct {
	Enabled    bool     `yaml:"enabled"`
	Command    []string `yaml:"command"`
	Timeout    Duration `yaml:"timeout"`
	OnNoDevice string   `yaml:"on_no_device"` // skip | fail | wait
}

type MergeCfg struct {
	RebaseBeforeMerge bool   `yaml:"rebase_before_merge"`
	VerifyAfterRebase string `yaml:"verify_after_rebase"` // build | none
	Push              string `yaml:"push"`                // auto | never
}

type ShipCfg struct {
	Command []string `yaml:"command"`
	// ResumeCommand clears the ship tool's sticky halt before a retry when
	// auto_resume_halt is true (e.g. ["autoship", "resume", "--config", "autoship.yaml"]).
	ResumeCommand  []string `yaml:"resume_command"`
	Timeout        Duration `yaml:"timeout"`
	AutoResumeHalt bool     `yaml:"auto_resume_halt"`
}

// Default returns the full default configuration from SPEC §12 (targets empty).
func Default() *Config {
	return &Config{
		Orchestrator: Orchestrator{
			DataDir:              "./data",
			MaxParallelJobs:      2,
			PollInterval:         Duration(3 * time.Second),
			JobIDPrefix:          "JOB",
			StreamOutput:         true,
			HeartbeatInterval:    Duration(60 * time.Second),
			GateReminderInterval: Duration(10 * time.Minute),
		},
		Database: Database{BusyTimeout: Duration(5 * time.Second)},
		Limits: Limits{
			MaxJobDuration:            Duration(12 * time.Hour),
			MaxScopeRounds:            2,
			MaxDesignReviewRounds:     2,
			MaxCodeReviewRounds:       3,
			MaxFixAttempts:            3,
			MaxFlakeRetries:           2,
			FlakeRerunCount:           3,
			MaxAgentRetries:           2,
			MaxReleaseRetries:         3,
			ReleaseRetryBackoff:       Duration(10 * time.Minute),
			MaxAgentInvocationsPerJob: 40,
			LogExcerptLines:           200,
			LogErrorPatterns:          []string{`(?i)error`, `(?i)exception`, `FAILED`},
			VerifyVotes:               3,
			VerifyMinConfirm:          2,
		},
		Resources: Resources{
			GradleSlots: 1,
			Devices: Devices{
				Discover:       true,
				AdbBinary:      "adb",
				AcquireTimeout: Duration(10 * time.Minute),
				BootEmulator: BootEmulator{
					EmulatorBinary: "emulator",
					BootTimeout:    Duration(6 * time.Minute),
					Headless:       true,
					KillAfterJob:   true,
				},
			},
		},
		Backends: map[string]Backend{
			"claude": {
				Kind:           "claude",
				Binary:         "claude",
				DefaultTimeout: Duration(30 * time.Minute),
				PermissionMode: "bypassPermissions",
				QuotaErrorPatterns: []string{
					`(?i)rate.?limit`, `(?i)usage.?limit`, `(?i)overloaded`, `(?i)quota`,
				},
				QuotaBackoff:           Duration(30 * time.Minute),
				TransportErrorPatterns: defaultTransportPatterns(),
				TransportBackoff:       Duration(2 * time.Minute),
			},
			"agy": {
				Kind:           "agy",
				Binary:         "agy",
				DefaultTimeout: Duration(30 * time.Minute),
				PermissionMode: "skip",
				PrintTimeout:   Duration(45 * time.Minute),
				SettingsFile:   "${USERPROFILE}/.gemini/antigravity-cli/settings.json",
				AssertModel:    true,
				QuotaErrorPatterns: []string{
					`(?i)rate.?limit`, `(?i)quota`, `(?i)resource.?exhausted`,
				},
				QuotaBackoff:           Duration(30 * time.Minute),
				TransportErrorPatterns: defaultTransportPatterns(),
				TransportBackoff:       Duration(2 * time.Minute),
			},
		},
		Agents: map[string]Agent{
			"opus":   {Backend: "claude", Model: "opus", Effort: "high"},
			"sonnet": {Backend: "claude", Model: "sonnet"},
			"gemini": {Backend: "agy", Model: "gemini-3-pro", Effort: "medium"},
		},
		// Review/analyze states keep Write (they must emit their .sdlc output
		// file); source-tree immutability is enforced mechanically by the
		// clean-tree check, and the diff they review is pre-staged by the
		// orchestrator so Bash is not needed.
		States: map[string]State{
			// SCOPING keeps Bash, unlike the reviewers: scoping "Fix 1.0.6
			// issues" means reading what 1.0.6 actually changed, and git log is
			// how that is answered. Read-only exploration is the whole job of
			// this state. It still may not edit the tree — discardTreeChanges
			// enforces it.
			StScoping:      {Agent: "opus", Timeout: Duration(15 * time.Minute), DisallowedTools: []string{"Edit", "NotebookEdit"}},
			StPlanning:     {Agent: "opus", Timeout: Duration(30 * time.Minute)},
			StDesignReview: {Agent: "sonnet", Timeout: Duration(15 * time.Minute), DisallowedTools: []string{"Edit", "NotebookEdit", "Bash"}},
			StImplementing: {Agent: "gemini", Timeout: Duration(45 * time.Minute)},
			StCodeReview:   {Agent: "opus", Timeout: Duration(20 * time.Minute), DisallowedTools: []string{"Edit", "NotebookEdit", "Bash"}, MustDifferBackendFrom: StImplementing},
			StAnalyzing:    {Agent: "sonnet", Timeout: Duration(10 * time.Minute), DisallowedTools: []string{"Edit", "NotebookEdit", "Bash"}},
			StFixing:       {Agent: "gemini", Timeout: Duration(45 * time.Minute)},
			StFinalReview:  {Agent: "opus", Timeout: Duration(20 * time.Minute), DisallowedTools: []string{"Edit", "NotebookEdit", "Bash"}, MustDifferBackendFrom: StImplementing},
			// Verifiers keep Bash: the rule that refutes most false findings is
			// "check the resolved dependency, do not answer from memory", and
			// that means unzipping a jar or reading the dependency cache. They
			// still may not edit the tree — the clean-tree check enforces it.
			StVerifying: {Agent: "opus", Timeout: Duration(15 * time.Minute), DisallowedTools: []string{"Edit", "NotebookEdit"}},
		},
		Policies: Policies{
			ProtectedBranches:        []string{"main", "master"},
			ProtectTestsOnCodeBugFix: true,
			TestFileGlobs: []string{
				"**/src/test/**", "**/src/androidTest/**", "**/*Test.kt", "**/*Test.java",
			},
			ReviewerDiffMustBeEmpty: true,
			DesignReviewBlocksAt:    "blocker",
			CodeReviewBlocksAt:      "blocker",
			FinalReviewBlocksAt:     "blocker",
			Scoping:                 "on",
		},
		Git:    Git{CleanupWorktrees: "on_success"},
		Server: Server{Listen: defaultListen},
	}
}

// defaultListen is the address a config that says nothing about the server
// gets, and the one named in the advice below.
const defaultListen = "127.0.0.1:7777"

// loopbackAdvice is appended to every address rejected for where it binds. A
// refusal that only says no leaves the operator with a server they cannot reach
// from their laptop and no sanctioned way to get there, which is how the
// address ends up at 0.0.0.0 anyway.
const loopbackAdvice = "sdlc serve has no authentication at all, so only a loopback address may be bound — " +
	"anything routable publishes an approve button to the network. " +
	"Keep 127.0.0.1 and forward it instead: ssh -L 7777:127.0.0.1:7777 host"

// NormalizeListen returns the exact string the caller must hand to net.Listen,
// or reports why addr must not be bound. The caller MUST bind the returned
// value and never the raw config value: validating one string and binding
// another leaves a window in which the two disagree — a name resolved at bind
// time can answer with a routable address that the check never saw — and this
// function is the whole of `sdlc serve`'s access control.
//
// It is exported because `sdlc serve --addr` takes the same rule as the config
// key, and a second copy of "which hosts count as loopback" is how the flag and
// the file would drift.
//
// allowPortZero is true only for --addr: an ephemeral port the operator cannot
// predict is not a useful thing to write in a config file, but it is exactly
// how a test binds without racing for a fixed one.
func NormalizeListen(addr string, allowPortZero bool) (string, error) {
	// An empty value is the one rejection an operator can fix by deleting a
	// line, so say that rather than reporting a malformed host:port.
	if addr == "" {
		return "", fmt.Errorf("is empty: delete the key to take the default %s, or name a loopback address. %s", defaultListen, loopbackAdvice)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("%q is not host:port: %v", addr, err)
	}
	// An empty host is not "unset", it is every interface — the one spelling of
	// a routable bind that looks like an omission rather than a decision.
	if host == "" {
		return "", fmt.Errorf("%q has no host, which binds every interface. %s", addr, loopbackAdvice)
	}
	// "localhost" is accepted because it is what an operator types, but it is
	// rewritten here to the literal that gets bound rather than passed through:
	// an /etc/hosts entry mapping localhost to a routable address would
	// otherwise sail through this check and bind that address. No name is ever
	// looked up: a DNS answer can change after validation, and a hostname that
	// resolves to a routable address today is exactly the case this check
	// exists to refuse.
	if host == "localhost" {
		host = "127.0.0.1"
	} else if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("%q is not a loopback address. %s", addr, loopbackAdvice)
	}
	// ParseUint rather than Atoi: Atoi also accepts "+7777" and "0007777",
	// which are not ports anybody meant to write, and its int result then needs
	// a second range check that ParseUint's bitSize does for free.
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return "", fmt.Errorf("%q: port %s is out of range (1-65535)", addr, port)
		}
		return "", fmt.Errorf("%q: port %q is not a number", addr, port)
	}
	if len(port) > 1 && port[0] == '0' {
		return "", fmt.Errorf("%q: port %q has a leading zero; write it as %d", addr, port, n)
	}
	if n == 0 && !allowPortZero {
		return "", fmt.Errorf("%q: port 0 asks the kernel for whatever port is free, which nobody can then be told to open; pick one", addr)
	}
	return net.JoinHostPort(host, port), nil
}

// ValidateListen reports why addr must not be bound. It is a thin wrapper over
// NormalizeListen for callers that only check — config validation, `sdlc
// validate` — and is not enough for a caller that then binds: that one must
// bind NormalizeListen's result.
//
// allowPortZero is true only for --addr, as above.
func ValidateListen(addr string, allowPortZero bool) error {
	_, err := NormalizeListen(addr, allowPortZero)
	return err
}

var envVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} with the environment value when VAR is set;
// unset variables are left verbatim (so e.g. a literal ${data_dir} written
// by a user surfaces as an obvious path error rather than silently vanishing).
func expandEnv(s string) string {
	return envVarRe.ReplaceAllStringFunc(s, func(m string) string {
		name := envVarRe.FindStringSubmatch(m)[1]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return m
	})
}

// Load reads path, decodes it strictly over Default(), applies computed
// defaults and env expansion, and validates. path=="" resolves via
// SDLC_CONFIG then ./sdlc.yaml.
func Load(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("SDLC_CONFIG")
	}
	if path == "" {
		path = "sdlc.yaml"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := Default()
	dec := yaml.NewDecoder(strings.NewReader(expandEnv(string(raw))))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", abs, err)
	}
	cfg.Path = abs
	cfg.applyComputedDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	return cfg, nil
}

// applyComputedDefaults fills fields whose default depends on other fields,
// and re-fills per-state/backend zero values that a partial user entry reset.
func (c *Config) applyComputedDefaults() {
	base := c.Path
	if base != "" {
		base = filepath.Dir(base)
	} else {
		base, _ = os.Getwd()
	}
	if !filepath.IsAbs(c.Orchestrator.DataDir) {
		c.Orchestrator.DataDir = filepath.Join(base, c.Orchestrator.DataDir)
	}
	if c.Database.Path == "" {
		c.Database.Path = filepath.Join(c.Orchestrator.DataDir, "sdlc.db")
	}
	if c.Orchestrator.LockFile == "" {
		c.Orchestrator.LockFile = filepath.Join(c.Orchestrator.DataDir, "engine.lock")
	}
	if c.Resources.Devices.AdbBinary == "" {
		c.Resources.Devices.AdbBinary = "adb"
	}
	def := Default()
	for name, b := range c.Backends {
		if b.Kind == "" {
			if name == "claude" || name == "agy" {
				b.Kind = name
			} else {
				b.Kind = "exec"
			}
		}
		if d, ok := def.Backends[name]; ok {
			if b.Binary == "" {
				b.Binary = d.Binary
			}
			if b.DefaultTimeout == 0 {
				b.DefaultTimeout = d.DefaultTimeout
			}
			if b.PermissionMode == "" {
				b.PermissionMode = d.PermissionMode
			}
			if b.PrintTimeout == 0 {
				b.PrintTimeout = d.PrintTimeout
			}
			if b.SettingsFile == "" {
				b.SettingsFile = d.SettingsFile
			}
			if len(b.QuotaErrorPatterns) == 0 {
				b.QuotaErrorPatterns = d.QuotaErrorPatterns
			}
			if b.QuotaBackoff == 0 {
				b.QuotaBackoff = d.QuotaBackoff
			}
			if len(b.TransportErrorPatterns) == 0 {
				b.TransportErrorPatterns = d.TransportErrorPatterns
			}
			if b.TransportBackoff == 0 {
				b.TransportBackoff = d.TransportBackoff
			}
		}
		if b.DefaultTimeout == 0 {
			b.DefaultTimeout = Duration(30 * time.Minute)
		}
		if b.QuotaBackoff == 0 {
			b.QuotaBackoff = Duration(30 * time.Minute)
		}
		// A custom `exec` backend has no defaults entry to inherit from, and a
		// backend with no transport patterns silently loses the protection
		// entirely — so every backend gets the list, not just the two shipped
		// ones.
		if len(b.TransportErrorPatterns) == 0 {
			b.TransportErrorPatterns = defaultTransportPatterns()
		}
		if b.TransportBackoff == 0 {
			b.TransportBackoff = Duration(2 * time.Minute)
		}
		b.SettingsFile = expandEnv(b.SettingsFile)
		c.Backends[name] = b
	}
	for name, s := range c.States {
		d, ok := def.States[name]
		if !ok {
			continue
		}
		if s.Agent == "" {
			s.Agent = d.Agent
		}
		if s.Timeout == 0 {
			s.Timeout = d.Timeout
		}
		c.States[name] = s
	}
	for _, name := range AgentStates {
		if _, ok := c.States[name]; !ok {
			c.States[name] = def.States[name]
		}
	}
	for key, t := range c.Targets {
		if t.DefaultBranch == "" {
			t.DefaultBranch = "main"
		}
		if t.BranchPrefix == "" {
			t.BranchPrefix = "sdlc/"
		}
		if t.WorktreesDir == "" {
			t.WorktreesDir = filepath.Join(t.RepoPath, ".worktrees")
		}
		if t.Build.Timeout == 0 {
			t.Build.Timeout = Duration(30 * time.Minute)
		}
		if t.UnitTest.Timeout == 0 {
			t.UnitTest.Timeout = Duration(30 * time.Minute)
		}
		if t.Lint.Timeout == 0 {
			t.Lint.Timeout = Duration(20 * time.Minute)
		}
		if t.Lint.RunIn == "" {
			t.Lint.RunIn = "off"
		}
		if t.UITest.Timeout == 0 {
			t.UITest.Timeout = Duration(40 * time.Minute)
		}
		if t.UITest.OnNoDevice == "" {
			t.UITest.OnNoDevice = "skip"
		}
		if t.Merge.VerifyAfterRebase == "" {
			t.Merge.VerifyAfterRebase = "build"
		}
		if t.Merge.Push == "" {
			t.Merge.Push = "auto"
		}
		if t.Ship.Timeout == 0 {
			t.Ship.Timeout = Duration(60 * time.Minute)
		}
		c.Targets[key] = t
	}
}

// Validate performs cross-field validation, including the
// must_differ_backend_from independence guardrail (SPEC §6.4).
func (c *Config) Validate() error {
	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if c.Orchestrator.MaxParallelJobs < 1 {
		fail("orchestrator.max_parallel_jobs must be >= 1")
	}
	if c.Resources.GradleSlots < 1 {
		fail("resources.gradle_slots must be >= 1")
	}
	switch c.Git.CleanupWorktrees {
	case "on_success", "always", "never":
	default:
		fail("git.cleanup_worktrees must be on_success|always|never, got %q", c.Git.CleanupWorktrees)
	}
	for key, sev := range map[string]string{
		"design_review_blocks_at": c.Policies.DesignReviewBlocksAt,
		"code_review_blocks_at":   c.Policies.CodeReviewBlocksAt,
		"final_review_blocks_at":  c.Policies.FinalReviewBlocksAt,
	} {
		switch sev {
		case "blocker", "major", "minor", "nit":
		default:
			fail("policies.%s must be blocker|major|minor|nit, got %q", key, sev)
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.Policies.Scoping)) {
	case "on", "off":
	default:
		fail("policies.scoping must be on|off, got %q", c.Policies.Scoping)
	}
	if c.Limits.MaxScopeRounds < 1 {
		fail("limits.max_scope_rounds must be >= 1")
	}
	for _, g := range c.Policies.HumanGates {
		switch n := strings.ToLower(strings.TrimSpace(g)); {
		case slices.Contains(OptionalHumanGates, n), n == "merge", n == "release":
		default:
			fail("policies.human_gates: unknown gate %q (%s|merge|release)", g,
				strings.Join(OptionalHumanGates, "|"))
		}
	}
	for name, b := range c.Backends {
		switch b.Kind {
		case "claude", "agy":
		case "exec":
			if len(b.ArgvTemplate) == 0 {
				fail("backends.%s: kind exec requires argv_template", name)
			}
		default:
			fail("backends.%s: unknown kind %q (claude|agy|exec)", name, b.Kind)
		}
		for _, p := range b.QuotaErrorPatterns {
			if _, err := regexp.Compile(p); err != nil {
				fail("backends.%s.quota_error_patterns %q: %v", name, p, err)
			}
		}
		for _, p := range b.TransportErrorPatterns {
			if _, err := regexp.Compile(p); err != nil {
				fail("backends.%s.transport_error_patterns %q: %v", name, p, err)
			}
		}
	}
	for name, a := range c.Agents {
		if _, ok := c.Backends[a.Backend]; !ok {
			fail("agents.%s.backend %q is not defined under backends", name, a.Backend)
		}
		if a.Model == "" {
			fail("agents.%s.model must be set", name)
		}
	}
	for _, sn := range AgentStates {
		s := c.States[sn]
		ag, ok := c.Agents[s.Agent]
		if !ok {
			fail("states.%s.agent %q is not defined under agents", sn, s.Agent)
			continue
		}
		if s.MustDifferBackendFrom != "" {
			other, ok := c.States[s.MustDifferBackendFrom]
			if !ok {
				fail("states.%s.must_differ_backend_from %q is not a configured state", sn, s.MustDifferBackendFrom)
			} else if oag, ok := c.Agents[other.Agent]; ok && oag.Backend == ag.Backend {
				fail("independence violation: states.%s (agent %s, backend %s) must use a different backend than states.%s (agent %s, backend %s)",
					sn, s.Agent, ag.Backend, s.MustDifferBackendFrom, other.Agent, oag.Backend)
			}
		}
	}
	for name := range c.States {
		found := false
		for _, sn := range AgentStates {
			if name == sn {
				found = true
				break
			}
		}
		if !found {
			fail("states.%s is not an agent state (valid: %s)", name, strings.Join(AgentStates, ", "))
		}
	}
	if c.Limits.VerifyVotes < 0 {
		fail("limits.verify_votes must be >= 0 (0 disables the verify pass)")
	}
	if c.Limits.VerifyVotes > 0 {
		if c.Limits.VerifyMinConfirm < 1 {
			fail("limits.verify_min_confirm must be >= 1 when verify_votes > 0")
		}
		if c.Limits.VerifyMinConfirm > c.Limits.VerifyVotes {
			fail("limits.verify_min_confirm (%d) cannot exceed limits.verify_votes (%d): no finding could ever survive",
				c.Limits.VerifyMinConfirm, c.Limits.VerifyVotes)
		}
	}
	if err := ValidateListen(c.Server.Listen, false); err != nil {
		fail("server.listen %v", err)
	}
	for _, p := range c.Limits.LogErrorPatterns {
		if _, err := regexp.Compile(p); err != nil {
			fail("limits.log_error_patterns %q: %v", p, err)
		}
	}
	for key, t := range c.Targets {
		if t.RepoPath == "" {
			fail("targets.%s.repo_path must be set", key)
		}
		if len(t.Build.Commands) == 0 {
			fail("targets.%s.build.commands must have at least one command", key)
		}
		if len(t.UnitTest.Command) == 0 {
			fail("targets.%s.unit_test.command must be set", key)
		}
		if t.UITest.Enabled && len(t.UITest.Command) == 0 {
			fail("targets.%s.ui_test.command must be set when ui_test.enabled", key)
		}
		switch t.UITest.OnNoDevice {
		case "skip", "fail", "wait":
		default:
			fail("targets.%s.ui_test.on_no_device must be skip|fail|wait", key)
		}
		switch t.Lint.RunIn {
		case "building", "testing", "off":
		default:
			fail("targets.%s.lint.run_in must be building|testing|off", key)
		}
		switch t.Merge.VerifyAfterRebase {
		case "build", "none":
		default:
			fail("targets.%s.merge.verify_after_rebase must be build|none", key)
		}
		switch t.Merge.Push {
		case "auto", "never":
		default:
			fail("targets.%s.merge.push must be auto|never", key)
		}
		for _, pb := range c.Policies.ProtectedBranches {
			if strings.HasPrefix(pb, t.BranchPrefix) {
				fail("targets.%s.branch_prefix %q would produce branches shadowing protected branch %q", key, t.BranchPrefix, pb)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("config invalid:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// Target returns the target config for key.
func (c *Config) Target(key string) (Target, error) {
	t, ok := c.Targets[key]
	if !ok {
		keys := make([]string, 0, len(c.Targets))
		for k := range c.Targets {
			keys = append(keys, k)
		}
		return Target{}, fmt.Errorf("unknown target %q (configured: %s)", key, strings.Join(keys, ", "))
	}
	return t, nil
}

// AgentFor resolves the agent and backend for an agent state.
func (c *Config) AgentFor(state string) (agentName string, ag Agent, b Backend, st State, err error) {
	st, ok := c.States[state]
	if !ok {
		err = fmt.Errorf("state %s has no agent configuration", state)
		return
	}
	ag, ok = c.Agents[st.Agent]
	if !ok {
		err = fmt.Errorf("states.%s.agent %q not found", state, st.Agent)
		return
	}
	b, ok = c.Backends[ag.Backend]
	if !ok {
		err = fmt.Errorf("agents.%s.backend %q not found", st.Agent, ag.Backend)
		return
	}
	return st.Agent, ag, b, st, nil
}
