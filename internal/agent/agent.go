// Package agent implements the AgentRunner contract (SPEC §6): one Run
// function over backend adapters for the claude CLI, the agy CLI, and a
// generic exec template. Exit codes are hints — post-conditions are checked
// by the caller against the filesystem — but quota classification, model
// assertion, and token accounting happen here.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vipinm/sdlc-orchestrator/internal/config"
	"github.com/vipinm/sdlc-orchestrator/internal/execx"
)

type Spec struct {
	BackendName string
	Backend     config.Backend
	Model       string
	Effort      string
	Prompt      string
	Cwd         string
	Timeout     time.Duration
	Allowed     []string
	Disallowed  []string
	LogPath     string // raw stdout+stderr envelope is written here
}

type Result struct {
	ExitCode  int
	Stdout    string
	Model     string // model reported by the CLI envelope ("" if unknown)
	TokensIn  int64
	TokensOut int64
	QuotaHit  bool
	TimedOut  bool
	// ModelMismatch is set when assert_model is on and the envelope reported
	// a different model than requested.
	ModelMismatch bool
}

// maxArgPrompt is the largest prompt passed as a CLI argument; longer prompts
// go via stdin (Windows command lines cap around 32K chars).
const maxArgPrompt = 24_000

type Runner struct {
	mu       sync.Mutex
	versions map[string]string // binary path -> --version output
}

func NewRunner() *Runner { return &Runner{versions: map[string]string{}} }

// BinaryVersion returns (and caches) `binary --version` for the event log.
func (r *Runner) BinaryVersion(binary string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.versions[binary]; ok {
		return v
	}
	_, out, err := execx.RunCapture(context.Background(), execx.Cmd{
		Argv:    []string{binary, "--version"},
		Timeout: 30 * time.Second,
	}, 4096)
	v := strings.TrimSpace(out)
	if err != nil {
		v = "unknown (" + err.Error() + ")"
	}
	r.versions[binary] = v
	return v
}

func (r *Runner) Run(ctx context.Context, s Spec) (Result, error) {
	switch s.Backend.Kind {
	case "claude":
		return r.runClaude(ctx, s)
	case "agy":
		return r.runAgy(ctx, s)
	case "exec":
		return r.runExec(ctx, s)
	default:
		return Result{}, fmt.Errorf("unknown backend kind %q", s.Backend.Kind)
	}
}

func (r *Runner) runClaude(ctx context.Context, s Spec) (Result, error) {
	argv := []string{s.Backend.Binary, "-p", "--output-format", "json"}
	if s.Model != "" {
		argv = append(argv, "--model", s.Model)
	}
	if s.Backend.PermissionMode != "" {
		argv = append(argv, "--permission-mode", s.Backend.PermissionMode)
	}
	if len(s.Allowed) > 0 {
		argv = append(argv, "--allowedTools", strings.Join(s.Allowed, ","))
	}
	if len(s.Disallowed) > 0 {
		argv = append(argv, "--disallowedTools", strings.Join(s.Disallowed, ","))
	}
	argv = append(argv, s.Backend.ExtraArgs...)
	// The prompt always goes via stdin: claude's --allowedTools/--disallowedTools
	// are variadic and would swallow a trailing positional prompt argument.
	return r.invoke(ctx, s, argv, s.Prompt)
}

func (r *Runner) runAgy(ctx context.Context, s Spec) (Result, error) {
	argv := []string{s.Backend.Binary, "-p"}
	if len(s.Prompt) <= maxArgPrompt {
		argv = append(argv, s.Prompt)
	}
	argv = append(argv, "--output-format", "json")
	if s.Model != "" {
		argv = append(argv, "--model", s.Model)
	}
	if s.Effort != "" {
		argv = append(argv, "--effort", s.Effort)
	}
	if s.Backend.PrintTimeout > 0 {
		argv = append(argv, "--print-timeout", s.Backend.PrintTimeout.D().String())
	}
	if s.Backend.PermissionMode == "skip" {
		// agy soft-denies shell commands in headless mode otherwise; agents
		// only ever run inside their job worktree.
		argv = append(argv, "--dangerously-skip-permissions")
	}
	argv = append(argv, s.Backend.ExtraArgs...)
	stdin := ""
	if len(s.Prompt) > maxArgPrompt {
		stdin = s.Prompt
	}
	res, err := r.invoke(ctx, s, argv, stdin)
	if err == nil && s.Backend.AssertModel && s.Model != "" && res.Model != "" &&
		!strings.EqualFold(res.Model, s.Model) && !strings.Contains(strings.ToLower(res.Model), strings.ToLower(s.Model)) {
		res.ModelMismatch = true
	}
	return res, err
}

