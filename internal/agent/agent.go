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
	// OnProgress, when set, receives one condensed line per action the agent
	// takes, as it takes it. Backends that emit a single JSON envelope at the
	// end call it once or not at all; the caller's heartbeat covers those.
	OnProgress func(string)
}

type Result struct {
	ExitCode  int
	Stdout    string
	Model     string // model reported by the CLI envelope ("" if unknown)
	TokensIn  int64
	TokensOut int64
	QuotaHit  bool
	// Unreachable means the backend could not be talked to at all — DNS
	// failure, refused connection, a dead API endpoint. It is deliberately
	// distinct from "the agent ran and did the wrong thing": nothing about the
	// job caused it and nothing about the job will fix it, so spending a retry
	// attempt on it converts a passing network blip into a dead job.
	Unreachable bool
	TimedOut    bool
	// envelopeFound and resultText record what parseEnvelope saw, so
	// classification can scope itself to the CLI's own error message instead
	// of the whole transcript. Unexported: they are an implementation detail
	// of how Unreachable is decided.
	envelopeFound  bool
	resultText     string
	terminalReason string
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
	// stream-json turns the run into one JSON object per event, which is the
	// only way to see what the agent is doing before it exits. It is opt-in
	// per backend because it changes the CLI contract (and requires --verbose),
	// and a config that works today must keep working untouched.
	if s.Backend.StreamJSON {
		argv = []string{s.Backend.Binary, "-p", "--output-format", "stream-json", "--verbose"}
	}
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
	promptArg, err := agyPromptArg(s)
	if err != nil {
		return Result{}, err
	}
	argv := []string{s.Backend.Binary, "-p", promptArg}
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
	// No stdin: agy has no flag that reads a plain prompt from one. -p always
	// takes a value, so a long prompt travels as a file instead. See
	// agyPromptArg.
	res, err := r.invoke(ctx, s, argv, "")
	if err == nil && s.Backend.AssertModel && s.Model != "" && res.Model != "" &&
		!strings.EqualFold(res.Model, s.Model) && !strings.Contains(strings.ToLower(res.Model), strings.ToLower(s.Model)) {
		res.ModelMismatch = true
	}
	return res, err
}

// agyPromptFile is where a too-long prompt is left for the agent to read. It
// sits in the exchange directory the orchestrator already recreates before each
// state and already keeps out of git, so it needs no new cleanup and cannot be
// committed by accident.
const agyPromptFile = ".sdlc/prompt.md"

// agyPromptArg returns the value for agy's -p.
//
// Short prompts go inline. A long one cannot: Windows refuses to launch a
// process whose command line exceeds ~32,767 characters (WinError 206), and
// that is a limit on what the kernel receives, so no amount of shell quoting,
// heredocs or $(cat) gets around it.
//
// The previous attempt passed the prompt on stdin and left -p with nothing
// attached, which made agy read the *next argument* as the prompt: it ran with
// the prompt "--output-format" and discarded the real instruction. agy says so
// itself, and it killed a real job — see docs/run-issues.md R9. There is no
// stdin route to fall back to either: -p/--print/--prompt are one flag and all
// require a value, and the only input agy takes from stdin is
// --input-format stream-json, which forces a different output format too.
//
// So a long prompt is written into the worktree and -p carries a pointer to it.
// The pointer is deliberately blunt: the agent has one thing to do before it
// starts, and a vague reference is how it ends up acting on the pointer instead
// of the instructions.
func agyPromptArg(s Spec) (string, error) {
	if len(s.Prompt) <= maxArgPrompt {
		return s.Prompt, nil
	}
	dir := s.Cwd
	if dir == "" {
		// No worktree (unit tests, ad-hoc runs). Anywhere readable will do,
		// because the pointer below is an absolute path in that case.
		dir = os.TempDir()
	}
	path := filepath.Join(dir, filepath.FromSlash(agyPromptFile))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("prompt file dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(s.Prompt), 0o644); err != nil {
		return "", fmt.Errorf("write prompt file: %w", err)
	}
	ref := agyPromptFile
	if s.Cwd == "" {
		ref = filepath.ToSlash(path)
	}
	return "Your instructions for this task are in the file " + ref +
		", relative to your current working directory. Read that file in full before you do " +
		"anything else, then carry it out exactly as written. It is the complete task and the " +
		"only instruction you have been given; this message is a pointer to it and nothing more.", nil
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
	var onLine func(string)
	if s.OnProgress != nil {
		onLine = func(line string) {
			if c := CondenseLine(line); c != "" {
				s.OnProgress(c)
			}
		}
	}
	// The log is written by RunCapture as output arrives, not here after the
	// fact: an operator watching a 20-minute state needs the file to exist and
	// grow while the agent is still running.
	res, out, err := execx.RunCapture(ctx, execx.Cmd{
		Argv:    argv,
		Dir:     s.Cwd,
		Timeout: timeout,
		Stdin:   stdin,
		LogPath: s.LogPath,
		OnLine:  onLine,
	}, 8<<20)
	if err != nil {
		return Result{ExitCode: -1, Stdout: out}, err
	}
	result := Result{ExitCode: res.ExitCode, Stdout: out, TimedOut: res.TimedOut}
	parseEnvelope(out, &result)
	result.QuotaHit = matchesAny(out, s.Backend.QuotaErrorPatterns)
	classifyTransport(&result, s.Backend.TransportErrorPatterns)
	return result, nil
}

// classifyTransport decides whether the backend could not be reached.
//
// The text it searches matters more than the patterns. Matching the whole
// transcript is wrong: an agent editing this very repository writes about
// ECONNREFUSED and socket timeouts in the ordinary course of its work, and
// suspending a job because the agent used the word would be worse than the bug
// this classification exists to fix. So:
//
//   - terminal_reason is authoritative when present — the CLI is stating why it
//     stopped, and nothing the agent said can be mistaken for that.
//   - with an envelope but no terminal_reason (agy, and older claude builds),
//     only the CLI's own `result` message is searched, never the transcript.
//   - with no envelope at all the CLI died before it could explain itself, so
//     the raw output is all there is and the patterns run over it.
//
// A run that succeeded is never unreachable, whatever it printed on the way.
func classifyTransport(res *Result, patterns []string) {
	defer func() {
		// A rate limit often surfaces as a transport-shaped error too. Quota
		// wins: it has the longer, deliberately-tuned backoff, and treating a
		// rate limit as a network blip would retry it far too eagerly.
		if res.QuotaHit {
			res.Unreachable = false
		}
	}()
	if res.ExitCode == 0 && !res.TimedOut {
		res.Unreachable = false
		return
	}
	// terminal_reason is the CLI naming why it stopped, and it settles the
	// question both ways: "api_error" means unreachable, and anything else —
	// max_turns, refusal — means the CLI got an answer and the run failed for
	// its own reasons, however much the agent's message talks about sockets.
	if res.terminalReason != "" {
		res.Unreachable = strings.EqualFold(res.terminalReason, "api_error")
		return
	}
	if res.envelopeFound {
		res.Unreachable = res.resultText != "" && matchesAny(res.resultText, patterns)
		return
	}
	res.Unreachable = matchesAny(res.Stdout, patterns)
}

// UnreachableDetail pulls the backend's own words out of a failed run, for the
// hold reason and the event log. Without it the operator is told the state's
// post-condition failed — "spec.json is missing" — which is the symptom, and
// sends them to read the prompt instead of their network.
//
// The CLI's `result` field is preferred because it is the sentence the CLI
// chose to explain itself with; the first transport-shaped line is the fallback
// for output with no envelope, which is what a CLI that died mid-request emits.
func UnreachableDetail(out string) string {
	const fallback = "backend could not be reached"
	if start := strings.Index(out, "{"); start >= 0 {
		var m map[string]any
		if json.Unmarshal([]byte(out[start:]), &m) == nil {
			if r, ok := m["result"].(string); ok && strings.TrimSpace(r) != "" {
				return flattenLine(r)
			}
		}
	}
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" && matchesAny(ln, defaultDetailHints) {
			return flattenLine(ln)
		}
	}
	return fallback
}

// defaultDetailHints are only used to pick a line to quote, never to decide
// whether the run failed — that decision is the backend's configured patterns
// and the CLI's own terminal_reason.
var defaultDetailHints = []string{
	`(?i)\b(ENOTFOUND|ECONNREFUSED|ECONNRESET|ETIMEDOUT|EAI_AGAIN|EHOSTUNREACH|ENETUNREACH)\b`,
	`(?i)(API Error|network|getaddrinfo|socket hang ?up|timed out)`,
}

// flattenLine collapses a multi-line CLI message into the single bounded line
// a hold reason and a console notice have room for. The rune-safe truncation
// is stream.go's, reused rather than reimplemented — a message with a path or
// any non-ASCII in it must not be cut mid-character.
func flattenLine(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
	return truncateLine(s)
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
	res.envelopeFound = true
	if r, ok := m["result"].(string); ok {
		res.resultText = r
	}
	// agy and exec backends name the failure differently; all of them are the
	// CLI speaking about itself rather than the agent's transcript.
	if res.resultText == "" {
		for _, key := range []string{"error", "message", "detail"} {
			if v, ok := m[key].(string); ok && v != "" {
				res.resultText = v
				break
			}
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
	// The claude CLI names why it stopped. "api_error" means it never got an
	// answer from the server, which is a different thing from the model
	// answering badly, and is the one case where retrying the job is pointless
	// until the network comes back. Reading the field beats pattern-matching
	// the prose around it, which changes with the CLI's wording.
	if tr, ok := m["terminal_reason"].(string); ok && strings.TrimSpace(tr) != "" {
		res.terminalReason = tr
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