func (r *Runner) runExec(ctx context.Context, s Spec) (Result, error) {
	// The name must be unique per invocation, not merely unlikely to repeat.
	// A timestamp is not: Windows' clock granularity is coarse enough that
	// agents spawned together in the same millisecond collide, and then two
	// backends read one prompt while a third deletes it out from under them.
	// The verify pass runs several agents at once, so this is a live path.
	f, err := os.CreateTemp("", "sdlc-prompt-*.md")
	if err != nil {
		return Result{}, err
	}
	promptFile := f.Name()
	if _, err := f.WriteString(s.Prompt); err != nil {
		f.Close()
		os.Remove(promptFile)
		return Result{}, err
	}
	if err := f.Close(); err != nil {
		os.Remove(promptFile)
		return Result{}, err
	}
	defer os.Remove(promptFile)
	repl := strings.NewReplacer(
		"{model}", s.Model,
		"{effort}", s.Effort,
		"{prompt_file}", promptFile,
	)
	argv := make([]string, 0, len(s.Backend.ArgvTemplate)+1)
	argv = append(argv, s.Backend.Binary)
	for _, a := range s.Backend.ArgvTemplate {
		argv = append(argv, repl.Replace(a))
	}
	argv = append(argv, s.Backend.ExtraArgs...)
	return r.invoke(ctx, s, argv, s.Prompt)
}

func (r *Runner) invoke(ctx context.Context, s Spec, argv []string, stdin string) (Result, error) {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = s.Backend.DefaultTimeout.D()
	}
	res, out, err := execx.RunCapture(ctx, execx.Cmd{
		Argv:    argv,
		Dir:     s.Cwd,
		Timeout: timeout,
		Stdin:   stdin,
	}, 8<<20)
	if s.LogPath != "" {
		_ = os.MkdirAll(filepath.Dir(s.LogPath), 0o755)
		_ = os.WriteFile(s.LogPath, []byte(out), 0o644)
	}
	if err != nil {
		return Result{ExitCode: -1, Stdout: out}, err
	}
	result := Result{ExitCode: res.ExitCode, Stdout: out, TimedOut: res.TimedOut}
	parseEnvelope(out, &result)
	result.QuotaHit = matchesAny(out, s.Backend.QuotaErrorPatterns)
	return result, nil
}

func matchesAny(text string, patterns []string) bool {
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			continue // validated at config load; be lenient here
		}
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// parseEnvelope extracts model and token counts from a CLI JSON envelope,
// tolerating both claude's shape and unknown shapes. Best-effort by design.
func parseEnvelope(out string, res *Result) {
	out = strings.TrimSpace(out)
	start := strings.Index(out, "{")
	if start < 0 {
		return
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out[start:]), &m); err != nil {
		// Maybe multiple JSON objects (stream) — try the last line.
		lines := strings.Split(out, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			ln := strings.TrimSpace(lines[i])
			if strings.HasPrefix(ln, "{") && json.Unmarshal([]byte(ln), &m) == nil {
				break
			}
		}
		if m == nil {
			return
		}
	}
	if usage, ok := m["usage"].(map[string]any); ok {
		res.TokensIn = asInt64(usage["input_tokens"])
		res.TokensOut = asInt64(usage["output_tokens"])
	}
	// claude: modelUsage is {"model-id": {...}}; agy variants may use "model".
	if mu, ok := m["modelUsage"].(map[string]any); ok {
		for k := range mu {
			res.Model = k
			break
		}
	}
	for _, key := range []string{"model", "model_id", "modelId"} {
		if v, ok := m[key].(string); ok && v != "" {
			res.Model = v
			break
		}
	}
	if ie, ok := m["is_error"].(bool); ok && ie && res.ExitCode == 0 {
		res.ExitCode = 1 // claude reports errors inside a 0-exit envelope
	}
	if st, ok := m["status"].(string); ok && strings.EqualFold(st, "ERROR") && res.ExitCode == 0 {
		res.ExitCode = 1 // agy reports {"status":"ERROR",...}
	}
}

func asInt64(v any) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return 0
}
